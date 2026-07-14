// Package main is the entry point for the tg-channel-summary-by-ai bot.
// It wires together all components: config, database, bot service, parser,
// summarizer, scheduler, HTTP server, and the embedded WebApp.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.Println("tg-channel-summary-by-ai starting...")

	// TODO: Load configuration from .env
	// cfg, err := config.Load()
	// if err != nil { log.Fatal(err) }

	// TODO: Initialize SQLite database
	// db, err := db.Open(cfg.DBPath)
	// if err != nil { log.Fatal(err) }
	// defer db.Close()

	// TODO: Start Telegram bot (long polling)
	// bot := bot.New(cfg.BotToken, db)
	// go bot.Start()

	// TODO: Start HTTP server (health check + WebApp)
	// srv := webapp.NewServer(db)
	// go srv.Start(cfg.Port)

	// TODO: Start digest scheduler
	// sched := scheduler.New(db)
	// sched.Start()

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("Received signal %v, shutting down...", sig)

	// TODO: Graceful shutdown
	// sched.Stop()
	// srv.Stop()
	// bot.Stop()

	log.Println("Shutdown complete")
}
