package config

import (
	"fmt"
	"os"
)

// Config holds all environment-sourced configuration for the bot.
type Config struct {
	BotToken string // SLACK_BOT_TOKEN (xoxb-...)
	AppToken string // SLACK_APP_TOKEN (xapp-...)
}

// Load reads required environment variables and returns a validated Config.
// Exits with a descriptive error if any required variable is missing.
func Load() (*Config, error) {
	cfg := &Config{
		BotToken: os.Getenv("SLACK_BOT_TOKEN"),
		AppToken: os.Getenv("SLACK_APP_TOKEN"),
	}

	if cfg.BotToken == "" {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN is not set")
	}
	if cfg.AppToken == "" {
		return nil, fmt.Errorf("SLACK_APP_TOKEN is not set")
	}

	return cfg, nil
}
