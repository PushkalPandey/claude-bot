# Slack Bot — Claude AI Integration

A Slack bot written in Go that forwards messages to the **Claude CLI** and posts replies back as threaded messages. It connects to Slack over **Socket Mode** (WebSocket), so no public URL or server is required.

---

## How it works

```
User sends Slack message
        ↓
Bot receives it via Socket Mode (WebSocket)
        ↓
Runs: claude --dangerously-skip-permissions -p "<message>" --output-format json
        ↓
Parses JSON response from Claude
        ↓
Posts reply in the Slack thread
```

Each Slack channel maintains its own Claude **session**, so conversations have memory across messages.

---

## Prerequisites

- **Go 1.25+**
- **Claude CLI** installed and authenticated (`claude` must be available in PATH)
- A **Slack App** with Socket Mode enabled

---

## Setup

### 1. Create a Slack App

1. Go to [api.slack.com/apps](https://api.slack.com/apps) → **Create New App** → From scratch
2. Under **OAuth & Permissions**, add these **Bot Token Scopes**:
   - `chat:write`
   - `channels:history`
   - `groups:history`
   - `im:history`
3. Under **Event Subscriptions**, enable events and subscribe to:
   - `message.channels`
   - `message.groups`
   - `message.im`
4. Under **Socket Mode**, enable it and generate an **App-Level Token** with `connections:write` scope
5. Install the app to your workspace

### 2. Set environment variables

```bash
export SLACK_BOT_TOKEN="xoxb-..."   # Bot User OAuth Token
export SLACK_APP_TOKEN="xapp-..."   # App-Level Token (Socket Mode)
```

### 3. Run

```bash
# Option A — build then run
go build -o slack-bot .
./slack-bot

# Option B — run directly
go run main.go
```

---

## Slack Commands

| Message | Action |
|---------|--------|
| Any text | Sent to Claude as a prompt; reply posted in thread |
| `stop` / `kill` | Kills the currently running Claude process |
| `reset` / `new session` | Clears conversation history for this channel |

---

## Project Structure

```
slack-bot/
├── main.go       # All application code
├── go.mod        # Go module definition
├── go.sum        # Dependency checksums
├── flow.html     # Visual code flow diagram (open in browser)
└── README.md
```

### Key functions in `main.go`

| Function | Lines | Purpose |
|----------|-------|---------|
| `main()` | 33–78 | Starts bot, sets up Slack client, event loop |
| `handleMessage()` | 80–190 | Routes messages, runs Claude, returns response |
| `postMessage()` | 192–203 | Sends a message to Slack (threaded reply) |
| `claudeResult` | 27–31 | Struct for parsing Claude's JSON output |

---

## Global State

| Variable | Type | Purpose |
|----------|------|---------|
| `runningCmd` | `*exec.Cmd` | Reference to the active Claude subprocess |
| `cancelFunc` | `context.CancelFunc` | Cancels the running subprocess on `stop` |
| `sessions` | `map[string]string` | Maps channel ID → Claude session ID |
| `runningMu` | `sync.Mutex` | Protects `runningCmd` and `cancelFunc` |
| `sessionsMu` | `sync.Mutex` | Protects the `sessions` map |

Only **one** Claude process runs at a time. If a new message arrives while Claude is running, the bot replies with a warning and rejects it.

---

## Claude CLI flags used

```
claude --dangerously-skip-permissions -p "<prompt>" --output-format json [--resume <session_id>]
```

- `--dangerously-skip-permissions` — skips interactive permission prompts
- `-p` — non-interactive prompt mode
- `--output-format json` — returns structured JSON with `result`, `session_id`, `is_error`
- `--resume` — continues an existing conversation session

---

## Dependencies

```
github.com/slack-go/slack v0.21.0
github.com/gorilla/websocket v1.5.3  (indirect)
```

Install with:

```bash
go mod download
```

---

## Visual Flow

Open **`flow.html`** in your browser for a full interactive diagram of the code flow, concurrency model, and data structures.
