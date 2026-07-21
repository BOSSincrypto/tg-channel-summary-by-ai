# Telegram Channel AI Digest Bot

A single-binary Go application that collects posts from public Telegram channels via `t.me/s`, summarizes them with AI (OpenRouter or custom OpenAI-compatible providers), and delivers daily digests to Telegram forum supergroups with topic support.

## Features

- **Channel parsing** — fetches and parses `https://t.me/s/{username}` server-rendered HTML, extracting message text, media captions, timestamps, and message IDs
- **AI summarization** — batch processes channel posts through OpenRouter (default) or custom OpenAI-compatible providers; generates one-sentence Russian summaries per post
- **Daily digests** — scheduled digest delivery to Telegram forum topics at a configurable time, with MarkdownV2 formatting and in-group cross-channel deduplication
- **Admin WebApp** — inline Telegram mini-app for managing channels, groups, AI providers, and digest settings; validates `initData` server-side
- **Owner-only access** — all admin commands (`/start`, `/settings`) and the WebApp are restricted to the configured owner Telegram ID
- **Durable operation** — SQLite with WAL mode, scheduler reconciliation on restart, pending-close recovery for forum topic lifecycle, and persistent digest state

## Architecture

```
┌─────────────────────────────────────────────────┐
│                 Go App (single binary)            │
│                                                   │
│  ┌──────────┐  ┌──────────┐  ┌────────────────┐  │
│  │ Telegram │  │  t.me/s  │  │  AI Summarizer │  │
│  │ Bot Svc  │  │  Parser  │  │  (OpenRouter /  │  │
│  │ (telego) │  │(goquery) │  │   custom)       │  │
│  └────┬─────┘  └────┬─────┘  └───────┬────────┘  │
│       │              │                │           │
│  ┌────┴──────────────┴────────────────┴─────────┐ │
│  │              SQLite (WAL mode)                │ │
│  └───────────────────────────────────────────────┘ │
│                                                   │
│  ┌──────────────┐  ┌──────────────┐               │
│  │  Scheduler   │  │  WebApp SPA  │               │
│  │ (robfig/     │  │  (embed)     │               │
│  │   cron)      │  │  + chi       │               │
│  └──────────────┘  └──────────────┘               │
│  ┌──────────────┐                                 │
│  │  Health HTTP │  :8080                          │
│  └──────────────┘                                 │
└─────────────────────────────────────────────────┘
```

## Prerequisites

- **Go 1.23+** (built with 1.26.5)
- **Telegram Bot Token** — obtain from [@BotFather](https://t.me/BotFather)
- **OpenRouter API key** — from [openrouter.ai/keys](https://openrouter.ai/keys)
- **A Telegram forum supergroup** — the bot delivers digests to forum topics

## Quick Start

### 1. Clone and configure

```bash
git clone <repo-url>
cd tg-channel-summary-by-ai

# Copy and edit environment
cp .env.example .env
```

Edit `.env` and set the required values:

```
BOT_TOKEN=your_bot_token_here
OWNER_TELEGRAM_ID=your_numeric_telegram_id
OPENROUTER_API_KEY=your_openrouter_key
```

### 2. Install dependencies

```bash
go mod download
```

### 3. Run

```bash
go run ./cmd/bot/
```

The application starts:
- Telegram bot long-polling
- HTTP server on port 8080 (health check at `/health`, WebApp at `/webapp/`)
- Cron scheduler for daily digests

### 4. Verify

```bash
curl -s http://localhost:8080/health
# {"status":"ok"}
```

Open your bot in Telegram and send `/start`. Tap "Open Settings" to access the admin WebApp.

## Configuration

All settings are loaded from environment variables (`.env` file or process environment).

| Variable | Required | Default | Description |
|---|---|---|---|
| `BOT_TOKEN` | Yes | — | Telegram bot token from @BotFather |
| `OWNER_TELEGRAM_ID` | Yes | — | Numeric Telegram user ID of the bot owner |
| `OPENROUTER_API_KEY` | Yes | — | OpenRouter API key |
| `PROVIDER_ENCRYPTION_KEY` | No | `BOT_TOKEN` | Key for encrypting provider API keys at rest |
| `CUSTOM_PROVIDERS` | No | `[]` | JSON array of custom AI providers |
| `DIGEST_TIME` | No | `21:00` | Daily digest time in `HH:MM` format |
| `TIMEZONE` | No | `Europe/Moscow` | IANA timezone for digest scheduling |
| `WEBAPP_URL` | No | `https://tg-channel-summary.fly.dev/webapp/` | HTTPS URL for the WebApp mini-app |
| `PORT` | No | `8080` | HTTP server port (health check + WebApp) |
| `DB_PATH` | No | `bot.db` | SQLite database file path |
| `LOG_LEVEL` | No | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `FETCH_DELAY_MS` | No | `2500` | Delay between channel fetch requests (ms) |
| `MAX_RETRIES` | No | `3` | Max retries for failed channel fetches |
| `MAX_POSTS_PER_CHANNEL` | No | `50` | Max posts per channel per digest |
| `POST_RETENTION_DAYS` | No | `90` | Days to retain scraped posts before cleanup |

Secrets must never be committed. `.env` and `*.db` are in `.gitignore`.

## Testing

```bash
# Run all tests
go test ./... -count=1

# Run tests for a specific package
go test ./internal/parser/... -count=1

# Vet for common issues
go vet ./...

# Verify compilation
go build -o nul ./...
```

Tests use in-memory SQLite and `httptest` mocks. No external services are required for testing.

## Deployment

The application is designed for Fly.io with a persistent volume for the SQLite database.

### Docker

Build and run locally:

```bash
docker build -t tg-channel-summary .
docker run -p 8080:8080 \
  -e BOT_TOKEN=... \
  -e OWNER_TELEGRAM_ID=... \
  -e OPENROUTER_API_KEY=... \
  -v sqlite_data:/data \
  tg-channel-summary
```

The Dockerfile uses a multi-stage build (Go builder → Debian slim runtime) with a non-root user and baked-in health check.

### Fly.io

The repository includes `fly.toml` and a GitHub Actions workflow (`.github/workflows/deploy.yml`) for automatic deployment on push to `main`.

Fly secrets are set separately:

```bash
fly secrets set BOT_TOKEN=... OWNER_TELEGRAM_ID=... OPENROUTER_API_KEY=...
```

## Project Structure

```
cmd/bot/              # Application entry point
internal/
  bot/                # Telegram bot service (commands, updates, membership)
  config/             # Environment configuration loading
  db/                 # SQLite database layer (repositories, migrations)
  digest/             # Digest assembly, formatting, and delivery
  forum/              # Forum topic mutation fencing
  lifecycle/          # Application lifecycle and token revocation coordination
  log/                # Structured logging with secret redaction
  maintenance/        # Periodic DB maintenance (retention cleanup, PRAGMA)
  model/              # Shared data structures
  parser/             # t.me/s HTML parser and post storage
  scheduler/          # Cron-based digest scheduling (robfig/cron v3)
  security/           # Secret redaction for logs and notifications
  summarizer/         # AI summarization (OpenRouter + custom providers)
  telegram/           # Telegram API wrappers and typed errors
  webapp/             # HTTP server (health, WebApp SPA, API endpoints)
webapp/               # Embedded SPA files (index.html, app.js, style.css, sw.js)
```

## Tech Stack

| Component | Library |
|---|---|
| Bot API | `github.com/mymmrac/telego` |
| HTML parsing | `github.com/PuerkitoBio/goquery` |
| HTTP router | `github.com/go-chi/chi/v5` |
| SQLite driver | `modernc.org/sqlite` (pure Go, no CGO) |
| Scheduler | `github.com/robfig/cron/v3` |
| AI API | `net/http` (OpenAI-compatible) |

## Token Revocation

If the bot token is revoked while running, the application detects the 401 response and performs a coordinated shutdown: polling stops, the HTTP/WebApp server enters a bounded terminal state, and all scheduled digest jobs are halted. The owner receives a revocation notification.

## Validator HTTP Mode

For local browser-based validation without external traffic, the application supports `VALIDATOR_HTTP_ONLY=1` with optional `VALIDATOR_FIXTURE=bot-admin-r2`. This mode starts only the HTTP/WebApp server on port 8080 using fake credentials and a temporary SQLite database. It does not start Telegram polling or call any external service. Production `.env` is never loaded in this mode.
