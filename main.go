package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

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

type claudeResult struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
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

	// Kill/reset command — clears the session for this channel
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

	// Get session ID for this channel (if any)
	sessionsMu.Lock()
	sessionID := sessions[channel]
	sessionsMu.Unlock()

	postMessage(api, channel, ts, "🤔 Thinking...")

	// Build claude command
	args := []string{"--dangerously-skip-permissions", "-p", text, "--output-format", "json"}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)

	runningMu.Lock()
	runningCmd = cmd
	runningMu.Unlock()

	out, err := cmd.CombinedOutput()

	runningMu.Lock()
	runningCmd = nil
	cancelFunc = nil
	runningMu.Unlock()

	if ctx.Err() != nil {
		postMessage(api, channel, ts, "⛔ Killed.")
		return
	}

	if err != nil {
		reply := string(out)
		if len(reply) > 2800 {
			reply = reply[:2800] + "\n... (truncated)"
		}
		postMessage(api, channel, ts, fmt.Sprintf("❌ Error: %v\n```\n%s\n```", err, reply))
		return
	}

	// Parse JSON response
	var result claudeResult
	if jsonErr := json.Unmarshal(out, &result); jsonErr != nil {
		// Fallback: return raw output
		reply := string(out)
		if len(reply) > 2800 {
			reply = reply[:2800] + "\n... (truncated)"
		}
		postMessage(api, channel, ts, reply)
		return
	}

	// Save session ID for next message
	if result.SessionID != "" {
		sessionsMu.Lock()
		sessions[channel] = result.SessionID
		sessionsMu.Unlock()
	}

	reply := result.Result
	if reply == "" {
		reply = "(no response)"
	}
	if len(reply) > 2800 {
		reply = reply[:2800] + "\n... (truncated)"
	}

	postMessage(api, channel, ts, reply)
}

func postMessage(api *slack.Client, channel, ts, text string) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
	}
	if ts != "" {
		opts = append(opts, slack.MsgOptionTS(ts)) // reply in thread
	}
	_, _, err := api.PostMessage(channel, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PostMessage error: %v\n", err)
	}
}
