package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	// Dynamically load the project's root .env file if it exists.
	// Since tests execute inside their respective package directories, we look up 2 levels.
	data, err := os.ReadFile("../../.env")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				val := strings.TrimSpace(parts[1])
				// Strip any wrapping quotes if present
				val = strings.Trim(val, `"'`)
				os.Setenv(key, val)
			}
		}
	}
}

func TestStorage(t *testing.T) {
	// 1. Verify JSON Store
	t.Run("JSONStore", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "storage_test")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tempDir)

		dbPath := filepath.Join(tempDir, "store.json")
		s, err := NewJSONStore(dbPath)
		if err != nil {
			t.Fatalf("failed to init JSON storage: %v", err)
		}

		runStoreTestSuite(t, s)

		// Test JSON-specific serialization/reload behavior
		s2, err := NewJSONStore(dbPath)
		if err != nil {
			t.Fatalf("failed to reload JSON storage: %v", err)
		}
		clients := s2.GetClients()
		if len(clients) == 0 {
			t.Errorf("expected clients to persist after JSON reload")
		}
	})

	// 2. Verify Redis Store (Optional, skipped if Redis is offline)
	t.Run("RedisStore", func(t *testing.T) {
		redisURL := os.Getenv("REDIS_URL")
		if redisURL == "" {
			redisURL = "redis://localhost:6379/0"
		}
		
		s, err := NewRedisStore(redisURL)
		if err != nil {
			t.Logf("Skipping Redis store tests: Redis server not available at %s (%v)", redisURL, err)
			return
		}
		defer s.Close()

		// Flush test database before running tests
		ctx := context.Background()
		s.client.FlushDB(ctx)

		runStoreTestSuite(t, s)
	})
}

// runStoreTestSuite executes unified functional tests against any Store interface implementation.
func runStoreTestSuite(t *testing.T, s Store) {
	// 1. Client Registration & Retrieval
	clientName := "Test App"
	client, err := s.RegisterClient(clientName)
	if err != nil {
		t.Fatalf("failed to register client: %v", err)
	}
	if client.Name != clientName {
		t.Errorf("expected client name %s, got %s", clientName, client.Name)
	}
	if client.APIKey == "" || client.ID == "" {
		t.Errorf("client API key or ID is empty")
	}

	retrievedClient, found := s.GetClientByAPIKey(client.APIKey)
	if !found {
		t.Fatalf("client not found by API key")
	}
	if retrievedClient.ID != client.ID {
		t.Errorf("retrieved client ID mismatch: expected %s, got %s", client.ID, retrievedClient.ID)
	}

	clients := s.GetClients()
	if len(clients) != 1 {
		t.Errorf("expected 1 client in database, got %d", len(clients))
	}

	// 2. Session Creation, Retrieval, and Verification
	callbackURL := "http://example.com/webhook"
	userRef := "user-123"
	session, err := s.CreateSession(client.ID, callbackURL, userRef, 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	if session.Status != "PENDING" {
		t.Errorf("expected session status PENDING, got %s", session.Status)
	}

	retrievedSession, found := s.GetSessionByToken(session.Token)
	if !found {
		t.Fatalf("session not found by token")
	}
	if retrievedSession.UserReference != userRef {
		t.Errorf("session user reference mismatch: expected %s, got %s", userRef, retrievedSession.UserReference)
	}

	verifiedSession, err := s.VerifySession(session.Token, 123456, "john_doe", "John")
	if err != nil {
		t.Fatalf("failed to verify session: %v", err)
	}
	if verifiedSession.Status != "VERIFIED" {
		t.Errorf("expected session status VERIFIED, got %s", verifiedSession.Status)
	}
	if verifiedSession.ChatID != 123456 || verifiedSession.TelegramUser != "john_doe" {
		t.Errorf("session user info mismatch")
	}

	// 3. Webhook Queue Handling
	payload := map[string]interface{}{"status": "ok"}
	job, err := s.EnqueueWebhook(session.Token, callbackURL, payload)
	if err != nil {
		t.Fatalf("failed to enqueue webhook: %v", err)
	}

	pending := s.GetPendingWebhooks()
	if len(pending) != 1 {
		t.Errorf("expected 1 pending webhook, got %d", len(pending))
	}
	if pending[0].ID != job.ID {
		t.Errorf("pending job ID mismatch")
	}

	// Update Webhook retry scheduling
	nextRetry := time.Now().Add(10 * time.Minute)
	err = s.UpdateWebhookStatus(job.ID, "FAILED", 1, "network error", nextRetry)
	if err != nil {
		t.Fatalf("failed to update webhook status: %v", err)
	}

	// Since nextRetry is scheduled in the future, it should not be returned by GetPendingWebhooks
	pending = s.GetPendingWebhooks()
	if len(pending) != 0 {
		t.Errorf("expected 0 pending webhooks (due to future retry time), got %d", len(pending))
	}

	// Update Webhook success status (should evict from queue)
	err = s.UpdateWebhookStatus(job.ID, "SUCCESS", 2, "", time.Time{})
	if err != nil {
		t.Fatalf("failed to mark webhook success: %v", err)
	}
}
