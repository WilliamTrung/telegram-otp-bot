package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"telegram-sms-bot/pkg/storage"
	"testing"
	"time"
)

func TestWebhookWorkerAndCircuitBreaker(t *testing.T) {
	// Create temporary storage path.
	tempDir, err := os.MkdirTemp("", "webhook_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	dbPath := filepath.Join(tempDir, "store.json")

	store, err := storage.NewJSONStore(dbPath)
	if err != nil {
		t.Fatalf("failed to init storage: %v", err)
	}

	// 1. Setup mock HTTP server for callbacks
	var requestsCount int32
	var responseStatus int32 = http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestsCount, 1)

		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("failed to parse payload in webhook: %v", err)
		}

		currentStatus := atomic.LoadInt32(&responseStatus)
		if currentStatus != http.StatusOK {
			w.WriteHeader(int(currentStatus))
			w.Write([]byte("mock error response"))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	// 2. Initialize worker
	w := NewWorker(store)
	// Speed up execution for tests
	w.cooldownDuration = 300 * time.Millisecond // 300ms cooldown for test
	w.tickerDuration = 10 * time.Millisecond    // tick every 10ms
	w.client = server.Client()

	w.Start()
	defer w.Stop()

	// Create verification session and enqueue webhook
	session, err := store.CreateSession("cli_test", server.URL, "user-1", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	payload := map[string]interface{}{"status": "VERIFIED"}
	_, err = store.EnqueueWebhook(session.Token, server.URL, payload)
	if err != nil {
		t.Fatalf("failed to enqueue webhook: %v", err)
	}

	// Wait for successful delivery
	time.Sleep(100 * time.Millisecond)

	// Verify webhook succeeded
	if atomic.LoadInt32(&requestsCount) != 1 {
		t.Errorf("expected 1 request, got %d", atomic.LoadInt32(&requestsCount))
	}

	// Fetch updated job from store
	store2, _ := storage.NewJSONStore(dbPath)
	pending := store2.GetPendingWebhooks()
	if len(pending) != 0 {
		t.Errorf("expected no pending webhooks, found %d", len(pending))
	}

	// 3. Test Failure & Circuit Breaker Tripping
	atomic.StoreInt32(&responseStatus, http.StatusInternalServerError)
	atomic.StoreInt32(&requestsCount, 0)

	// Create new session & webhook to trip the breaker
	u, _ := url.Parse(server.URL)
	host := u.Host

	// We need 5 consecutive failures to trip the breaker
	for i := 0; i < 5; i++ {
		sess, _ := store.CreateSession("cli_test", server.URL, fmt.Sprintf("user-fail-%d", i), 5*time.Minute)
		_, err = store.EnqueueWebhook(sess.Token, server.URL, payload)
		if err != nil {
			t.Fatalf("failed to enqueue: %v", err)
		}
		// Give worker time to process this single failure
		time.Sleep(20 * time.Millisecond)
	}

	// The circuit breaker for this host should be OPEN
	state := w.getBreakerState(host)
	if state != "OPEN" {
		t.Errorf("expected circuit breaker state to be OPEN, got %s", state)
	}

	// Enqueue a new webhook during OPEN state
	sess, _ := store.CreateSession("cli_test", server.URL, "user-during-open", 5*time.Minute)
	_, err = store.EnqueueWebhook(sess.Token, server.URL, payload)
	if err != nil {
		t.Fatalf("failed to enqueue: %v", err)
	}

	// Wait and ensure it is NOT processed (request count should not increase)
	initialRequests := atomic.LoadInt32(&requestsCount)
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&requestsCount) != initialRequests {
		t.Errorf("webhook processed while breaker was OPEN: request count changed")
	}

	// 4. Test Recovery (HALF-OPEN -> CLOSED)
	// Make server return 200 OK again
	atomic.StoreInt32(&responseStatus, http.StatusOK)
	// Wait for cooldown duration (300ms) and worker ticks to pass
	time.Sleep(350 * time.Millisecond)

	// Breaker should close automatically since the pending request succeeded
	state = w.getBreakerState(host)
	if state != "CLOSED" {
		t.Errorf("expected circuit breaker state to be CLOSED after recovery, got %s", state)
	}

	// Double check that there are no pending webhooks left
	pending = store.GetPendingWebhooks()
	if len(pending) != 0 {
		t.Errorf("expected 0 pending webhooks after recovery, got %d", len(pending))
	}
}
