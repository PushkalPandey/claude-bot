package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

var (
	runningCmd *exec.Cmd
	runningMu  sync.Mutex
	cancelFunc context.CancelFunc

	// Per-channel Claude session IDs
	sessions   = map[string]string{}
	sessionsMu sync.Mutex
)

// Stream JSON event types from Claude CLI --output-format stream-json
type streamEvent struct {
	Type         string        `json:"type"`
	Message      *assistantMsg `json:"message,omitempty"`
	Result       string        `json:"result,omitempty"`
	SessionID    string        `json:"session_id,omitempty"`
	IsError      bool          `json:"is_error,omitempty"`
	DurationMs   int64         `json:"duration_ms,omitempty"`
	TotalCostUSD float64       `json:"total_cost_usd,omitempty"`
	Usage        *tokenUsage   `json:"usage,omitempty"`
}

type assistantMsg struct {
	Content []contentBlock `json:"content"`
	Usage   *tokenUsage    `json:"usage,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type tokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func main() {
	botToken := os.Getenv("SLACK_BOT_TOKEN")
	appToken := os.Getenv("SLACK_APP_TOKEN")

	if botToken == "" || appToken == "" {
		fmt.Fprintln(os.Stderr, "SLACK_BOT_TOKEN and SLACK_APP_TOKEN must be set")
		os.Exit(1)
	}

	api := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
	)

	client := socketmode.New(api)

	go func() {
		for evt := range client.Events {
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				fmt.Println("Connecting to Slack...")
			case socketmode.EventTypeConnected:
				fmt.Println("Connected! Listening for messages via Socket Mode.")
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				client.Ack(*evt.Request)

				if eventsAPIEvent.Type == slackevents.CallbackEvent {
					switch ev := eventsAPIEvent.InnerEvent.Data.(type) {
					case *slackevents.MessageEvent:
						// Ignore bot messages, edits, deletes
						if ev.BotID != "" || ev.SubType != "" {
							continue
						}
						go handleMessage(api, ev)
					}
				}
			}
		}
	}()

	client.Run()
}

func handleMessage(api *slack.Client, ev *slackevents.MessageEvent) {
	text := strings.TrimSpace(ev.Text)
	channel := ev.Channel
	ts := ev.TimeStamp

	// Kill command
	if strings.EqualFold(text, "stop") || strings.EqualFold(text, "kill") {
		runningMu.Lock()
		if cancelFunc != nil {
			cancelFunc()
			cancelFunc = nil
			runningMu.Unlock()
			postMessage(api, channel, ts, "⛔ Killed.")
		} else {
			runningMu.Unlock()
			postMessage(api, channel, ts, "No command is currently running.")
		}
		return
	}

	// Reset session for this channel
	if strings.EqualFold(text, "reset") || strings.EqualFold(text, "new session") {
		sessionsMu.Lock()
		delete(sessions, channel)
		sessionsMu.Unlock()
		postMessage(api, channel, ts, "🔄 Session reset. Starting fresh.")
		return
	}

	// Reject if already running
	runningMu.Lock()
	if runningCmd != nil && runningCmd.ProcessState == nil {
		runningMu.Unlock()
		postMessage(api, channel, ts, "⚠️ Claude is still thinking. Send `stop` to kill it first.")
		return
	}

	// Set up cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	cancelFunc = cancel
	runningMu.Unlock()

	defer func() {
		runningMu.Lock()
		runningCmd = nil
		cancelFunc = nil
		runningMu.Unlock()
	}()

	// Get session ID for this channel (if any)
	sessionsMu.Lock()
	sessionID := sessions[channel]
	sessionsMu.Unlock()

	// Post initial "Thinking..." message and capture its TS for live updates
	_, msgTS, err := api.PostMessage(channel,
		slack.MsgOptionText("🤔 Thinking...", false),
		slack.MsgOptionTS(ts),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PostMessage error: %v\n", err)
		return
	}

	// Build claude command with stream-json
	args := []string{"--dangerously-skip-permissions", "-p", text, "--output-format", "stream-json"}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		updateMessage(api, channel, msgTS, "❌ Failed to start Claude.")
		return
	}

	if err := cmd.Start(); err != nil {
		updateMessage(api, channel, msgTS, fmt.Sprintf("❌ Failed to start Claude: %v", err))
		return
	}

	runningMu.Lock()
	runningCmd = cmd
	runningMu.Unlock()

	// Shared state updated by the reader goroutine
	var (
		mu           sync.Mutex
		accumulated  string
		inputTokens  int
		outputTokens int
		finalEvent   *streamEvent
	)

	// Read stream-json lines in background
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MB buffer
		for scanner.Scan() {
			var evt streamEvent
			if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
				continue
			}
			mu.Lock()
			switch evt.Type {
			case "assistant":
				if evt.Message != nil {
					for _, c := range evt.Message.Content {
						if c.Type == "text" {
							accumulated += c.Text
						}
					}
					if evt.Message.Usage != nil {
						inputTokens = evt.Message.Usage.InputTokens
						outputTokens = evt.Message.Usage.OutputTokens
					}
				}
				if evt.Usage != nil {
					inputTokens = evt.Usage.InputTokens
					outputTokens = evt.Usage.OutputTokens
				}
			case "result":
				finalEvent = &evt
				if evt.Usage != nil {
					inputTokens = evt.Usage.InputTokens
					outputTokens = evt.Usage.OutputTokens
				}
			}
			mu.Unlock()
		}
	}()

	// Ticker: edit the Slack message every 2 seconds while running
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			mu.Lock()
			partial := accumulated
			in, out := inputTokens, outputTokens
			mu.Unlock()

			if partial != "" {
				msg := fmt.Sprintf("💭 Claude is responding...\n\n%s\n\n⏱ Tokens: %d in / %d out", partial, in, out)
				if len(msg) > 3000 {
					msg = msg[:3000] + "\n... (truncated)"
				}
				updateMessage(api, channel, msgTS, msg)
			}
		case <-done:
			break loop
		}
	}

	cmd.Wait()

	if ctx.Err() != nil {
		updateMessage(api, channel, msgTS, "⛔ Killed.")
		return
	}

	mu.Lock()
	fe := finalEvent
	accText := accumulated
	in, out := inputTokens, outputTokens
	mu.Unlock()

	if fe == nil {
		updateMessage(api, channel, msgTS, "❌ No response from Claude.")
		return
	}

	if fe.IsError {
		reply := fe.Result
		if reply == "" {
			reply = "(unknown error)"
		}
		updateMessage(api, channel, msgTS, fmt.Sprintf("❌ Error: %s", reply))
		return
	}

	// Save session ID for next message in this channel
	if fe.SessionID != "" {
		sessionsMu.Lock()
		sessions[channel] = fe.SessionID
		sessionsMu.Unlock()
	}

	// Build final message
	reply := fe.Result
	if reply == "" {
		reply = accText
	}
	if reply == "" {
		reply = "(no response)"
	}

	footer := fmt.Sprintf("✅ Done · %.1fs · %d in / %d out tokens · $%.4f",
		float64(fe.DurationMs)/1000.0, in, out, fe.TotalCostUSD)

	finalMsg := reply + "\n\n" + footer
	if len(finalMsg) > 3000 {
		maxReply := 3000 - len(footer) - 10
		if maxReply > 0 {
			reply = reply[:maxReply] + "...\n(truncated)"
		}
		finalMsg = reply + "\n\n" + footer
	}

	updateMessage(api, channel, msgTS, finalMsg)
}

func postMessage(api *slack.Client, channel, ts, text string) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if ts != "" {
		opts = append(opts, slack.MsgOptionTS(ts))
	}
	_, _, err := api.PostMessage(channel, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PostMessage error: %v\n", err)
	}
}

func updateMessage(api *slack.Client, channel, ts, text string) {
	_, _, _, err := api.UpdateMessage(channel, ts, slack.MsgOptionText(text, false))
	if err != nil {
		fmt.Fprintf(os.Stderr, "UpdateMessage error: %v\n", err)
	}
}
