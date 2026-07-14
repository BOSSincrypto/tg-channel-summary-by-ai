// Package main is the entry point for the tg-channel-summary-by-ai bot.
// It wires together all components: config, database, bot service, parser,
// summarizer, scheduler, HTTP server, and the embedded WebApp.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/boss/tg-channel-summary-by-ai/internal/config"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
)

func main() {
	log.Println("tg-channel-summary-by-ai starting...")

	// Load configuration from .env
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// TODO: Initialize SQLite database
	// db, err := db.Open(cfg.DBPath)
	// if err != nil { log.Fatal(err) }
	// defer db.Close()

	// TODO: Start Telegram bot (long polling)
	// bot := bot.New(cfg.BotToken, db)
	// go bot.Start()

	// Start HTTP server (health check + WebApp)
	srv := webapp.New()
	go func() {
		log.Printf("HTTP server listening on :%s", cfg.Port)
		if err := srv.Start(cfg.Port); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// TODO: Start digest scheduler
	// sched := scheduler.New(db)
	// sched.Start()

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("Received signal %v, shutting down...", sig)

	// Graceful shutdown
	srv.Stop()
	// TODO: sched.Stop()
	// TODO: bot.Stop()

	log.Println("Shutdown complete")
}
