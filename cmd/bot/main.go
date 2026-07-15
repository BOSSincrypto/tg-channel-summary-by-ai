// Package main is the entry point for the tg-channel-summary-by-ai bot.
// It wires together all components: config, database, bot service, parser,
// summarizer, scheduler, HTTP server, and the embedded WebApp.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/bot"
	"github.com/boss/tg-channel-summary-by-ai/internal/config"
	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/maintenance"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/boss/tg-channel-summary-by-ai/internal/scheduler"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
)

func main() {
	log.Println("tg-channel-summary-by-ai starting...")

	// Load configuration from .env or environment variables.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Initialize SQLite so Fly boots against the mounted /data volume.
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to open database at %s: %v", cfg.DBPath, err)
	}
	defer store.Close()
	log.Printf("database opened at %s", cfg.DBPath)

	ownerNotifier := bot.NewOwnerNotifier(cfg.BotToken, cfg.OwnerTelegramID)
	maintenanceSvc := maintenance.New(maintenance.Options{
		RetentionDays: cfg.PostRetentionDays,
		Interval:      24 * time.Hour,
		DBPath:        cfg.DBPath,
		Cleaner:       store,
		ConfigStore:   store.Config,
		Notifier:      ownerNotifier,
	})
	maintenanceSvc.Start()

	// TODO: Start Telegram bot (long polling)
	// bot := bot.New(cfg.BotToken, store)
	// go bot.Start()

	// Start HTTP server (health check + WebApp)
	srv := webapp.New()
	go func() {
		log.Printf("HTTP server listening on :%s", cfg.Port)
		if err := srv.Start(cfg.Port); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Wire the production parser -> post storage -> digest path before the
	// scheduler starts. Scheduled group runs use this same injected service.
	channelParser := parser.New()
	postStorage := parser.NewPostStorage(store.Channels, store.Posts)
	channelProcessor := parser.NewChannelProcessor(channelParser, postStorage)
	digestService := digest.NewWithProcessor(store, channelProcessor)
	sched := scheduler.New(digestService)
	sched.Start()

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("Received signal %v, shutting down...", sig)

	// Graceful shutdown
	srv.Stop()
	maintenanceSvc.Stop()
	sched.Stop()
	// TODO: bot.Stop()

	log.Println("Shutdown complete")
}
