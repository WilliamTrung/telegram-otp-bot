package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"telegram-sms-bot/pkg/storage"
	"telegram-sms-bot/pkg/webhook"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Go Embed to bundle the static web folder into the binary
//go:embed web/*
var embedWeb embed.FS

type App struct {
	store   storage.Store
	worker  *webhook.Worker
	bot     *tgbotapi.BotAPI
	port    string
	webFS   fs.FS
}

// WebhookPayload sent to client services
type WebhookPayload struct {
	Event         string            `json:"event"`
	Token         string            `json:"token"`
	UserReference string            `json:"user_reference"`
	Status        string            `json:"status"`
	Telegram      TelegramUserInfo  `json:"telegram"`
	Timestamp     string            `json:"timestamp"`
}

type TelegramUserInfo struct {
	ChatID    int64  `json:"chat_id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type DailyLogger struct {
	mu         sync.Mutex
	logDir     string
	filePrefix string
	fileExt    string
	currentDay string
	file       *os.File
	out        io.Writer
}

func (l *DailyLogger) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	day := time.Now().Format("2006-01-02")
	if day != l.currentDay || l.file == nil {
		if l.file != nil {
			l.file.Close()
		}
		if err := os.MkdirAll(l.logDir, 0755); err != nil {
			return 0, err
		}
		filename := filepath.Join(l.logDir, fmt.Sprintf("%s-%s%s", l.filePrefix, day, l.fileExt))
		f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return 0, err
		}
		l.file = f
		l.currentDay = day
	}

	n, err = l.file.Write(p)
	if err != nil {
		return n, err
	}
	if l.out != nil {
		_, _ = l.out.Write(p)
	}
	return n, nil
}

func main() {
	// Set up logging to stdout and optionally a log file
	logFile := os.Getenv("LOG_FILE")
	if logFile != "" {
		dir := filepath.Dir(logFile)
		base := filepath.Base(logFile)
		ext := filepath.Ext(base)
		prefix := strings.TrimSuffix(base, ext)

		logger := &DailyLogger{
			logDir:     dir,
			filePrefix: prefix,
			fileExt:    ext,
			out:        os.Stdout,
		}
		log.SetOutput(logger)
	}

	log.Println("Starting Telegram Verification Gateway...")

	// 1. Read Environment Variables
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dataFile := os.Getenv("DATA_FILE")
	if dataFile == "" {
		dataFile = "data/store.json"
	}

	// 2. Initialize Storage Engine (Redis with JSON fallback)
	var store storage.Store
	var err error
	redisURL := os.Getenv("REDIS_URL")

	if redisURL != "" {
		var rErr error
		store, rErr = storage.NewRedisStore(redisURL)
		if rErr != nil {
			log.Printf("WARNING: Redis connection failed: %v. Falling back to local file storage.", rErr)
			store, err = storage.NewJSONStore(dataFile)
		} else {
			log.Println("Successfully connected to Redis. Using Redis storage engine.")
		}
	} else {
		log.Println("REDIS_URL not configured. Using local JSON file storage fallback.")
		store, err = storage.NewJSONStore(dataFile)
	}

	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}

	// 3. Initialize Webhook Worker
	worker := webhook.NewWorker(store)
	worker.Start()
	defer worker.Stop()

	// 4. Initialize Telegram Bot
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("Failed to initialize Telegram Bot: %v", err)
	}
	log.Printf("Authorized on bot account: @%s", bot.Self.UserName)

	// Sub-filesystem for embedded web assets
	webSub, err := fs.Sub(embedWeb, "web")
	if err != nil {
		log.Fatalf("Failed to get web sub-filesystem: %v", err)
	}

	app := &App{
		store:  store,
		worker: worker,
		bot:    bot,
		port:   port,
		webFS:  webSub,
	}

	// 5. Start Telegram Polling in background
	go app.runTelegramBot()

	// 6. Start Session Cleanup Scheduler (every 1 minute)
	go app.runSessionCleanup()

	// 7. Configure HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/api/clients", app.handleClients)
	mux.HandleFunc("/api/verify/init", app.handleVerifyInit)
	mux.HandleFunc("/api/verify/status", app.handleVerifyStatus)
	
	// Serve static files from embedded webFS
	fileServer := http.FileServer(http.FS(app.webFS))
	mux.Handle("/", fileServer)

	// Wrap handler with logging & CORS headers
	handler := loggingMiddleware(corsMiddleware(mux))

	log.Printf("HTTP Server listening on port %s...", port)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}

// corsMiddleware sets headers to allow local cross-origin development testing.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware prints request logs to stdout.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s - %v", r.Method, r.RequestURI, r.RemoteAddr, time.Since(start))
	})
}

// runTelegramBot runs the long polling loop for incoming updates.
func (app *App) runTelegramBot() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := app.bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID
		msgText := update.Message.Text
		username := update.Message.From.UserName
		firstName := update.Message.From.FirstName

		// Detect if it is the /start command
		if strings.HasPrefix(msgText, "/start") {
			parts := strings.Fields(msgText)
			
			// Format: /start auth_token
			if len(parts) == 2 {
				token := parts[1]
				app.handleUserVerification(token, chatID, username, firstName)
			} else {
				// Base /start without token
				reply := tgbotapi.NewMessage(chatID, "Welcome to the Secure Verification Gateway.\n\nPlease initialize verification via your client application's web portal to receive a secure link.")
				_, _ = app.bot.Send(reply)
			}
		} else {
			// Catch-all response
			reply := tgbotapi.NewMessage(chatID, "Please interact with the gateway through your client application verification link.")
			_, _ = app.bot.Send(reply)
		}
	}
}

// handleUserVerification processes /start <token> updates.
func (app *App) handleUserVerification(token string, chatID int64, username, firstName string) {
	// Attempt to verify the session in storage.
	session, err := app.store.VerifySession(token, chatID, username, firstName)
	if err != nil {
		log.Printf("Verification error for token %s: %v", token, err)
		reply := tgbotapi.NewMessage(chatID, fmt.Sprintf("❌ Verification Failed: %v.\nPlease return to your portal and try again.", err))
		_, _ = app.bot.Send(reply)
		return
	}

	log.Printf("Successfully verified user: @%s (ChatID: %d) for token: %s", username, chatID, token)

	// Send instant confirmation message back to user.
	replyText := fmt.Sprintf("✅ **Verification Successful!**\n\nAuthorized for application session.\nUser Reference: `%s`\n\nYou may close this window and return to your web page.", session.UserReference)
	reply := tgbotapi.NewMessage(chatID, replyText)
	reply.ParseMode = tgbotapi.ModeMarkdown
	_, _ = app.bot.Send(reply)

	// Enqueue webhook payload delivery.
	payload := WebhookPayload{
		Event:         "verification.completed",
		Token:         token,
		UserReference: session.UserReference,
		Status:        "VERIFIED",
		Timestamp:     time.Now().Format(time.RFC3339),
	}
	payload.Telegram.ChatID = chatID
	payload.Telegram.Username = username
	payload.Telegram.FirstName = firstName

	_, err = app.store.EnqueueWebhook(token, session.CallbackURL, payload)
	if err != nil {
		log.Printf("Failed to enqueue webhook for token %s: %v", token, err)
	}
}

// runSessionCleanup periodically purges expired sessions.
func (app *App) runSessionCleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		evicted := app.store.CleanupSessions()
		if evicted > 0 {
			log.Printf("Cleaned up %d expired verification sessions", evicted)
		}
	}
}

// handleClients processes client registration.
// POST /api/clients
func (app *App) handleClients(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "Invalid name field", http.StatusBadRequest)
		return
	}

	client, err := app.store.RegisterClient(req.Name)
	if err != nil {
		log.Printf("Failed to register client: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(client)
}

// handleVerifyInit initializes a verification session.
// POST /api/verify/init
func (app *App) handleVerifyInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Bearer Token Auth check
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		http.Error(w, "Unauthorized (Missing Bearer Token)", http.StatusUnauthorized)
		return
	}
	apiKey := strings.TrimPrefix(authHeader, "Bearer ")

	client, found := app.store.GetClientByAPIKey(apiKey)
	if !found {
		http.Error(w, "Unauthorized (Invalid API Key)", http.StatusUnauthorized)
		return
	}

	// 2. Parse request body
	var req struct {
		CallbackURL   string `json:"callback_url"`
		UserReference string `json:"user_reference"`
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.CallbackURL) == "" || strings.TrimSpace(req.UserReference) == "" {
		http.Error(w, "CallbackURL and UserReference are required fields", http.StatusBadRequest)
		return
	}

	// 3. Create pending session (expires in 5 minutes)
	session, err := app.store.CreateSession(client.ID, req.CallbackURL, req.UserReference, 5*time.Minute)
	if err != nil {
		log.Printf("Failed to create session: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// 4. Respond with token and telegram link
	telegramLink := fmt.Sprintf("https://t.me/%s?start=%s", app.bot.Self.UserName, session.Token)
	
	resp := map[string]string{
		"token":         session.Token,
		"telegram_link": telegramLink,
		"expires_at":    session.ExpiresAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleVerifyStatus retrieves current verification session state.
// GET /api/verify/status?token=<token>
func (app *App) handleVerifyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "Missing token query parameter", http.StatusBadRequest)
		return
	}

	session, found := app.store.GetSessionByToken(token)
	if !found {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	// If session has expired in-memory, evaluate status on the fly.
	if session.Status == "PENDING" && time.Now().After(session.ExpiresAt) {
		session.Status = "EXPIRED"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(session)
}
