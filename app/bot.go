package app

import (
	"context"
	"os/exec"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"slack-bot/config"
	"slack-bot/lib/logger"
)

// Bot wires together the Slack client, Claude runner, and per-channel session store.
type Bot struct {
	api    *slack.Client
	client *socketmode.Client

	// Guards the active Claude subprocess and its cancel function.
	runningMu  sync.Mutex
	runningCmd *exec.Cmd
	cancelFunc context.CancelFunc

	// Per-channel Claude session IDs for conversation continuity.
	sessionsMu sync.Mutex
	sessions   map[string]string
}

// New creates a Bot from the loaded Config.
func New(cfg *config.Config) *Bot {
	api := slack.New(
		cfg.BotToken,
		slack.OptionAppLevelToken(cfg.AppToken),
	)
	return &Bot{
		api:      api,
		client:   socketmode.New(api),
		sessions: make(map[string]string),
	}
}

// Start begins the Socket Mode event loop (blocking).
func (b *Bot) Start() {
	go b.listenEvents()
	logger.Info("Slack Socket Mode client starting")
	b.client.Run()
}

// listenEvents processes all incoming Socket Mode events.
func (b *Bot) listenEvents() {
	for evt := range b.client.Events {
		switch evt.Type {
		case socketmode.EventTypeConnecting:
			logger.Info("Connecting to Slack...")
		case socketmode.EventTypeConnected:
			logger.Info("Connected to Slack via Socket Mode")
		case socketmode.EventTypeEventsAPI:
			eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
			if !ok {
				continue
			}
			b.client.Ack(*evt.Request)

			if eventsAPIEvent.Type == slackevents.CallbackEvent {
				switch ev := eventsAPIEvent.InnerEvent.Data.(type) {
				case *slackevents.MessageEvent:
					// Ignore bot messages, edits, and deletes.
					if ev.BotID != "" || ev.SubType != "" {
						continue
					}
					go b.handleMessage(ev)
				}
			}
		}
	}
}

// --- Session helpers ---

func (b *Bot) sessionGet(channel string) string {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	return b.sessions[channel]
}

func (b *Bot) sessionSet(channel, sessionID string) {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	b.sessions[channel] = sessionID
}

func (b *Bot) sessionDelete(channel string) {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	delete(b.sessions, channel)
}
