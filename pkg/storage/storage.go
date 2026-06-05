package storage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Client struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	APIKey    string    `json:"api_key"`
	CreatedAt time.Time `json:"created_at"`
}

type Session struct {
	Token          string    `json:"token"`
	ClientID       string    `json:"client_id"`
	CallbackURL    string    `json:"callback_url"`
	UserReference  string    `json:"user_reference"`
	Status         string    `json:"status"` // PENDING, VERIFIED, EXPIRED
	ChatID         int64     `json:"chat_id,omitempty"`
	TelegramUser   string    `json:"telegram_user,omitempty"`
	TelegramFirst  string    `json:"telegram_first,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type WebhookJob struct {
	ID            string    `json:"id"`
	SessionToken  string    `json:"session_token"`
	CallbackURL   string    `json:"callback_url"`
	Payload       []byte    `json:"payload"`
	Status        string    `json:"status"` // PENDING, SUCCESS, FAILED
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
	NextRetryTime time.Time `json:"next_retry_time"`
	CreatedAt     time.Time `json:"created_at"`
}

type StoreData struct {
	Clients      map[string]Client     `json:"clients"`       // Key: APIKey
	Sessions     map[string]Session    `json:"sessions"`      // Key: Token
	WebhookQueue map[string]*WebhookJob `json:"webhook_queue"`  // Key: ID
}

type Store interface {
	RegisterClient(name string) (Client, error)
	GetClientByAPIKey(apiKey string) (Client, bool)
	GetClients() []Client
	CreateSession(clientID, callbackURL, userRef string, expiresAfter time.Duration) (Session, error)
	GetSessionByToken(token string) (Session, bool)
	VerifySession(token string, chatID int64, username, firstName string) (Session, error)
	EnqueueWebhook(sessionToken, callbackURL string, payload interface{}) (WebhookJob, error)
	GetPendingWebhooks() []*WebhookJob
	UpdateWebhookStatus(id string, status string, attempts int, lastErr string, nextRetry time.Time) error
	CleanupSessions() int
}

type JSONStore struct {
	filePath string
	data     StoreData
	mutex    sync.RWMutex
}

func generateRandomHex(n int) (string, error) {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// NewJSONStore initializes and loads the JSON store.
func NewJSONStore(filePath string) (*JSONStore, error) {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	s := &JSONStore{
		filePath: filePath,
		data: StoreData{
			Clients:      make(map[string]Client),
			Sessions:     make(map[string]Session),
			WebhookQueue: make(map[string]*WebhookJob),
		},
	}

	err := s.load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to load storage file: %w", err)
	}

	return s, nil
}

// load reads the JSON file into memory.
func (s *JSONStore) load() error {
	file, err := os.Open(s.filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}

	if len(data) == 0 {
		return nil
	}

	var storeData StoreData
	if err := json.Unmarshal(data, &storeData); err != nil {
		return err
	}

	if storeData.Clients == nil {
		storeData.Clients = make(map[string]Client)
	}
	if storeData.Sessions == nil {
		storeData.Sessions = make(map[string]Session)
	}
	if storeData.WebhookQueue == nil {
		storeData.WebhookQueue = make(map[string]*WebhookJob)
	}

	s.data = storeData
	return nil
}

// saveLocked writes memory state to disk. Must be called with a write lock held.
func (s *JSONStore) saveLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := s.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}

	if err := os.Remove(s.filePath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return os.Rename(tmpFile, s.filePath)
}

// RegisterClient creates a new client and API Key.
func (s *JSONStore) RegisterClient(name string) (Client, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

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

	s.data.Clients[client.APIKey] = client
	if err := s.saveLocked(); err != nil {
		return Client{}, err
	}

	return client, nil
}

// GetClientByAPIKey retrieves a client by API Key.
func (s *JSONStore) GetClientByAPIKey(apiKey string) (Client, bool) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	client, exists := s.data.Clients[apiKey]
	return client, exists
}

// GetClients returns a list of all registered clients.
func (s *JSONStore) GetClients() []Client {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	list := make([]Client, 0, len(s.data.Clients))
	for _, client := range s.data.Clients {
		list = append(list, client)
	}
	return list
}

// CreateSession creates a verification session.
func (s *JSONStore) CreateSession(clientID, callbackURL, userRef string, expiresAfter time.Duration) (Session, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

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

	s.data.Sessions[session.Token] = session
	if err := s.saveLocked(); err != nil {
		return Session{}, err
	}

	return session, nil
}

// GetSessionByToken retrieves a session by verification token.
func (s *JSONStore) GetSessionByToken(token string) (Session, bool) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	session, exists := s.data.Sessions[token]
	return session, exists
}

// VerifySession marks a session as verified and records Telegram user info.
func (s *JSONStore) VerifySession(token string, chatID int64, username, firstName string) (Session, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	session, exists := s.data.Sessions[token]
	if !exists {
		return Session{}, errors.New("session not found")
	}

	if session.Status != "PENDING" {
		return Session{}, fmt.Errorf("session is already in %s status", session.Status)
	}

	if time.Now().After(session.ExpiresAt) {
		session.Status = "EXPIRED"
		s.data.Sessions[token] = session
		_ = s.saveLocked()
		return Session{}, errors.New("session has expired")
	}

	session.Status = "VERIFIED"
	session.ChatID = chatID
	session.TelegramUser = username
	session.TelegramFirst = firstName

	s.data.Sessions[token] = session
	if err := s.saveLocked(); err != nil {
		return Session{}, err
	}

	return session, nil
}

// EnqueueWebhook adds a webhook payload delivery task.
func (s *JSONStore) EnqueueWebhook(sessionToken, callbackURL string, payload interface{}) (WebhookJob, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	jobIDHex, err := generateRandomHex(8)
	if err != nil {
		return WebhookJob{}, err
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return WebhookJob{}, err
	}

	job := &WebhookJob{
		ID:            "web_" + jobIDHex,
		SessionToken:  sessionToken,
		CallbackURL:   callbackURL,
		Payload:       payloadBytes,
		Status:        "PENDING",
		Attempts:      0,
		NextRetryTime: time.Now(),
		CreatedAt:     time.Now(),
	}

	s.data.WebhookQueue[job.ID] = job
	if err := s.saveLocked(); err != nil {
		return WebhookJob{}, err
	}

	return *job, nil
}

// GetPendingWebhooks retrieves all webhooks that are ready to run or retry.
func (s *JSONStore) GetPendingWebhooks() []*WebhookJob {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var pending []*WebhookJob
	now := time.Now()
	for _, job := range s.data.WebhookQueue {
		if (job.Status == "PENDING" || job.Status == "FAILED") && now.After(job.NextRetryTime) {
			jobCopy := *job
			pending = append(pending, &jobCopy)
		}
	}
	return pending
}

// UpdateWebhookStatus updates the processing details of a webhook delivery.
func (s *JSONStore) UpdateWebhookStatus(id string, status string, attempts int, lastErr string, nextRetry time.Time) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	job, exists := s.data.WebhookQueue[id]
	if !exists {
		return errors.New("webhook job not found")
	}

	job.Status = status
	job.Attempts = attempts
	job.LastError = lastErr
	job.NextRetryTime = nextRetry

	if err := s.saveLocked(); err != nil {
		return err
	}

	return nil
}

// CleanupSessions evicts expired PENDING sessions.
func (s *JSONStore) CleanupSessions() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	count := 0
	now := time.Now()
	for k, session := range s.data.Sessions {
		if session.Status == "PENDING" && now.After(session.ExpiresAt) {
			session.Status = "EXPIRED"
			s.data.Sessions[k] = session
			count++
		}
	}

	if count > 0 {
		_ = s.saveLocked()
	}
	return count
}
