// Package main is the entry point for the tg-channel-summary-by-ai bot.
// It wires together all components: config, database, bot service, parser,
// summarizer, scheduler, HTTP server, and the embedded WebApp.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/bot"
	"github.com/boss/tg-channel-summary-by-ai/internal/config"
	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/lifecycle"
	"github.com/boss/tg-channel-summary-by-ai/internal/maintenance"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/boss/tg-channel-summary-by-ai/internal/scheduler"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
	"github.com/mymmrac/telego"
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
	appLifecycle := lifecycle.New(5 * time.Second)
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

	// Configure the HTTP server (health check + WebApp) before wiring the
	// remaining production services.
	webAppAuth, err := webapp.NewWebAppAuthWithOrigin(cfg.BotToken, cfg.OwnerTelegramID, cfg.WebAppURL)
	if err != nil {
		log.Fatalf("failed to configure WebApp authentication: %v", err)
	}
	srv := webapp.NewWithProvidersAuthenticated(store, 10*time.Second, http.DefaultClient, webAppAuth)
	revocationHandler := func(err error) {
		log.Printf("FATAL: Bot token revoked (401 Unauthorized): %v", err)
		srv.EnterTerminal(err)
		appLifecycle.TokenRevoked(err)
	}
	srv.SetTokenRevocationHandler(revocationHandler)
	ownerNotifier.SetTokenRevocationHandler(revocationHandler)
	srv.SetChannelVerificationRetry(cfg.MaxRetries, nil)

	// Wire the production parser -> post storage -> digest path before the
	// scheduler starts. Scheduled group runs use this same injected service.
	channelParser := parser.New()
	postStorage := parser.NewPostStorage(store.Channels, store.Posts)
	channelProcessor := parser.NewChannelProcessor(channelParser, postStorage, ownerNotifier).
		WithMaxRetries(cfg.MaxRetries)
	digestService := digest.NewWithProcessorAndAIWithMaxPostsPerChannel(store, channelProcessor, store.Groups, http.DefaultClient, cfg.MaxPostsPerChan, ownerNotifier)
	srv.SetDigestRunner(digestService)
	sched := scheduler.New(digestService, scheduler.WithGroupSource(store.Groups))

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
	telegramBot.SetTokenRevocationHandler(revocationHandler)
	srv.SetTopicLifecycle(telegramBot)
	telegramBot.SetForumTopicRegistry(store.ForumTopics)
	if err := telegramBot.ReconcilePendingTopicClosures(context.Background()); err != nil {
		log.Printf("pending forum topic reconciliation incomplete: %v", err)
	}
	appLifecycle.Add(srv)
	appLifecycle.Add(maintenanceSvc)
	appLifecycle.Add(sched)
	appLifecycle.Add(telegramBot)
	maintenanceSvc.Start()
	if terminal, _ := appLifecycle.Terminal(); terminal {
		<-appLifecycle.Done()
		return
	}
	digestService.SetDelivery(telegramBot)
	if err := sched.Start(); err != nil {
		log.Fatalf("failed to start scheduler: %v", err)
	}
	if terminal, _ := appLifecycle.Terminal(); terminal {
		<-appLifecycle.Done()
		return
	}
	telegramBot.SetSettingsApplier(func(ctx context.Context, _ *telego.Message, settings bot.BotSettings) error {
		return applyProductionSettings(ctx, store, sched, settings)
	})
	srv.SetSettingsApplier(func(ctx context.Context, mutation webapp.SettingsMutation) (int64, error) {
		return applyProductionSettingsMutation(ctx, store, sched, mutation)
	})
	go func() {
		log.Printf("HTTP server listening on :%s", cfg.Port)
		if err := srv.Start(cfg.Port); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()
	go func() {
		if err := telegramBot.Start(); err != nil {
			log.Printf("Telegram bot stopped: %v", err)
		}
	}()

	// Wait for a signal or a coordinated terminal transition.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-quit:
		log.Printf("Received signal %v, shutting down...", sig)
	case <-appLifecycle.Done():
		log.Printf("Application entered bounded terminal state")
	}

	// Graceful shutdown
	srv.Stop()
	maintenanceSvc.Stop()
	sched.Stop()
	telegramBot.Stop()

	log.Println("Shutdown complete")
}

// applyProductionSettings is the production update boundary for Telegram
// WebApp sendData. It persists the payload and updates every configured group
// while refreshing the already-running shared scheduler instance.
func applyProductionSettings(ctx context.Context, store *db.DB, sched *scheduler.Scheduler, settings bot.BotSettings) error {
	_, err := applyProductionSettingsMutation(ctx, store, sched, webapp.SettingsMutation{
		DigestTime: settings.DigestTime,
		Channels:   settings.Channels,
	})
	return err
}

type persistedWebAppSettings struct {
	DigestTime   string   `json:"digest_time"`
	Timezone     string   `json:"timezone"`
	DefaultModel string   `json:"default_model"`
	Channels     []string `json:"channels"`
}

func applyProductionSettingsMutation(ctx context.Context, store *db.DB, sched *scheduler.Scheduler, mutation webapp.SettingsMutation) (int64, error) {
	if store == nil || sched == nil {
		return 0, errors.New("production settings dependencies are not configured")
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("apply production settings: %w", err)
	}
	digestTime := strings.TrimSpace(mutation.DigestTime)
	parsed, err := time.Parse("15:04", digestTime)
	if err != nil || parsed.Format("15:04") != digestTime {
		return 0, errors.New("digest_time must be in HH:MM format")
	}
	current := persistedWebAppSettings{
		DigestTime:   digestTime,
		Timezone:     "Europe/Moscow",
		DefaultModel: summarizer.DefaultOpenRouterModel,
	}
	if value, getErr := store.Config.Get("webapp_settings"); getErr == nil {
		if unmarshalErr := json.Unmarshal([]byte(value), &current); unmarshalErr != nil {
			return 0, fmt.Errorf("decode persisted WebApp settings: %w", unmarshalErr)
		}
	} else if !errors.Is(getErr, db.ErrNotFound) {
		return 0, fmt.Errorf("load persisted WebApp settings: %w", getErr)
	}
	if strings.TrimSpace(current.Timezone) == "" {
		current.Timezone = "Europe/Moscow"
	}
	if strings.TrimSpace(current.DefaultModel) == "" {
		current.DefaultModel = summarizer.DefaultOpenRouterModel
	}
	current.DigestTime = digestTime
	if timezone := strings.TrimSpace(mutation.Timezone); timezone != "" {
		current.Timezone = timezone
	}
	if model := strings.TrimSpace(mutation.DefaultModel); model != "" {
		current.DefaultModel = model
	}
	if mutation.Channels != nil {
		current.Channels = append([]string(nil), mutation.Channels...)
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return 0, fmt.Errorf("encode WebApp settings: %w", err)
	}
	version, err := store.Config.SetOptimistic("webapp_settings", string(encoded), mutation.Version)
	if err != nil {
		return 0, fmt.Errorf("persist WebApp settings: %w", err)
	}

	groups, err := store.Groups.List()
	if err != nil {
		return 0, fmt.Errorf("list groups for WebApp settings: %w", err)
	}
	for _, group := range groups {
		groupSettings, err := store.Groups.GetGroupSettings(group.ID)
		if err != nil {
			return 0, fmt.Errorf("load settings for group %d: %w", group.ID, err)
		}
		groupSettings.DigestTime = digestTime
		if timezone := strings.TrimSpace(mutation.Timezone); timezone != "" {
			groupSettings.Timezone = timezone
		} else if strings.TrimSpace(groupSettings.Timezone) == "" {
			groupSettings.Timezone = current.Timezone
		}
		if err := store.Groups.UpdateGroupSettings(groupSettings); err != nil {
			return 0, fmt.Errorf("persist settings for group %d: %w", group.ID, err)
		}
		if group.Status == "" || group.Status == model.GroupStatusActive {
			if err := sched.RefreshGroup(group.ID); err != nil {
				return 0, fmt.Errorf("refresh scheduler for group %d: %w", group.ID, err)
			}
		}
	}
	return version, nil
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
