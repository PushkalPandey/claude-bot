package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"slack-bot/claude"
	"slack-bot/lib/logger"
)

// handleMessage is the core message dispatcher.
// It runs in its own goroutine per incoming Slack message.
func (b *Bot) handleMessage(ev *slackevents.MessageEvent) {
	text := strings.TrimSpace(ev.Text)
	channel := ev.Channel
	ts := ev.TimeStamp

	switch {
	case strings.EqualFold(text, "stop"), strings.EqualFold(text, "kill"):
		b.handleKill(channel, ts)
	case strings.EqualFold(text, "reset"), strings.EqualFold(text, "new session"):
		b.handleReset(channel, ts)
	default:
		b.handlePrompt(channel, ts, text)
	}
}

// handleKill cancels any running Claude subprocess.
func (b *Bot) handleKill(channel, ts string) {
	b.runningMu.Lock()
	if b.cancelFunc != nil {
		b.cancelFunc()
		b.cancelFunc = nil
		b.runningMu.Unlock()
		b.postMessage(channel, ts, "⛔ Killed.")
		logger.Info("Claude process killed", "channel", channel)
	} else {
		b.runningMu.Unlock()
		b.postMessage(channel, ts, "No command is currently running.")
	}
}

// handleReset clears the stored Claude session for this channel.
func (b *Bot) handleReset(channel, ts string) {
	b.sessionDelete(channel)
	b.postMessage(channel, ts, "🔄 Session reset. Starting fresh.")
	logger.Info("Session reset", "channel", channel)
}

// handlePrompt runs Claude with the user's text and streams the response back.
func (b *Bot) handlePrompt(channel, ts, text string) {
	// Reject if a Claude process is already running.
	b.runningMu.Lock()
	if b.runningCmd != nil && b.runningCmd.ProcessState == nil {
		b.runningMu.Unlock()
		b.postMessage(channel, ts, "⚠️ Claude is still thinking. Send `stop` to kill it first.")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancelFunc = cancel
	b.runningMu.Unlock()

	defer func() {
		b.runningMu.Lock()
		b.runningCmd = nil
		b.cancelFunc = nil
		b.runningMu.Unlock()
	}()

	sessionID := b.sessionGet(channel)

	// Post the initial placeholder message that will be updated live.
	_, msgTS, err := b.api.PostMessage(channel,
		slack.MsgOptionText("🤔 Thinking...", false),
		slack.MsgOptionTS(ts),
	)
	if err != nil {
		logger.Error("PostMessage error", "err", err, "channel", channel)
		return
	}

	state, done, cmd, err := claude.Run(ctx, text, sessionID)
	if err != nil {
		b.updateMessage(channel, msgTS, fmt.Sprintf("❌ Failed to start Claude: %v", err))
		logger.Error("Failed to start Claude", "err", err)
		return
	}

	b.runningMu.Lock()
	b.runningCmd = cmd
	b.runningMu.Unlock()

	// Tick every 2 seconds to push live updates into the Slack message.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			b.updateMessage(channel, msgTS, state.BuildLive(false))
		case <-done:
			break loop
		}
	}

	cmd.Wait() //nolint:errcheck // exit code is surfaced via the result event

	// Cancelled by "stop" command.
	if ctx.Err() != nil {
		b.updateMessage(channel, msgTS, "⛔ Killed.")
		return
	}

	fe := state.GetFinalEvent()
	if fe == nil {
		b.updateMessage(channel, msgTS, "❌ No response from Claude.")
		return
	}

	if fe.IsError {
		errMsg := fe.Result
		if errMsg == "" {
			errMsg = "(unknown error)"
		}
		b.updateMessage(channel, msgTS, fmt.Sprintf("❌ Error: %s", errMsg))
		logger.Error("Claude returned an error", "result", errMsg, "channel", channel)
		return
	}

	// Persist session for next message in this channel.
	if fe.SessionID != "" {
		b.sessionSet(channel, fe.SessionID)
	}

	finalMsg := state.BuildLive(true)
	finalMsg += fmt.Sprintf("\n✅ *Done* · %.1fs · %d turns · $%.4f",
		float64(fe.DurationMs)/1000.0, fe.NumTurns, fe.TotalCostUSD)

	if len(finalMsg) > 3900 {
		finalMsg = finalMsg[:3900] + "\n…(truncated)"
	}

	b.updateMessage(channel, msgTS, finalMsg)
	logger.Info("Response complete",
		"channel", channel,
		"turns", fe.NumTurns,
		"cost_usd", fe.TotalCostUSD,
		"duration_ms", fe.DurationMs,
	)
}

// --- Slack helpers ---

// postMessage sends a new message, optionally threaded under ts.
func (b *Bot) postMessage(channel, ts, text string) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if ts != "" {
		opts = append(opts, slack.MsgOptionTS(ts))
	}
	if _, _, err := b.api.PostMessage(channel, opts...); err != nil {
		logger.Error("PostMessage error", "err", err, "channel", channel)
	}
}

// updateMessage edits an existing Slack message in place.
func (b *Bot) updateMessage(channel, ts, text string) {
	if _, _, _, err := b.api.UpdateMessage(channel, ts, slack.MsgOptionText(text, false)); err != nil {
		logger.Error("UpdateMessage error", "err", err, "channel", channel)
	}
}
