// Package main is the entry point for the tg-channel-summary-by-ai bot.
// It wires together all components: config, database, bot service, parser,
// summarizer, scheduler, HTTP server, and the embedded WebApp.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/bot"
	"github.com/boss/tg-channel-summary-by-ai/internal/config"
	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/forum"
	"github.com/boss/tg-channel-summary-by-ai/internal/lifecycle"
	applog "github.com/boss/tg-channel-summary-by-ai/internal/log"
	"github.com/boss/tg-channel-summary-by-ai/internal/maintenance"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/boss/tg-channel-summary-by-ai/internal/scheduler"
	"github.com/boss/tg-channel-summary-by-ai/internal/security"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
	"github.com/mymmrac/telego"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var productionSettingsMu sync.Mutex

func main() {
	applog.Info("tg-channel-summary-by-ai starting...")
	if validatorHTTPOnlyEnabled() {
		if err := runValidatorHTTPOnly(); err != nil {
			applog.Fatalf("validator HTTP mode stopped: %v", err)
		}
		return
	}
	// Load configuration from .env or environment variables.
	cfg, err := config.Load()
	if err != nil {
		applog.Fatalf("failed to load config: %v", err)
	}
	// Initialize structured logger with secret redaction.
	applog.SetDefault(applog.NewWithRedactor(os.Stderr, applog.ParseLevel(cfg.LogLevel),
		security.NewRedactor(cfg.BotToken, cfg.OpenRouterKey)))
	// Initialize SQLite so Fly boots against the mounted /data volume.
	store, err := db.OpenWithEncryptionKey(cfg.DBPath, cfg.ProviderKey)
	if err != nil {
		applog.Fatalf("failed to open database at %s: %v", cfg.DBPath, err)
	}
	defer store.Close()
	applog.Info("database opened", "path", cfg.DBPath)
	if err := ensureDefaultAIProvider(store, cfg.OpenRouterKey); err != nil {
		applog.Fatalf("failed to configure default AI provider: %v", err)
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
		applog.Fatalf("failed to configure WebApp authentication: %v", err)
	}
	srv := webapp.NewWithProvidersAuthenticated(store, 10*time.Second, http.DefaultClient, webAppAuth)
	forumFence := &forum.MutationFence{}
	srv.SetForumMutationFence(forumFence)
	revocationHandler := func(err error) {
		applog.Error("Bot token revoked (401 Unauthorized)", "err", err)
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
	sched := scheduler.New(digestService,
		scheduler.WithGroupSource(store.Groups),
		scheduler.WithDigestHistory(store.Digests),
		scheduler.WithDSTSkipNotifier(func(groupID int64, groupTitle, digestTime, timezone, reason string) {
			title := groupTitle
			if strings.TrimSpace(title) == "" {
				title = fmt.Sprintf("группа %d", groupID)
			}
			message := fmt.Sprintf("⚠️ Пропущен дайджест для группы «%s» в %s (%s): %s",
				title, digestTime, timezone, reason)
			if err := ownerNotifier.NotifyOwner(context.Background(), message); err != nil {
				applog.Printf("failed to notify owner about DST skip for group %d: %v", groupID, err)
			}
		}),
	)
	srv.SetGroupScheduler(sched)
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
		applog.Fatalf("failed to configure Telegram bot: %v", err)
	}
	telegramBot.SetTokenRevocationHandler(revocationHandler)
	srv.SetTopicLifecycle(telegramBot)
	srv.SetAvailableGroupDiscovery(telegramBot)
	telegramBot.SetForumMutationFence(forumFence)
	telegramBot.SetForumTopicRegistry(store.ForumTopics)
	if err := telegramBot.ReconcilePendingTopicClosures(context.Background()); err != nil {
		applog.Printf("pending forum topic reconciliation incomplete: %v", err)
	}
	if err := telegramBot.ReconcilePendingTopicCreations(context.Background()); err != nil {
		applog.Printf("pending forum topic creation cleanup incomplete: %v", err)
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
	if err := digestService.ResumePendingAll(); err != nil {
		// Keep the durable checkpoint for the next retry while allowing the
		// rest of the application to start. A transient Telegram outage must
		// not prevent HTTP health checks or the scheduler from coming up.
		applog.Printf("pending digest delivery reconciliation incomplete: %v", err)
	}
	if err := sched.Start(); err != nil {
		applog.Fatalf("failed to start scheduler: %v", err)
	}
	// Run missed-schedule catch-up asynchronously so startup is not blocked
	// while past-due digests are generated. This implements the deterministic
	// "always catch up on missed schedules" behavior required after restart.
	go func() {
		if err := sched.CatchUp(); err != nil {
			applog.Printf("scheduler catch-up incomplete: %v", err)
		}
	}()
	if err := srv.ReconcileGroupScheduler(context.Background()); err != nil {
		applog.Printf("pending WebApp group scheduler reconciliation incomplete: %v", err)
	}
	if err := reconcilePendingSettings(context.Background(), store, sched); err != nil {
		applog.Printf("pending WebApp settings reconciliation incomplete: %v", err)
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
		applog.Info("HTTP server listening", "port", cfg.Port)
		if err := srv.Start(cfg.Port); err != nil {
			applog.Printf("HTTP server error: %v", err)
		}
	}()
	go func() {
		if err := telegramBot.Start(); err != nil {
			applog.Printf("Telegram bot stopped: %v", err)
		}
	}()
	// Wait for a signal or a coordinated terminal transition.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-quit:
		applog.Info("received signal, shutting down", "signal", sig)
	case <-appLifecycle.Done():
		applog.Printf("Application entered bounded terminal state")
	}
	// Graceful shutdown
	srv.Stop()
	maintenanceSvc.Stop()
	sched.Stop()
	telegramBot.Stop()
	applog.Info("Shutdown complete")
}
func validatorHTTPOnlyEnabled() bool {
	return os.Getenv("VALIDATOR_HTTP_ONLY") == "1"
}

// newValidatorHTTPServer wires only the database-backed HTTP/WebApp surface.
// It intentionally does not construct Telegram, parser, scheduler, or
// maintenance services, so browser validation cannot create external traffic.
func newValidatorHTTPServer(cfg *config.Config, store *db.DB) (*webapp.Server, error) {
	if cfg == nil || store == nil {
		return nil, errors.New("validator HTTP mode requires configuration and database")
	}
	auth, err := webapp.NewWebAppAuthWithOrigin(cfg.BotToken, cfg.OwnerTelegramID, cfg.WebAppURL)
	if err != nil {
		return nil, fmt.Errorf("configure validator WebApp authentication: %w", err)
	}
	server := webapp.NewWithProvidersAuthenticated(
		store,
		10*time.Second,
		&http.Client{Transport: validatorHTTPTransport{}},
		auth,
	)
	server.SetChannelVerifier(validatorDisabledChannelVerifier{})
	server.SetGroupVerifier(validatorDisabledGroupVerifier{})
	return server, nil
}
func runValidatorHTTPOnly() error {
	cfg, err := config.LoadValidator()
	if err != nil {
		return err
	}
	runDB, err := newValidatorRunDatabase(cfg.DBPath)
	if err != nil {
		return err
	}
	validatorOwnerKey := cfg.DBPath
	defer func() {
		if cleanupErr := runDB.cleanup(); cleanupErr != nil {
			applog.Printf("validator database cleanup failed: %v", cleanupErr)
		}
	}()
	cfg.DBPath = runDB.path
	listener, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("bind validator HTTP listener on port %s: %w", cfg.Port, err)
	}
	var owner *validatorListenerOwner
	defer func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			applog.Printf("validator listener cleanup failed: %v", closeErr)
		}
		if owner != nil {
			if releaseErr := owner.Release(); releaseErr != nil {
				applog.Printf("validator listener ownership cleanup failed: %v", releaseErr)
			}
		}
	}()
	owner, err = newValidatorListenerOwnerForRun(validatorOwnerKey, cfg.DBPath)
	if err != nil {
		return err
	}
	if err := owner.Claim(); err != nil {
		return err
	}
	store, err := db.OpenWithEncryptionKey(cfg.DBPath, cfg.ProviderKey)
	if err != nil {
		return fmt.Errorf("open validator database at %s: %w", cfg.DBPath, err)
	}
	defer store.Close()
	if err := ensureDefaultAIProvider(store, cfg.OpenRouterKey); err != nil {
		return fmt.Errorf("configure validator AI provider: %w", err)
	}
	if validatorFixtureEnabled() {
		if _, err := seedValidatorBotAdminFixture(store); err != nil {
			return fmt.Errorf("seed validator fixture %s: %w", validatorFixtureProfile, err)
		}
	}
	srv, err := newValidatorHTTPServer(cfg, store)
	if err != nil {
		return err
	}
	if validatorFixtureEnabled() {
		if err := configureValidatorBotAdminFixture(srv, store); err != nil {
			return fmt.Errorf("configure validator fixture %s: %w", validatorFixtureProfile, err)
		}
		if err := srv.SetValidatorBrowserBoundary(os.Getenv(validatorTokenEnv), validatorOwnerInitData()); err != nil {
			return fmt.Errorf("configure validator browser boundary: %w", err)
		}
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Serve(listener)
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)
	select {
	case sig := <-quit:
		applog.Printf("Validator HTTP mode received signal %v, shutting down...", sig)
	case err := <-serverErr:
		srv.Stop()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("validator HTTP server: %w", err)
		}
		return nil
	}
	srv.Stop()
	if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("validator HTTP server shutdown: %w", err)
	}
	return nil
}

// applyProductionSettings is the production update boundary for Telegram
// WebApp sendData. It persists the payload and updates every configured group
// while refreshing the already-running shared scheduler instance.
func applyProductionSettings(ctx context.Context, store *db.DB, sched *scheduler.Scheduler, settings bot.BotSettings) error {
	_, err := applyProductionSettingsMutation(ctx, store, sched, webapp.SettingsMutation{
		DigestTime:   settings.DigestTime,
		Timezone:     settings.Timezone,
		DefaultModel: settings.DefaultModel,
		Channels:     settings.Channels,
		Version:      settings.Version,
	})
	return err
}

type persistedWebAppSettings struct {
	DigestTime   string   `json:"digest_time"`
	Timezone     string   `json:"timezone"`
	DefaultModel string   `json:"default_model"`
	Channels     []string `json:"channels"`
}

const pendingSettingsSyncKey = "webapp_settings_sync_pending"

func reconcilePendingSettings(ctx context.Context, store *db.DB, sched *scheduler.Scheduler) error {
	if store == nil || sched == nil {
		return errors.New("reconcile settings: dependencies are not configured")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reconcile settings: %w", err)
	}
	productionSettingsMu.Lock()
	defer productionSettingsMu.Unlock()
	return sched.WithLifecycle(func() error {
		return reconcilePendingSettingsLocked(ctx, store, sched)
	})
}
func reconcilePendingSettingsLocked(ctx context.Context, store *db.DB, sched *scheduler.Scheduler) error {
	pending, err := store.Config.Get(pendingSettingsSyncKey)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load pending settings sync: %w", err)
	}
	var intent struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal([]byte(pending), &intent); err != nil || intent.Version <= 0 {
		if err == nil {
			err = errors.New("pending settings intent has no positive version")
		}
		return fmt.Errorf("decode pending settings sync: %w", err)
	}
	groups, err := store.Groups.List()
	if err != nil {
		return fmt.Errorf("list groups for pending settings sync: %w", err)
	}
	activeSettings := make(map[int64]*model.GroupSettings, len(groups))
	for _, group := range groups {
		if group.Status != "" && group.Status != model.GroupStatusActive {
			continue
		}
		settings, err := store.Groups.GetGroupSettings(group.ID)
		if err != nil {
			return fmt.Errorf("load group %d for pending settings sync: %w", group.ID, err)
		}
		activeSettings[group.ID] = settings
	}
	plan, err := sched.PrepareSettingsRefresh(activeSettings)
	if err != nil {
		return fmt.Errorf("prepare pending settings sync: %w", err)
	}
	if err := plan.Apply(); err != nil {
		return fmt.Errorf("apply pending settings sync version %d: %w", intent.Version, err)
	}
	if err := store.ClearSettingsSyncPending(pendingSettingsSyncKey); err != nil {
		return fmt.Errorf("clear pending settings sync: %w", err)
	}
	return nil
}
func applyProductionSettingsMutation(ctx context.Context, store *db.DB, sched *scheduler.Scheduler, mutation webapp.SettingsMutation) (int64, error) {
	if store == nil || sched == nil {
		return 0, errors.New("production settings dependencies are not configured")
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("apply production settings: %w", err)
	}
	productionSettingsMu.Lock()
	defer productionSettingsMu.Unlock()
	var version int64
	err := sched.WithLifecycle(func() error {
		var applyErr error
		version, applyErr = applyProductionSettingsMutationLocked(ctx, store, sched, mutation)
		return applyErr
	})
	return version, err
}
func applyProductionSettingsMutationLocked(ctx context.Context, store *db.DB, sched *scheduler.Scheduler, mutation webapp.SettingsMutation) (int64, error) {
	if mutation.Version <= 0 {
		return 0, db.ErrConflict
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
	groups, err := store.Groups.List()
	if err != nil {
		return 0, fmt.Errorf("list groups for WebApp settings: %w", err)
	}
	groupSettingsByID := make(map[int64]*model.GroupSettings, len(groups))
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
		groupSettingsByID[group.ID] = groupSettings
	}
	activeGroupSettings := make(map[int64]*model.GroupSettings, len(groups))
	allGroupSettings := make([]*model.GroupSettings, 0, len(groups))
	for _, group := range groups {
		allGroupSettings = append(allGroupSettings, groupSettingsByID[group.ID])
		if group.Status == "" || group.Status == model.GroupStatusActive {
			activeGroupSettings[group.ID] = groupSettingsByID[group.ID]
		}
	}
	plan, err := sched.PrepareSettingsRefresh(activeGroupSettings)
	if err != nil {
		return 0, fmt.Errorf("prepare scheduler settings refresh: %w", err)
	}
	pendingValue, err := json.Marshal(struct {
		Version      int64                          `json:"version"`
		DigestTime   string                         `json:"digest_time"`
		Timezone     string                         `json:"timezone"`
		DefaultModel string                         `json:"default_model"`
		Groups       map[int64]*model.GroupSettings `json:"groups"`
	}{
		Version:      mutation.Version + 1,
		DigestTime:   current.DigestTime,
		Timezone:     current.Timezone,
		DefaultModel: current.DefaultModel,
		Groups:       groupSettingsByID,
	})
	if err != nil {
		return 0, fmt.Errorf("encode scheduler settings intent: %w", err)
	}
	version, err := store.ApplySettingsTransaction(db.SettingsUpdate{
		ConfigKey:       "webapp_settings",
		ConfigValue:     string(encoded),
		ExpectedVersion: mutation.Version,
		GroupSettings:   allGroupSettings,
		PendingKey:      pendingSettingsSyncKey,
		PendingValue:    string(pendingValue),
	})
	if err != nil {
		return 0, fmt.Errorf("persist WebApp settings transaction: %w", err)
	}
	if err := plan.Apply(); err != nil {
		// The committed intent remains durable and is reconciled on restart.
		return 0, fmt.Errorf("apply scheduler settings refresh: %w", err)
	}
	if err := store.ClearSettingsSyncPending(pendingSettingsSyncKey); err != nil {
		// The database and scheduler are already converged. Keeping the intent
		// makes this cleanup failure safely retryable after restart.
		return version, fmt.Errorf("clear scheduler settings intent: %w", err)
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
