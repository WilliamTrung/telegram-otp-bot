# Telegram OTP Verification Gateway

A lightweight, zero-cost, deep-linked Telegram authentication and OTP verification gateway service written in Go. 

The gateway acts as an authentication provider, allowing external applications to register, generate API keys, request secure user verifications, and receive resilient webhook callbacks containing verified user details once authorization is completed in the Telegram application.

---

## ✨ Features

- **Zero-Cost Infrastructure**: Designed to run securely on free-tier cloud instances (e.g. Oracle Cloud Free Tier) or local devices (e.g. Raspberry Pi) behind a NAT with **zero open inbound ports** required for the Telegram bot (uses Outbound Long Polling).
- **Primary Redis Store with JSON Fallback**: Uses **Redis** as the primary storage engine for clients, sessions, and queues. Performs automatic connection checks on startup, logging a warning and falling back to a local thread-safe **JSON file store** (`data/store.json`) if Redis is offline or unconfigured.
- **Resilient Webhook Worker**: Dispatches callback results in a background loop with:
  - **Exponential Backoff Retries**: Double delay intervals (2s, 4s, 8s, 16s...) capped at 1 hour for failed deliveries.
  - **Automatic Eviction**: Webhook logs are automatically purged from memory/Redis after 7 days.
- **Circuit Breaker Pattern**: Tracks consecutive callback failures per client host. If failures reach **5**, the breaker transitions to `OPEN` and halts webhook dispatches to that host for a **15-second cool-down** period before transitioning to `HALF-OPEN` and retrying.
- **Aesthetic Developer Portal**: Serves an embedded, glassmorphic UI directly from the binary (`go:embed`) that provides:
  - Client API Key generator.
  - Verification Sandbox Simulator (renders redirection links, QR codes, active timers, and live polling logs).
  - Documentation integration guide.

---

## 📐 Architecture & Flow

```mermaid
sequenceDiagram
    participant ClientApp as External Client Service
    participant Gateway as Go Backend Gateway
    participant DB as Redis / JSON fallback
    participant Bot as Telegram Bot Loop
    participant TG as Telegram App

    Note over ClientApp, Gateway: 1. Application Registration
    ClientApp->>Gateway: POST /api/clients { "name": "My App" }
    Gateway->>DB: Save Client & API Key
    Gateway-->>ClientApp: Return client_id & api_key

    Note over ClientApp, Gateway: 2. Initiate Verification
    ClientApp->>Gateway: POST /api/verify/init (Auth: Bearer API_KEY)
    Note over Gateway: Create PENDING session
    Gateway->>DB: Save session (Expires in 5m)
    Gateway-->>ClientApp: Return verification_token & telegram_link (t.me/bot?start=token)

    Note over ClientApp, TG: 3. User Interaction
    TG->>Bot: User clicks "START" -> Sends /start token
    Note over Bot: Extract and validate token
    Bot->>DB: Update status to VERIFIED & save chat_id
    Bot-->>TG: Send success message to Telegram user

    Note over Gateway, ClientApp: 4. Webhook Dispatch (Resilient Loop)
    Gateway->>ClientApp: POST callback_url (Payload: verified user details)
    alt HTTP 200 OK
        Note over Gateway: Mark webhook SUCCESS
    else Error / Non-200
        Note over Gateway: Apply exponential backoff. Trip breaker if failures >= 5.
    end
```

---

## ⚡ API Endpoints

### 1. Register Client Application
* **Endpoint**: `POST /api/clients`
* **Request Body**:
```json
{
  "name": "My Client App"
}
```
* **Response Payload**:
```json
{
  "id": "cli_9f0a2d48",
  "name": "My Client App",
  "api_key": "vkey_cf5e6c7d8b9a0e...",
  "created_at": "2026-06-05T17:00:00Z"
}
```

### 2. Initialize Verification Session
* **Endpoint**: `POST /api/verify/init`
* **Headers**: `Authorization: Bearer <your_api_key>`
* **Request Body**:
```json
{
  "callback_url": "https://your-service.com/webhook/verify",
  "user_reference": "user_id_101"
}
```
* **Response Payload**:
```json
{
  "token": "auth_90a36bc8de...",
  "telegram_link": "https://t.me/your_bot_username?start=auth_90a36bc8de...",
  "expires_at": "2026-06-05T17:05:00Z"
}
```

### 3. Check Session Status (Optional Polling)
* **Endpoint**: `GET /api/verify/status?token=<token>`
* **Response Payload**:
```json
{
  "token": "auth_90a36bc8de...",
  "client_id": "cli_9f0a2d48",
  "callback_url": "https://your-service.com/webhook/verify",
  "user_reference": "user_id_101",
  "status": "VERIFIED",
  "chat_id": 98765432,
  "telegram_user": "john_doe",
  "telegram_first": "John",
  "created_at": "2026-06-05T17:00:00Z",
  "expires_at": "2026-06-05T17:05:00Z"
}
```

---

## 🔔 Webhook Callback Payload

When the user clicks "START" in Telegram, the gateway dispatches a `POST` request to the `callback_url` provided during session initialization:

```json
{
  "event": "verification.completed",
  "token": "auth_90a36bc8de...",
  "user_reference": "user_id_101",
  "status": "VERIFIED",
  "telegram": {
    "chat_id": 98765432,
    "username": "john_doe",
    "first_name": "John"
  },
  "timestamp": "2026-06-05T17:01:10Z"
}
```
*Your service **MUST** respond with an HTTP `200 OK` status to register delivery success.*

---

## 🔄 Step-by-Step API Integration Walkthrough

Below is a complete sequence of `curl` commands demonstrating the step-by-step API call flow from application registration to user verification.

### Step 1: Register Your Application
First, register your external application to obtain your unique Client ID and API Key (`vkey_...`).

```bash
curl -X POST http://localhost:8080/api/clients \
  -H "Content-Type: application/json" \
  -d '{"name": "My Client App"}'
```

**Expected Response:**
```json
{
  "id": "cli_9f0a2d48",
  "name": "My Client App",
  "api_key": "vkey_cf5e6c7d8b9a0e...",
  "created_at": "2026-06-06T07:00:00Z"
}
```

### Step 2: Initialize a Verification Session
When a user on your frontend initiates the verification flow, request a verification session from the gateway using your API Key. Pass your `callback_url` (where callbacks will be POSTed) and a unique `user_reference`.

```bash
curl -X POST http://localhost:8080/api/verify/init \
  -H "Authorization: Bearer vkey_cf5e6c7d8b9a0e..." \
  -H "Content-Type: application/json" \
  -d '{
    "callback_url": "https://your-service.com/webhook/verify",
    "user_reference": "user_id_101"
  }'
```

**Expected Response:**
```json
{
  "token": "auth_90a36bc8de...",
  "telegram_link": "https://t.me/your_bot_username?start=auth_90a36bc8de...",
  "expires_at": "2026-06-06T07:05:00Z"
}
```

### Step 3: Direct the User to Telegram
Redirect your user to the `telegram_link` generated in Step 2.
- When clicked, it will open the chat with your bot inside their Telegram app.
- They will be prompted to click the **"START"** button.

### Step 4: Handle the Verification Result

#### Method A: Receive the Webhook (Recommended)
Once the user clicks **"START"**, the bot registers the interaction and the gateway automatically dispatches a secure callback webhook POST to the `callback_url` you provided:

```json
{
  "event": "verification.completed",
  "token": "auth_90a36bc8de...",
  "user_reference": "user_id_101",
  "status": "VERIFIED",
  "telegram": {
    "chat_id": 98765432,
    "username": "john_doe",
    "first_name": "John"
  },
  "timestamp": "2026-06-06T07:01:10Z"
}
```
*Your service **MUST** respond with an HTTP `200 OK` status to register delivery success.*

#### Method B: Poll the Status API (Fallback)
If your service has webhooks disabled or firewalled, you can check the session status by polling the verification endpoint:

```bash
curl http://localhost:8080/api/verify/status?token=auth_90a36bc8de...
```

**Expected Response:**
```json
{
  "token": "auth_90a36bc8de...",
  "client_id": "cli_9f0a2d48",
  "callback_url": "https://your-service.com/webhook/verify",
  "user_reference": "user_id_101",
  "status": "VERIFIED",
  "chat_id": 98765432,
  "telegram_user": "john_doe",
  "telegram_first": "John",
  "created_at": "2026-06-06T07:00:00Z",
  "expires_at": "2026-06-06T07:05:00Z"
}
```

---

## 🛠️ Installation & Setup

### Prerequisites
- Go 1.25+ (if running locally)
- Telegram bot token (from [@BotFather](https://t.me/BotFather))
- Optional: Redis Server (if using Redis storage)

### 1. Environment Configuration
Create a `.env` file in the root of the project:
```env
TELEGRAM_BOT_TOKEN=your_telegram_bot_token_here
PORT=8080
# Optional: leave empty to fallback to local JSON file storage automatically
REDIS_URL=redis://:password@192.168.0.105:6379/0
```

### 2. Run Locally
```bash
go run main.go
```
Open `http://localhost:8080` in your browser to access the Developer Console and Sandbox.

### 3. Run with Docker Compose
```bash
docker compose up --build -d
```

---

## 🧪 Running Tests
Unit tests automatically parse your root `.env` to verify end-to-end integration connectivity. To force test execution and ignore cached results, run:
```bash
go test -count=1 -v ./...
```
