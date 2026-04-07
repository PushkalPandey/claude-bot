package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"slack-bot/app"
	"slack-bot/config"
	"slack-bot/lib/logger"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	bot := app.New(cfg)

	go bot.Start()

	logger.Info("Claude Slack Bot started")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down gracefully...")
}
