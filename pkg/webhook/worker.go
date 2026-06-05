package webhook

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"telegram-sms-bot/pkg/storage"
	"time"
)

type CircuitBreaker struct {
	State               string    // CLOSED, OPEN, HALF-OPEN
	ConsecutiveFailures int
	LastFailureTime     time.Time
}

type Worker struct {
	store            storage.Store
	client           *http.Client
	breakers         map[string]*CircuitBreaker
	breakersMutex    sync.Mutex
	stopChan         chan struct{}
	wg               sync.WaitGroup
	cooldownDuration time.Duration
	tickerDuration   time.Duration
}

// NewWorker creates a webhook worker.
func NewWorker(store storage.Store) *Worker {
	return &Worker{
		store:            store,
		client:           &http.Client{Timeout: 5 * time.Second},
		breakers:         make(map[string]*CircuitBreaker),
		stopChan:         make(chan struct{}),
		cooldownDuration: 15 * time.Second, // 15s cooldown
		tickerDuration:   1 * time.Second,  // scan queue every 1s
	}
}

// Start launches the background polling worker.
func (w *Worker) Start() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(w.tickerDuration)
		defer ticker.Stop()

		for {
			select {
			case <-w.stopChan:
				return
			case <-ticker.C:
				w.processQueue()
			}
		}
	}()
}

// Stop halts the background worker.
func (w *Worker) Stop() {
	close(w.stopChan)
	w.wg.Wait()
}

// getBreakerState resolves or updates the circuit breaker state for a host.
func (w *Worker) getBreakerState(host string) string {
	w.breakersMutex.Lock()
	defer w.breakersMutex.Unlock()

	cb, exists := w.breakers[host]
	if !exists {
		cb = &CircuitBreaker{
			State:               "CLOSED",
			ConsecutiveFailures: 0,
		}
		w.breakers[host] = cb
		return "CLOSED"
	}

	if cb.State == "OPEN" {
		if time.Since(cb.LastFailureTime) >= w.cooldownDuration {
			cb.State = "HALF-OPEN"
			return "HALF-OPEN"
		}
	}

	return cb.State
}

// recordFailure records a failed communication for a host, potentially tripping the breaker.
func (w *Worker) recordFailure(host string) {
	w.breakersMutex.Lock()
	defer w.breakersMutex.Unlock()

	cb := w.breakers[host]
	if cb == nil {
		return
	}

	cb.ConsecutiveFailures++
	cb.LastFailureTime = time.Now()

	if cb.State == "CLOSED" && cb.ConsecutiveFailures >= 5 {
		cb.State = "OPEN"
	} else if cb.State == "HALF-OPEN" {
		cb.State = "OPEN"
	}
}

// recordSuccess records a successful communication, closing the breaker.
func (w *Worker) recordSuccess(host string) {
	w.breakersMutex.Lock()
	defer w.breakersMutex.Unlock()

	cb := w.breakers[host]
	if cb == nil {
		return
	}

	cb.State = "CLOSED"
	cb.ConsecutiveFailures = 0
}

// processQueue fetches pending webhooks, verifies host circuit breaker state, and fires them.
func (w *Worker) processQueue() {
	jobs := w.store.GetPendingWebhooks()
	if len(jobs) == 0 {
		return
	}

	for _, job := range jobs {
		u, err := url.Parse(job.CallbackURL)
		var host string
		if err != nil {
			host = "unknown"
		} else {
			host = u.Host
		}

		state := w.getBreakerState(host)
		if state == "OPEN" {
			// Skip processing jobs for this host while the circuit breaker is open.
			continue
		}

		// Fire webhook delivery in goroutine to remain non-blocking.
		w.wg.Add(1)
		go func(j storage.WebhookJob, h string) {
			defer w.wg.Done()
			w.deliverWebhook(j, j.Attempts+1, h)
		}(*job, host)
	}
}

// deliverWebhook sends the POST request to the client endpoint and updates status.
func (w *Worker) deliverWebhook(job storage.WebhookJob, attempts int, host string) {
	req, err := http.NewRequest("POST", job.CallbackURL, bytes.NewReader(job.Payload))
	if err != nil {
		_ = w.store.UpdateWebhookStatus(job.ID, "FAILED", attempts, fmt.Sprintf("invalid request: %v", err), time.Now().Add(5*time.Minute))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Telegram-Verification-Gateway/1.0")

	resp, err := w.client.Do(req)
	if err != nil {
		w.recordFailure(host)
		w.scheduleRetry(job.ID, attempts, fmt.Sprintf("network error: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		w.recordFailure(host)
		w.scheduleRetry(job.ID, attempts, fmt.Sprintf("HTTP status %d: %s", resp.StatusCode, string(body)))
		return
	}

	// Success!
	w.recordSuccess(host)
	_ = w.store.UpdateWebhookStatus(job.ID, "SUCCESS", attempts, "", time.Time{})
}

// scheduleRetry calculates exponential backoff and updates the job's retry timestamp.
func (w *Worker) scheduleRetry(id string, attempts int, lastErr string) {
	// Exponential backoff: 2^attempts seconds (e.g. 2s, 4s, 8s, 16s, 32s...)
	backoff := time.Duration(1<<uint(attempts)) * time.Second
	if backoff > 1*time.Hour {
		backoff = 1 * time.Hour
	}

	nextRetry := time.Now().Add(backoff)
	_ = w.store.UpdateWebhookStatus(id, "FAILED", attempts, lastErr, nextRetry)
}
