// Package main is the entry point for the tg-channel-summary-by-ai bot.
// It wires together all components: config, database, bot service, parser,
// summarizer, scheduler, HTTP server, and the embedded WebApp.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/bot"
	"github.com/boss/tg-channel-summary-by-ai/internal/config"
	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/maintenance"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/boss/tg-channel-summary-by-ai/internal/scheduler"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
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
	store, err := db.OpenWithEncryptionKey(cfg.DBPath, cfg.ProviderKey)
	if err != nil {
		log.Fatalf("failed to open database at %s: %v", cfg.DBPath, err)
	}
	defer store.Close()
	log.Printf("database opened at %s", cfg.DBPath)
	if err := ensureDefaultAIProvider(store, cfg.OpenRouterKey); err != nil {
		log.Fatalf("failed to configure default AI provider: %v", err)
	}

	ownerNotifier := bot.NewOwnerNotifier(cfg.BotToken, cfg.OwnerTelegramID, cfg.OpenRouterKey)
	ownerNotifier.SetProviderSecretSource(func() []string {
		providers, err := store.Providers.List()
		if err != nil {
			return nil
		}
		secrets := make([]string, 0, len(providers))
		for _, provider := range providers {
			secrets = append(secrets, provider.APIKey)
		}
		return secrets
	})
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
	webAppAuth, err := webapp.NewWebAppAuthWithOrigin(cfg.BotToken, cfg.OwnerTelegramID, cfg.WebAppURL)
	if err != nil {
		log.Fatalf("failed to configure WebApp authentication: %v", err)
	}
	srv := webapp.NewWithProvidersAuthenticated(store, 10*time.Second, http.DefaultClient, webAppAuth)
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
	channelProcessor := parser.NewChannelProcessor(channelParser, postStorage, ownerNotifier).
		WithMaxRetries(cfg.MaxRetries)
	digestService := digest.NewWithProcessorAndAIWithMaxPostsPerChannel(store, channelProcessor, store.Groups, http.DefaultClient, cfg.MaxPostsPerChan, ownerNotifier)
	sched := scheduler.New(digestService, scheduler.WithGroupSource(store.Groups))
	if err := sched.Start(); err != nil {
		log.Fatalf("failed to start scheduler: %v", err)
	}

	telegramBot, err := bot.NewWithConfig(
		cfg.BotToken,
		cfg.OwnerTelegramID,
		cfg.WebAppURL,
		store.Groups,
		store.Channels,
		ownerNotifier,
		sched,
	)
	if err != nil {
		log.Fatalf("failed to configure Telegram bot: %v", err)
	}
	go func() {
		if err := telegramBot.Start(); err != nil {
			log.Printf("Telegram bot stopped: %v", err)
		}
	}()

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("Received signal %v, shutting down...", sig)

	// Graceful shutdown
	srv.Stop()
	maintenanceSvc.Stop()
	sched.Stop()
	telegramBot.Stop()

	log.Println("Shutdown complete")
}

func ensureDefaultAIProvider(store *db.DB, apiKey string) error {
	if _, err := store.Providers.GetDefault(); err == nil {
		return nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("check default AI provider: %w", err)
	}

	provider, err := store.Providers.GetByName("OpenRouter")
	if errors.Is(err, db.ErrNotFound) {
		_, err = store.Providers.Insert(&model.AIProvider{
			Name:         "OpenRouter",
			BaseURL:      summarizer.DefaultOpenRouterBaseURL,
			APIKey:       apiKey,
			DefaultModel: summarizer.DefaultOpenRouterModel,
			IsDefault:    true,
		})
		if err != nil {
			return fmt.Errorf("insert default AI provider: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load OpenRouter provider: %w", err)
	}

	provider.APIKey = apiKey
	provider.DefaultModel = summarizer.DefaultOpenRouterModel
	provider.IsDefault = true
	if err := store.Providers.Update(provider); err != nil {
		return fmt.Errorf("update default OpenRouter provider: %w", err)
	}
	return nil
}
