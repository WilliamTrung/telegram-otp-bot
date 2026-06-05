package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisStore struct {
	client *redis.Client
}

// NewRedisStore initializes the Redis client and pings the database.
func NewRedisStore(redisURL string) (*RedisStore, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis url: %w", err)
	}

	client := redis.NewClient(opts)

	// Ping the server to ensure connection is valid
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to ping redis: %w", err)
	}

	return &RedisStore{client: client}, nil
}

// Close closes the underlying Redis client connection.
func (r *RedisStore) Close() error {
	return r.client.Close()
}

// RegisterClient creates a new client and API Key in Redis.
func (r *RedisStore) RegisterClient(name string) (Client, error) {
	clientIDHex, err := generateRandomHex(8)
	if err != nil {
		return Client{}, err
	}
	apiKeyHex, err := generateRandomHex(16)
	if err != nil {
		return Client{}, err
	}

	client := Client{
		ID:        "cli_" + clientIDHex,
		Name:      name,
		APIKey:    "vkey_" + apiKeyHex,
		CreatedAt: time.Now(),
	}

	ctx := context.Background()
	clientData, err := json.Marshal(client)
	if err != nil {
		return Client{}, err
	}

	pipe := r.client.TxPipeline()
	pipe.SAdd(ctx, "clients:all", client.APIKey)
	pipe.Set(ctx, "client:by_api_key:"+client.APIKey, clientData, 0)
	
	_, err = pipe.Exec(ctx)
	if err != nil {
		return Client{}, fmt.Errorf("failed to save client to redis: %w", err)
	}

	return client, nil
}

// GetClientByAPIKey retrieves a client by API Key.
func (r *RedisStore) GetClientByAPIKey(apiKey string) (Client, bool) {
	ctx := context.Background()
	val, err := r.client.Get(ctx, "client:by_api_key:"+apiKey).Result()
	if errors.Is(err, redis.Nil) {
		return Client{}, false
	} else if err != nil {
		return Client{}, false
	}

	var client Client
	if err := json.Unmarshal([]byte(val), &client); err != nil {
		return Client{}, false
	}

	return client, true
}

// GetClients returns a list of all registered clients.
func (r *RedisStore) GetClients() []Client {
	ctx := context.Background()
	apiKeys, err := r.client.SMembers(ctx, "clients:all").Result()
	if err != nil {
		return nil
	}

	var list []Client
	for _, apiKey := range apiKeys {
		val, err := r.client.Get(ctx, "client:by_api_key:"+apiKey).Result()
		if err == nil {
			var client Client
			if err := json.Unmarshal([]byte(val), &client); err == nil {
				list = append(list, client)
			}
		}
	}
	return list
}

// CreateSession creates a verification session in Redis with a TTL.
func (r *RedisStore) CreateSession(clientID, callbackURL, userRef string, expiresAfter time.Duration) (Session, error) {
	tokenHex, err := generateRandomHex(16)
	if err != nil {
		return Session{}, err
	}

	now := time.Now()
	session := Session{
		Token:         "auth_" + tokenHex,
		ClientID:      clientID,
		CallbackURL:   callbackURL,
		UserReference: userRef,
		Status:        "PENDING",
		CreatedAt:     now,
		ExpiresAt:     now.Add(expiresAfter),
	}

	ctx := context.Background()
	sessionData, err := json.Marshal(session)
	if err != nil {
		return Session{}, err
	}

	// Save to Redis setting the TTL to match the session lifetime
	err = r.client.Set(ctx, "session:by_token:"+session.Token, sessionData, expiresAfter).Err()
	if err != nil {
		return Session{}, fmt.Errorf("failed to save session to redis: %w", err)
	}

	return session, nil
}

// GetSessionByToken retrieves a session by verification token.
func (r *RedisStore) GetSessionByToken(token string) (Session, bool) {
	ctx := context.Background()
	val, err := r.client.Get(ctx, "session:by_token:"+token).Result()
	if errors.Is(err, redis.Nil) {
		return Session{}, false
	} else if err != nil {
		return Session{}, false
	}

	var session Session
	if err := json.Unmarshal([]byte(val), &session); err != nil {
		return Session{}, false
	}

	return session, true
}

// VerifySession marks a session as verified.
func (r *RedisStore) VerifySession(token string, chatID int64, username, firstName string) (Session, error) {
	ctx := context.Background()
	key := "session:by_token:" + token
	
	val, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return Session{}, errors.New("session not found or expired")
	} else if err != nil {
		return Session{}, err
	}

	var session Session
	if err := json.Unmarshal([]byte(val), &session); err != nil {
		return Session{}, err
	}

	if session.Status != "PENDING" {
		return Session{}, fmt.Errorf("session is already in %s status", session.Status)
	}

	session.Status = "VERIFIED"
	session.ChatID = chatID
	session.TelegramUser = username
	session.TelegramFirst = firstName

	sessionData, err := json.Marshal(session)
	if err != nil {
		return Session{}, err
	}

	// Keep verified sessions stored in Redis for 24 hours for lookup history
	err = r.client.Set(ctx, key, sessionData, 24*time.Hour).Err()
	if err != nil {
		return Session{}, err
	}

	return session, nil
}

// EnqueueWebhook adds a webhook task to Redis.
func (r *RedisStore) EnqueueWebhook(sessionToken, callbackURL string, payload interface{}) (WebhookJob, error) {
	jobIDHex, err := generateRandomHex(8)
	if err != nil {
		return WebhookJob{}, err
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return WebhookJob{}, err
	}

	job := WebhookJob{
		ID:            "web_" + jobIDHex,
		SessionToken:  sessionToken,
		CallbackURL:   callbackURL,
		Payload:       payloadBytes,
		Status:        "PENDING",
		Attempts:      0,
		NextRetryTime: time.Now(),
		CreatedAt:     time.Now(),
	}

	ctx := context.Background()
	jobData, err := json.Marshal(job)
	if err != nil {
		return WebhookJob{}, err
	}

	pipe := r.client.TxPipeline()
	// Keep webhook job logs in Redis for 7 days
	pipe.Set(ctx, "webhook:job:"+job.ID, jobData, 7*24*time.Hour)
	pipe.ZAdd(ctx, "webhook:pending", redis.Z{
		Score:  float64(job.NextRetryTime.Unix()),
		Member: job.ID,
	})

	_, err = pipe.Exec(ctx)
	if err != nil {
		return WebhookJob{}, err
	}

	return job, nil
}

// GetPendingWebhooks retrieves all webhook jobs whose NextRetryTime is in the past.
func (r *RedisStore) GetPendingWebhooks() []*WebhookJob {
	ctx := context.Background()
	now := time.Now().Unix()

	// Find all job IDs scheduled to run before or at this moment
	jobIDs, err := r.client.ZRangeByScore(ctx, "webhook:pending", &redis.ZRangeBy{
		Min: "-inf",
		Max: fmt.Sprintf("%d", now),
	}).Result()

	if err != nil || len(jobIDs) == 0 {
		return nil
	}

	var pending []*WebhookJob
	for _, id := range jobIDs {
		val, err := r.client.Get(ctx, "webhook:job:"+id).Result()
		if errors.Is(err, redis.Nil) {
			// Job was deleted/expired from string store, clean up from queue
			r.client.ZRem(ctx, "webhook:pending", id)
			continue
		} else if err != nil {
			continue
		}

		var job WebhookJob
		if err := json.Unmarshal([]byte(val), &job); err == nil {
			pending = append(pending, &job)
		}
	}

	return pending
}

// UpdateWebhookStatus updates a webhook job status.
func (r *RedisStore) UpdateWebhookStatus(id string, status string, attempts int, lastErr string, nextRetry time.Time) error {
	ctx := context.Background()
	key := "webhook:job:" + id

	val, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return errors.New("webhook job not found")
	} else if err != nil {
		return err
	}

	var job WebhookJob
	if err := json.Unmarshal([]byte(val), &job); err != nil {
		return err
	}

	job.Status = status
	job.Attempts = attempts
	job.LastError = lastErr
	job.NextRetryTime = nextRetry

	jobData, err := json.Marshal(job)
	if err != nil {
		return err
	}

	pipe := r.client.TxPipeline()
	pipe.Set(ctx, key, jobData, 7*24*time.Hour) // Keep for 7 days

	if status == "SUCCESS" {
		pipe.ZRem(ctx, "webhook:pending", id)
	} else {
		// Update retry time in Sorted Set
		pipe.ZAdd(ctx, "webhook:pending", redis.Z{
			Score:  float64(nextRetry.Unix()),
			Member: id,
		})
	}

	_, err = pipe.Exec(ctx)
	return err
}

// CleanupSessions is a no-op because Redis automatically evicts expired sessions via TTL.
func (r *RedisStore) CleanupSessions() int {
	return 0
}
