// Package bot provides the Telegram bot service using the telego library.
// It handles long polling for updates, command routing, callback queries,
// and sending messages to groups and users.
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/telegram"
	"github.com/mymmrac/telego"
)

var ErrTokenRevoked = telegram.ErrTokenRevoked

type logger interface {
	Printf(format string, args ...any)
}

type telegramClient interface {
	GetMe(context.Context) (*telego.User, error)
	GetChatMember(context.Context, *telego.GetChatMemberParams) (telego.ChatMember, error)
	SetMyCommands(context.Context, *telego.SetMyCommandsParams) error
	AnswerCallbackQuery(context.Context, *telego.AnswerCallbackQueryParams) error
	SendMessage(context.Context, *telego.SendMessageParams) (*telego.Message, error)
	GetChat(context.Context, *telego.GetChatParams) (*telego.ChatFullInfo, error)
	CreateForumTopic(context.Context, *telego.CreateForumTopicParams) (*telego.ForumTopic, error)
	CloseForumTopic(context.Context, *telego.CloseForumTopicParams) error
	DeleteForumTopic(context.Context, *telego.DeleteForumTopicParams) error
	EditForumTopic(context.Context, *telego.EditForumTopicParams) error
}

var ErrTopicPermissionDenied = errors.New("bot lacks can_manage_topics permission")

type updatePoller interface {
	UpdatesViaLongPolling(context.Context, *telego.GetUpdatesParams, ...telego.LongPollingOption) (<-chan telego.Update, error)
}

type ownerNotifier interface {
	NotifyOwner(context.Context, string) error
}

type forumTopicRegistry interface {
	Observe(int64, int64, string) error
	PersistOwned(int64, int64, string) error
	Get(int64, int64) (*model.ForumTopic, error)
	MarkEdited(int64, int64, string) error
	MarkClosed(int64, int64) error
	MarkReopened(int64, int64) error
	DeleteOwned(int64, int64) error
}

type forumTopicCloseCoordinator interface {
	BeginClose(int64, int64) error
	ListPending() ([]model.ForumTopic, error)
}

// GroupLifecycle receives scheduler lifecycle events without coupling the bot
// package to the scheduler package.
type GroupLifecycle interface {
	RemoveGroup(groupID int64)
}

type groupRestorer interface {
	RestoreGroup(groupID int64) error
}

// CommandHandler handles a normalized bot command and its optional argument.
type CommandHandler func(context.Context, *telego.Message, string) error

// Service represents the Telegram bot service.
type Service struct {
	api            telegramClient
	poller         updatePoller
	groups         *db.GroupRepository
	channels       *db.ChannelRepository
	notifier       ownerNotifier
	lifecycle      GroupLifecycle
	logger         logger
	ownerID        string
	botName        string
	webAppURL      string
	topicRegistry  forumTopicRegistry
	commands       map[string]CommandHandler
	applyData      func(context.Context, *telego.Message, BotSettings) error
	onTokenRevoked func(error)

	ctx     context.Context
	cancel  context.CancelFunc
	stopMu  sync.Mutex
	topicMu sync.Mutex
}

// New creates an unconfigured service. Use NewWithConfig for production.
func New() *Service {
	service := &Service{
		logger:   log.Default(),
		commands: make(map[string]CommandHandler),
	}
	service.configureAdminCommands()
	return service
}

// NewWithConfig creates a Telegram service backed by telego long polling.
func NewWithConfig(token, ownerID, webAppURL string, groups *db.GroupRepository, channels *db.ChannelRepository, notifier ownerNotifier, lifecycle GroupLifecycle) (*Service, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("bot token is required")
	}
	if strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("owner telegram id is required")
	}
	if parsedOwnerID, err := strconv.ParseInt(strings.TrimSpace(ownerID), 10, 64); err != nil || parsedOwnerID <= 0 {
		return nil, errors.New("owner telegram id must be a positive integer")
	}
	client, err := telego.NewBot(token, telego.WithDefaultLogger(false, false))
	if err != nil {
		return nil, fmt.Errorf("create Telegram bot: %w", err)
	}
	service := New()
	service.api = client
	service.poller = client
	service.groups = groups
	service.channels = channels
	service.notifier = notifier
	service.lifecycle = lifecycle
	service.ownerID = strings.TrimSpace(ownerID)
	service.botName = strings.TrimPrefix(client.Username(), "@")
	service.webAppURL = strings.TrimSpace(webAppURL)
	service.configureAdminCommands()
	return service, nil
}

// SetCommandHandler registers a handler for a normalized command.
func (s *Service) SetCommandHandler(command string, handler CommandHandler) {
	if s == nil {
		return
	}
	if s.commands == nil {
		s.commands = make(map[string]CommandHandler)
	}
	command = strings.ToLower(strings.TrimSpace(command))
	command = strings.TrimPrefix(command, "/")
	if command == "" || handler == nil {
		return
	}
	// All commands registered by this service are administrative operations.
	// Guard them at the dispatch boundary so a future handler cannot
	// accidentally expose a sensitive action without the owner check.
	s.commands[command] = func(ctx context.Context, message *telego.Message, argument string) error {
		if message == nil || message.From == nil || message.From.ID <= 0 {
			return nil
		}
		if !s.isConfigured() {
			return s.sendPlain(ctx, message.Chat.ChatID(), "Bot is not configured.")
		}
		if !s.isOwner(message.From.ID) {
			return s.sendPlain(ctx, message.Chat.ChatID(), "Access denied.")
		}
		return handler(ctx, message, argument)
	}
}

// SetSettingsApplier configures the persistence callback for WebApp data.
func (s *Service) SetSettingsApplier(applier func(context.Context, *telego.Message, BotSettings) error) {
	if s != nil {
		s.applyData = applier
	}
}

// SetTokenRevocationHandler connects every Telegram 401 boundary to the
// application lifecycle supervisor.
func (s *Service) SetTokenRevocationHandler(handler func(error)) {
	if s != nil {
		s.onTokenRevoked = handler
	}
}

// SetForumTopicRegistry connects Telegram lifecycle/discovery updates to the
// durable registry used by the WebApp catalog.
func (s *Service) SetForumTopicRegistry(registry forumTopicRegistry) {
	if s != nil {
		s.topicRegistry = registry
	}
}

// Start begins long polling for updates.
func (s *Service) Start() error {
	if s == nil || s.api == nil || s.poller == nil {
		return errors.New("bot service is not configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stopMu.Lock()
	s.ctx = ctx
	s.cancel = cancel
	s.stopMu.Unlock()
	defer cancel()

	me, err := s.api.GetMe(ctx)
	if err != nil {
		if classified := s.classifyTelegramError(err); classified != nil {
			return fmt.Errorf("getMe: %w", classified)
		}
		return fmt.Errorf("verify bot identity with getMe: %w", err)
	}
	if me == nil || me.ID == 0 || strings.TrimSpace(me.Username) == "" {
		return errors.New("verify bot identity with getMe: incomplete identity")
	}
	s.botName = strings.TrimPrefix(strings.ToLower(me.Username), "@")
	s.logf("Bot identity verified: @%s (ID: %d)", me.Username, me.ID)
	if err := s.registerCommands(ctx); err != nil {
		return fmt.Errorf("register bot commands: %w", err)
	}

	updates, err := s.poller.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		Limit:          100,
		Timeout:        30,
		AllowedUpdates: []string{"message", "callback_query", "my_chat_member"},
	}, telego.WithLongPollingRetryTimeout(0))
	if err != nil {
		if classified := s.classifyTelegramError(err); classified != nil {
			return fmt.Errorf("start long polling: %w", classified)
		}
		return fmt.Errorf("start long polling: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-updates:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				// telego closes the update channel when long polling stops,
				// including after an API error. Probe the token once so a
				// post-start 401 is not mistaken for a clean shutdown.
				if _, probeErr := s.api.GetMe(ctx); probeErr != nil {
					if classified := s.classifyTelegramError(probeErr); classified != nil {
						return fmt.Errorf("long polling stopped: %w", classified)
					}
					return fmt.Errorf("long polling stopped: verify bot identity: %w", probeErr)
				}
				return nil
			}
			if err := s.HandleUpdate(ctx, &update); err != nil {
				if errors.Is(err, ErrTokenRevoked) {
					return err
				}
				s.logf("update %d failed: %v", update.UpdateID, err)
			}
		}
	}
}

// Stop gracefully shuts down the bot service.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.stopMu.Lock()
	cancel := s.cancel
	s.stopMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) registerCommands(ctx context.Context) error {
	if s == nil || s.api == nil {
		return errors.New("bot service is not configured")
	}
	commands := []telego.BotCommand{
		{Command: "start", Description: "Start the bot"},
		{Command: "settings", Description: "Configure bot settings"},
	}
	scope := &telego.BotCommandScopeAllPrivateChats{Type: "all_private_chats"}
	if err := s.api.SetMyCommands(ctx, &telego.SetMyCommandsParams{Commands: commands, Scope: scope}); err != nil {
		return s.classifyTelegramError(err)
	}
	s.logf("Registered %d bot commands: start, settings", len(commands))
	return nil
}

// HandleUpdate dispatches one Telegram update. It is exported for deterministic
// webhook-free tests and for alternate runners.
func (s *Service) HandleUpdate(ctx context.Context, update *telego.Update) error {
	if s == nil || update == nil {
		return nil
	}
	if update.CallbackQuery != nil {
		query := update.CallbackQuery
		// Answer before dispatching any action so Telegram always dismisses the
		// client-side loading spinner, including unknown or failed callbacks.
		if s.api != nil {
			if err := s.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: query.ID}); err != nil {
				s.logf("answer callback query: %v", err)
				return s.classifyTelegramError(err)
			}
		}
		if query.From.ID <= 0 || !s.isConfigured() || !s.isOwner(query.From.ID) {
			if query.Message != nil {
				if message := query.Message.Message(); message != nil && query.From.ID > 0 {
					return s.sendPlain(ctx, message.Chat.ChatID(), s.accessDeniedMessage())
				}
			}
			return nil
		}
		if handler, ok := s.commands[strings.ToLower(strings.TrimSpace(query.Data))]; ok {
			if query.Message != nil {
				if message := query.Message.Message(); message != nil {
					// The message attached to a callback is the bot's
					// original message, so its From field is not the user
					// who pressed the button. Preserve the chat and content
					// while supplying the authenticated callback sender to
					// the centralized command guard.
					callbackMessage := *message
					callbackUser := query.From
					callbackMessage.From = &callbackUser
					return handler(ctx, &callbackMessage, "")
				}
			}
		}
		return nil
	}
	if update.MyChatMember != nil {
		return s.handleMyChatMember(ctx, update.MyChatMember)
	}
	if update.Message != nil {
		return s.handleMessage(ctx, update.Message)
	}
	return nil
}

func (s *Service) handleMessage(ctx context.Context, message *telego.Message) error {
	if message == nil {
		return nil
	}
	if err := s.observeForumTopicMessage(message); err != nil {
		return err
	}
	if message.WebAppData != nil {
		return s.handleWebAppData(ctx, message)
	}
	command, argument, ok := ParseCommand(message.Text, s.botName)
	if !ok {
		return nil
	}
	s.logf("Parsed command: %s", command)
	handler, ok := s.commands[command]
	if !ok {
		return nil
	}
	return handler(ctx, message, argument)
}

func (s *Service) observeForumTopicMessage(message *telego.Message) error {
	if s == nil || s.topicRegistry == nil || s.groups == nil || message == nil ||
		message.Chat.Type != telego.ChatTypeSupergroup {
		return nil
	}
	threadID := int64(message.MessageThreadID)
	if threadID <= 0 {
		return nil
	}
	group, err := s.groups.GetByChatID(message.Chat.ID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find forum topic group %d: %w", message.Chat.ID, err)
	}
	if group.Status != "" && group.Status != model.GroupStatusActive {
		return nil
	}
	switch {
	case message.ForumTopicCreated != nil:
		name := strings.TrimSpace(message.ForumTopicCreated.Name)
		if name == "" {
			return nil
		}
		return s.topicRegistry.Observe(group.ID, threadID, name)
	case message.ForumTopicEdited != nil:
		if strings.TrimSpace(message.ForumTopicEdited.Name) == "" {
			return nil
		}
		if err := s.topicRegistry.MarkEdited(group.ID, threadID, message.ForumTopicEdited.Name); err != nil &&
			!errors.Is(err, db.ErrNotFound) {
			return err
		}
	case message.ForumTopicClosed != nil:
		if err := s.topicRegistry.MarkClosed(group.ID, threadID); err != nil &&
			!errors.Is(err, db.ErrNotFound) {
			return err
		}
	case message.ForumTopicReopened != nil:
		if err := s.topicRegistry.MarkReopened(group.ID, threadID); err != nil &&
			!errors.Is(err, db.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (s *Service) configureAdminCommands() {
	s.SetCommandHandler("start", s.handleStart)
	s.SetCommandHandler("settings", s.handleSettings)
}

func (s *Service) handleStart(ctx context.Context, message *telego.Message, _ string) error {
	botName := strings.TrimSpace(s.botName)
	if botName == "" {
		botName = "tgaidigestbot"
	}
	greeting := "Welcome"
	if message != nil && message.From != nil && strings.TrimSpace(message.From.FirstName) != "" {
		greeting += ", " + escapeMarkdownV2(message.From.FirstName)
	}
	text := fmt.Sprintf(
		"%s to *%s*\\. This bot summarizes Telegram channels and creates daily digests\\.",
		greeting,
		escapeMarkdownV2("@"+botName),
	)
	return s.sendAdminWebAppMessage(ctx, message.Chat.ChatID(), text, true)
}

func (s *Service) handleSettings(ctx context.Context, message *telego.Message, _ string) error {
	return s.sendAdminWebAppMessage(ctx, message.Chat.ChatID(), "Open settings", false)
}

func (s *Service) sendAdminWebAppMessage(ctx context.Context, chatID telego.ChatID, text string, markdown bool) error {
	if s == nil || s.api == nil {
		return errors.New("bot service is not configured")
	}
	if !isHTTPSWebAppURL(s.webAppURL) {
		return s.sendPlain(ctx, chatID, "Bot is not configured.")
	}
	params := &telego.SendMessageParams{
		ChatID: chatID,
		Text:   text,
		ReplyMarkup: (&telego.InlineKeyboardMarkup{}).WithInlineKeyboard([]telego.InlineKeyboardButton{
			{Text: "Open Settings", WebApp: &telego.WebAppInfo{URL: s.webAppURL}},
		}),
	}
	if markdown {
		params.ParseMode = "MarkdownV2"
	}
	_, err := s.api.SendMessage(ctx, params)
	return s.classifyTelegramError(err)
}

func (s *Service) isConfigured() bool {
	if s == nil {
		return false
	}
	ownerID, err := strconv.ParseInt(strings.TrimSpace(s.ownerID), 10, 64)
	return err == nil && ownerID > 0
}

func (s *Service) isOwner(userID int64) bool {
	if s == nil || userID <= 0 {
		return false
	}
	ownerID, err := strconv.ParseInt(strings.TrimSpace(s.ownerID), 10, 64)
	return err == nil && ownerID > 0 && ownerID == userID
}

func (s *Service) accessDeniedMessage() string {
	if !s.isConfigured() {
		return "Bot is not configured."
	}
	return "Access denied."
}

func escapeMarkdownV2(value string) string {
	const special = `_*[]()~` + "`" + `>#+-=|{}.!`
	var escaped strings.Builder
	escaped.Grow(len(value))
	for _, char := range value {
		if strings.ContainsRune(special, char) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(char)
	}
	return escaped.String()
}

func isHTTPSWebAppURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}

// ParseCommand extracts a lower-case command and argument. A command addressed
// to another bot is ignored, while an addressed command for this bot has its
// @username suffix removed.
func ParseCommand(text, botName string) (command, argument string, ok bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", "", false
	}
	raw := strings.TrimPrefix(fields[0], "/")
	if raw == "" {
		return "", "", false
	}
	parts := strings.SplitN(raw, "@", 2)
	if len(parts) == 2 {
		if strings.TrimSpace(botName) == "" || !strings.EqualFold(parts[1], strings.TrimPrefix(botName, "@")) {
			return "", "", false
		}
	}
	command = strings.ToLower(parts[0])
	if len(fields) > 1 {
		argument = strings.Join(fields[1:], " ")
	}
	return command, argument, true
}

// BotSettings is the safe subset accepted from Telegram WebApp sendData.
type BotSettings struct {
	DigestTime   string   `json:"digest_time"`
	Timezone     string   `json:"timezone"`
	DefaultModel string   `json:"default_model"`
	Channels     []string `json:"channels"`
	Version      int64    `json:"version"`
}

func (s *Service) handleWebAppData(ctx context.Context, message *telego.Message) error {
	if message.From == nil || message.From.ID == 0 {
		return nil
	}
	if !s.isConfigured() {
		return s.sendPlain(ctx, message.Chat.ChatID(), "Bot is not configured.")
	}
	if !s.isOwner(message.From.ID) {
		return s.sendPlain(ctx, message.Chat.ChatID(), "Access denied.")
	}
	var settings BotSettings
	if err := decodeAndValidateSettings(message.WebAppData.Data, &settings); err != nil {
		return s.sendPlain(ctx, message.Chat.ChatID(), "Invalid configuration: "+err.Error())
	}
	if s.applyData != nil {
		if err := s.applyData(ctx, message, settings); err != nil {
			return s.sendPlain(ctx, message.Chat.ChatID(), "Unable to update settings: "+err.Error())
		}
	}
	return s.sendPlain(ctx, message.Chat.ChatID(), "Settings updated successfully.")
}

func decodeAndValidateSettings(data string, settings *BotSettings) error {
	if err := jsonUnmarshal([]byte(data), settings); err != nil {
		return errors.New("invalid JSON")
	}
	if settings.DigestTime == "" {
		return errors.New("digest_time is required")
	}
	if _, err := time.Parse("15:04", settings.DigestTime); err != nil {
		return errors.New("digest_time must be in HH:MM format")
	}
	if settings.Channels == nil {
		return errors.New("channels is required")
	}
	if settings.Version <= 0 {
		return errors.New("version must be a positive current settings version")
	}
	return nil
}

func (s *Service) sendPlain(ctx context.Context, chatID telego.ChatID, text string) error {
	if s == nil || s.api == nil {
		return errors.New("bot service is not configured")
	}
	_, err := s.api.SendMessage(ctx, &telego.SendMessageParams{ChatID: chatID, Text: text})
	return s.classifyTelegramError(err)
}

// Deliver sends one assembled digest to its configured Telegram group and
// returns the message metadata needed by the WebApp result contract.
func (s *Service) Deliver(ctx context.Context, groupID int64, result *digest.Digest) (digest.DeliveryReceipt, error) {
	if s == nil || s.api == nil || s.groups == nil {
		return digest.DeliveryReceipt{}, errors.New("Telegram delivery is not configured")
	}
	if result == nil || strings.TrimSpace(result.Text) == "" {
		return digest.DeliveryReceipt{}, errors.New("digest message is empty")
	}
	group, err := s.groups.GetByID(groupID)
	if err != nil {
		return digest.DeliveryReceipt{}, fmt.Errorf("load digest group %d: %w", groupID, err)
	}
	params := &telego.SendMessageParams{
		ChatID: groupTelegramChatID(group.TelegramChatID),
		Text:   result.Text,
	}
	assignments, err := s.groups.GetChannelAssignments(groupID)
	if err != nil {
		return digest.DeliveryReceipt{}, fmt.Errorf("load digest topics for group %d: %w", groupID, err)
	}
	for _, assignment := range assignments {
		if assignment.TopicThreadID != nil {
			params.MessageThreadID = int(*assignment.TopicThreadID)
			break
		}
	}
	message, err := s.api.SendMessage(ctx, params)
	if err != nil {
		return digest.DeliveryReceipt{}, fmt.Errorf("send digest to group %d: %w", groupID, s.classifyTelegramError(err))
	}
	if message == nil || message.MessageID == 0 {
		return digest.DeliveryReceipt{}, errors.New("Telegram delivery returned no message metadata")
	}
	return digest.DeliveryReceipt{MessageID: int64(message.MessageID)}, nil
}

func (s *Service) handleMyChatMember(ctx context.Context, update *telego.ChatMemberUpdated) error {
	if update == nil || update.NewChatMember == nil {
		return nil
	}
	if update.Chat.Type != "group" && update.Chat.Type != "supergroup" {
		return nil
	}
	if s.groups == nil {
		return errors.New("group repository is not configured")
	}
	status := update.NewChatMember.MemberStatus()
	switch status {
	case "left", "kicked":
		group, err := s.groups.GetByChatID(update.Chat.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("find removed group %d: %w", update.Chat.ID, err)
		}
		if err := s.groups.SetStatus(group.ID, model.GroupStatusInactive); err != nil {
			return fmt.Errorf("mark group %d inactive: %w", group.ID, err)
		}
		if s.lifecycle != nil {
			s.lifecycle.RemoveGroup(group.ID)
		}
		if s.notifier != nil {
			notice := fmt.Sprintf("Bot was removed from group %q (%d). The group has been marked inactive.", group.Title, group.TelegramChatID)
			if err := s.notifier.NotifyOwner(ctx, notice); err != nil {
				s.logf("notify owner about removed group %d: %v", group.ID, err)
			}
		}
		return nil
	case "member", "administrator":
		return s.handleGroupJoin(ctx, update.Chat)
	default:
		return nil
	}
}

func (s *Service) handleGroupJoin(ctx context.Context, chat telego.Chat) error {
	if s == nil || s.api == nil {
		return errors.New("bot service is not configured")
	}
	if s.groups == nil {
		return errors.New("group repository is not configured")
	}
	fullChat, err := s.api.GetChat(ctx, &telego.GetChatParams{ChatID: chat.ChatID()})
	if err != nil {
		return fmt.Errorf("get chat %d: %w", chat.ID, s.classifyTelegramError(err))
	}
	chat.IsForum = fullChat != nil && fullChat.IsForum
	if !chat.IsForum {
		group, err := s.upsertGroup(chat, model.GroupStatusIneligible)
		if err != nil {
			return err
		}
		if s.lifecycle != nil {
			s.lifecycle.RemoveGroup(group.ID)
		}
		_, sendErr := s.api.SendMessage(ctx, &telego.SendMessageParams{
			ChatID: chat.ChatID(),
			Text:   "This bot requires a forum supergroup with topics enabled. Please convert the group to a forum or create a new forum group.",
		})
		if sendErr != nil {
			// The group has already been persisted as ineligible and its
			// scheduler job removed. A transient warning-delivery failure
			// must not make cleanup unreliable or cause repeated lifecycle
			// processing to leave a stale active job behind.
			classifiedSendErr := s.classifyTelegramError(sendErr)
			if errors.Is(classifiedSendErr, ErrTokenRevoked) {
				return classifiedSendErr
			}
			s.logf("send forum requirement for group %d: %v", chat.ID, classifiedSendErr)
		}
		return nil
	}
	group, err := s.upsertGroup(chat, model.GroupStatusActive)
	if err != nil {
		return err
	}
	if restorer, ok := s.lifecycle.(groupRestorer); ok {
		if err := restorer.RestoreGroup(group.ID); err != nil {
			return fmt.Errorf("restore scheduler for group %d: %w", group.ID, err)
		}
	}
	return err
}

func (s *Service) upsertGroup(chat telego.Chat, status string) (*model.Group, error) {
	group, err := s.groups.GetByChatID(chat.ID)
	if errors.Is(err, db.ErrNotFound) {
		group = &model.Group{TelegramChatID: chat.ID, Title: chat.Title, Status: status}
		id, insertErr := s.groups.Insert(group)
		if insertErr != nil {
			return nil, fmt.Errorf("insert group %d: %w", chat.ID, insertErr)
		}
		group.ID = id
		return group, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find group %d: %w", chat.ID, err)
	}
	group.Title = chat.Title
	group.Status = status
	if err := s.groups.Update(group); err != nil {
		return nil, fmt.Errorf("update group %d: %w", chat.ID, err)
	}
	return group, nil
}

// CreateChannelTopic creates and persists a topic for an existing assignment.
func (s *Service) CreateChannelTopic(ctx context.Context, groupID, channelID int64) error {
	if s == nil || s.api == nil {
		return errors.New("bot service is not configured")
	}
	if s.groups == nil {
		return errors.New("group repository is not configured")
	}
	group, err := s.groups.GetByID(groupID)
	if err != nil {
		return fmt.Errorf("load group: %w", err)
	}
	if group.Status != "" && group.Status != model.GroupStatusActive {
		return fmt.Errorf("group %d is not eligible for forum topics", groupID)
	}
	assignments, err := s.groups.GetChannelAssignments(groupID)
	if err != nil {
		return fmt.Errorf("load channel assignment: %w", err)
	}
	found := false
	for _, assignment := range assignments {
		if assignment.ChannelID == channelID {
			found = true
			if assignment.TopicThreadID != nil {
				return nil
			}
			break
		}
	}
	if !found {
		return db.ErrNotFound
	}
	if s.channels == nil {
		return errors.New("channel repository is not configured")
	}
	channel, err := s.channels.GetByID(channelID)
	if err != nil {
		return fmt.Errorf("load channel: %w", err)
	}
	name := channel.Title
	if strings.TrimSpace(name) == "" {
		name = "@" + channel.Username
	}
	name = truncateRunes(name, 128)
	if err := s.ensureTopicPermission(ctx, group.TelegramChatID); err != nil {
		return fmt.Errorf("check topic permission before create: %w", err)
	}
	topic, err := s.api.CreateForumTopic(ctx, &telego.CreateForumTopicParams{
		ChatID: groupTelegramChatID(group.TelegramChatID),
		Name:   name,
	})
	if err != nil {
		return fmt.Errorf("create topic for channel %d: %w", channelID, s.classifyTelegramError(err))
	}
	if topic == nil || topic.MessageThreadID <= 0 {
		return errors.New("create topic returned an invalid message thread id")
	}
	if err := s.groups.UpdateChannelTopic(groupID, channelID, int64(topic.MessageThreadID)); err != nil {
		if permissionErr := s.ensureTopicPermission(ctx, group.TelegramChatID); permissionErr != nil {
			return fmt.Errorf("check topic permission before delete compensation: %w", permissionErr)
		}
		cleanupErr := s.api.DeleteForumTopic(ctx, &telego.DeleteForumTopicParams{
			ChatID:          groupTelegramChatID(group.TelegramChatID),
			MessageThreadID: int(topic.MessageThreadID),
		})
		if cleanupErr != nil {
			classifiedCleanupErr := s.classifyTelegramError(cleanupErr)
			return fmt.Errorf("persist topic for channel %d: %w; rollback topic: %w", channelID, err, classifiedCleanupErr)
		}
		return fmt.Errorf("persist topic for channel %d: %w", channelID, err)
	}
	if s.topicRegistry != nil {
		if err := s.topicRegistry.PersistOwned(groupID, int64(topic.MessageThreadID), name); err != nil {
			if permissionErr := s.ensureTopicPermission(ctx, group.TelegramChatID); permissionErr != nil {
				return fmt.Errorf("check topic permission before delete compensation: %w", permissionErr)
			}
			rollbackErr := s.groups.UnassignChannel(groupID, channelID)
			cleanupErr := s.api.DeleteForumTopic(ctx, &telego.DeleteForumTopicParams{
				ChatID:          groupTelegramChatID(group.TelegramChatID),
				MessageThreadID: int(topic.MessageThreadID),
			})
			if rollbackErr != nil {
				return fmt.Errorf("persist topic registry for channel %d: %w; rollback assignment: %v", channelID, err, rollbackErr)
			}
			if cleanupErr != nil {
				return fmt.Errorf("persist topic registry for channel %d: %w; rollback topic: %v", channelID, err, s.classifyTelegramError(cleanupErr))
			}
			return fmt.Errorf("persist topic registry for channel %d: %w", channelID, err)
		}
	}
	return nil
}

// RenameChannelTopic updates the Telegram topic name for an assignment.
func (s *Service) RenameChannelTopic(ctx context.Context, groupID, channelID int64, name string) error {
	if s == nil || s.api == nil {
		return errors.New("bot service is not configured")
	}
	if s.groups == nil {
		return errors.New("group repository is not configured")
	}
	threadID, chatID, err := s.topicAssignment(groupID, channelID)
	if err != nil {
		return err
	}
	name = truncateRunes(strings.TrimSpace(name), 128)
	if name == "" {
		return errors.New("topic name is required")
	}
	if err := s.ensureTopicPermission(ctx, chatID); err != nil {
		return fmt.Errorf("check topic permission before rename: %w", err)
	}
	if err := s.api.EditForumTopic(ctx, &telego.EditForumTopicParams{
		ChatID:          groupTelegramChatID(chatID),
		MessageThreadID: int(threadID),
		Name:            name,
	}); err != nil {
		return fmt.Errorf("rename topic: %w", s.classifyTelegramError(err))
	}
	return nil
}

type versionedAssignmentRepository interface {
	UnassignChannelOptimistic(int64, int64, int64) (int64, error)
}

// RemoveChannelTopic removes an assignment and closes the Telegram topic only
// when the durable registry proves that this bot created it.
func (s *Service) RemoveChannelTopic(ctx context.Context, groupID, channelID int64) error {
	_, err := s.removeChannelTopic(ctx, groupID, channelID, 0)
	return err
}

// RemoveChannelTopicWithVersion removes an assignment only when the supplied
// group aggregate version is current. The versioned check happens before any
// permission or Telegram lifecycle call.
func (s *Service) RemoveChannelTopicWithVersion(ctx context.Context, groupID, channelID, expectedVersion int64) (int64, error) {
	if expectedVersion <= 0 {
		return 0, db.ErrConflict
	}
	return s.removeChannelTopic(ctx, groupID, channelID, expectedVersion)
}

func (s *Service) removeChannelTopic(ctx context.Context, groupID, channelID, expectedVersion int64) (int64, error) {
	if s == nil || s.api == nil {
		return 0, errors.New("bot service is not configured")
	}
	if s.groups == nil {
		return 0, errors.New("group repository is not configured")
	}
	group, err := s.groups.GetByID(groupID)
	if err != nil {
		return 0, fmt.Errorf("load group: %w", err)
	}
	if expectedVersion > 0 && group.Version != expectedVersion {
		return 0, db.ErrConflict
	}
	if err := s.ensureTopicPermission(ctx, group.TelegramChatID); err != nil {
		return 0, fmt.Errorf("check topic permission before removal: %w", err)
	}
	s.topicMu.Lock()
	defer s.topicMu.Unlock()
	unassign := func() (int64, error) {
		if expectedVersion <= 0 {
			if err := s.groups.UnassignChannel(groupID, channelID); err != nil {
				return 0, err
			}
			return 0, nil
		}
		repository, ok := interface{}(s.groups).(versionedAssignmentRepository)
		if !ok {
			return 0, errors.New("versioned assignment repository is not configured")
		}
		return repository.UnassignChannelOptimistic(groupID, channelID, expectedVersion)
	}
	threadID, chatID, err := s.topicAssignment(groupID, channelID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return unassign()
		}
		return 0, err
	}
	var registeredTopic *model.ForumTopic
	if s.topicRegistry != nil {
		topic, registryErr := s.topicRegistry.Get(groupID, threadID)
		if errors.Is(registryErr, db.ErrNotFound) {
			return unassign()
		}
		if registryErr != nil {
			return 0, fmt.Errorf("load forum topic ownership: %w", registryErr)
		}
		if !topic.LifecycleOwned {
			return unassign()
		}
		registeredTopic = topic
	}
	assignments, err := s.groups.GetChannelAssignments(groupID)
	if err != nil {
		return 0, fmt.Errorf("load topic assignments: %w", err)
	}
	shared := hasOtherTopicAssignment(assignments, channelID, threadID)
	closeCoordinator, coordinated := s.topicRegistry.(forumTopicCloseCoordinator)
	if registeredTopic != nil && registeredTopic.Closed {
		return unassign()
	}
	if registeredTopic == nil || registeredTopic.LifecycleOwned {
		if err := s.ensureTopicPermission(ctx, chatID); err != nil {
			return 0, fmt.Errorf("check topic permission before close: %w", err)
		}
	}
	// The initial shared-assignment check deliberately happens before close
	// intent. A shared removal still continues through the guarded close path
	// after unassignment, so the final removal can close an otherwise-unused
	// lifecycle-owned topic without closing a topic that still has a survivor.
	if shared {
		if _, err := unassign(); err != nil {
			return 0, fmt.Errorf("remove shared topic assignment: %w", err)
		}
	} else {
		// Remove the assignment before recording close intent. This keeps an
		// assignment-persistence failure from creating a pending close that
		// still points at the topic. BeginClose below atomically verifies that
		// no other assignment appeared while this removal was in flight.
		if _, err := unassign(); err != nil {
			return 0, fmt.Errorf("remove topic assignment: %w", err)
		}
	}
	if coordinated {
		if err := closeCoordinator.BeginClose(groupID, threadID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				if expectedVersion > 0 {
					return expectedVersion + 1, nil
				}
				return 0, nil
			}
			return 0, fmt.Errorf("record topic close intent: %w", err)
		}
	}
	// Re-read group_channels immediately before the irreversible Telegram
	// close. A concurrent assignment, or an assignment whose persistence
	// failed during the removal, must cancel this pending close instead of
	// allowing recovery to close a still-referenced topic.
	assignments, err = s.groups.GetChannelAssignments(groupID)
	if err != nil {
		return 0, fmt.Errorf("recheck topic assignments before close: %w", err)
	}
	if hasTopicAssignment(assignments, threadID) {
		if coordinated {
			if err := s.topicRegistry.MarkReopened(groupID, threadID); err != nil &&
				!errors.Is(err, db.ErrNotFound) {
				return 0, fmt.Errorf("cancel shared topic close: %w", err)
			}
		}
		if expectedVersion > 0 {
			return expectedVersion + 1, nil
		}
		return 0, nil
	}
	if err := s.ensureTopicPermission(ctx, chatID); err != nil {
		return 0, fmt.Errorf("check topic permission before close: %w", err)
	}
	if err := s.api.CloseForumTopic(ctx, &telego.CloseForumTopicParams{
		MessageThreadID: int(threadID),
	}); err != nil {
		if coordinated {
			return 0, fmt.Errorf("close topic: %w; durable close intent remains pending", s.classifyTelegramError(err))
		}
		rollbackErr := s.groups.AssignChannel(groupID, channelID, &threadID)
		if rollbackErr != nil {
			return 0, fmt.Errorf("close topic: %w; rollback topic assignment: %v", s.classifyTelegramError(err), rollbackErr)
		}
		return 0, fmt.Errorf("close topic: %w", s.classifyTelegramError(err))
	}
	if s.topicRegistry != nil {
		if err := s.topicRegistry.MarkClosed(groupID, threadID); err != nil &&
			!errors.Is(err, db.ErrNotFound) {
			if coordinated {
				return 0, fmt.Errorf("persist closed topic state: %w; durable close intent remains pending", err)
			}
			rollbackErr := s.groups.AssignChannel(groupID, channelID, &threadID)
			if rollbackErr != nil {
				return 0, fmt.Errorf("persist closed topic state: %w; rollback topic assignment: %v", err, rollbackErr)
			}
			return 0, fmt.Errorf("persist closed topic state: %w", err)
		}
	}
	if expectedVersion > 0 {
		return expectedVersion + 1, nil
	}
	return 0, nil
}

// ReconcilePendingTopicClosures retries durable close intents once during
// startup. Pending topics stay hidden from the catalog until Telegram close
// and registry finalization both succeed.
func (s *Service) ReconcilePendingTopicClosures(ctx context.Context) error {
	if s == nil || s.api == nil || s.groups == nil || s.topicRegistry == nil {
		return nil
	}
	s.topicMu.Lock()
	defer s.topicMu.Unlock()
	coordinator, ok := s.topicRegistry.(forumTopicCloseCoordinator)
	if !ok {
		return nil
	}
	pending, err := coordinator.ListPending()
	if err != nil {
		return fmt.Errorf("list pending topic closes: %w", err)
	}
	var reconcileErr error
	for _, topic := range pending {
		if !topic.LifecycleOwned {
			continue
		}
		group, err := s.groups.GetByID(topic.GroupID)
		if err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("load pending topic group %d: %w", topic.GroupID, err))
			continue
		}
		if err := s.ensureTopicPermission(ctx, group.TelegramChatID); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("check pending topic permission %d: %w", topic.MessageThreadID, err))
			continue
		}
		assignments, err := s.groups.GetChannelAssignments(topic.GroupID)
		if err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("recheck pending topic assignments %d: %w", topic.MessageThreadID, err))
			continue
		}
		if hasTopicAssignment(assignments, topic.MessageThreadID) {
			if err := s.topicRegistry.MarkReopened(topic.GroupID, topic.MessageThreadID); err != nil &&
				!errors.Is(err, db.ErrNotFound) {
				reconcileErr = errors.Join(reconcileErr, fmt.Errorf("cancel pending topic close %d: %w", topic.MessageThreadID, err))
			}
			continue
		}
		if err := s.ensureTopicPermission(ctx, group.TelegramChatID); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("check pending topic permission %d: %w", topic.MessageThreadID, err))
			continue
		}
		if err := s.api.CloseForumTopic(ctx, &telego.CloseForumTopicParams{
			ChatID:          groupTelegramChatID(group.TelegramChatID),
			MessageThreadID: int(topic.MessageThreadID),
		}); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("reconcile close topic %d: %w", topic.MessageThreadID, s.classifyTelegramError(err)))
			continue
		}
		if err := s.topicRegistry.MarkClosed(topic.GroupID, topic.MessageThreadID); err != nil &&
			!errors.Is(err, db.ErrNotFound) {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("reconcile registry topic %d: %w", topic.MessageThreadID, err))
		}
	}
	return reconcileErr
}

func hasOtherTopicAssignment(assignments []model.GroupChannel, removedChannelID, threadID int64) bool {
	for _, assignment := range assignments {
		if assignment.ChannelID == removedChannelID || assignment.TopicThreadID == nil {
			continue
		}
		if *assignment.TopicThreadID == threadID {
			return true
		}
	}
	return false
}

func hasTopicAssignment(assignments []model.GroupChannel, threadID int64) bool {
	for _, assignment := range assignments {
		if assignment.TopicThreadID != nil && *assignment.TopicThreadID == threadID {
			return true
		}
	}
	return false
}

func (s *Service) topicAssignment(groupID, channelID int64) (threadID, chatID int64, err error) {
	if s.groups == nil {
		return 0, 0, errors.New("group repository is not configured")
	}
	group, err := s.groups.GetByID(groupID)
	if err != nil {
		return 0, 0, fmt.Errorf("load group: %w", err)
	}
	assignments, err := s.groups.GetChannelAssignments(groupID)
	if err != nil {
		return 0, 0, fmt.Errorf("load channel assignment: %w", err)
	}
	for _, assignment := range assignments {
		if assignment.ChannelID == channelID {
			if assignment.TopicThreadID == nil {
				return 0, 0, db.ErrNotFound
			}
			return *assignment.TopicThreadID, group.TelegramChatID, nil
		}
	}
	return 0, 0, db.ErrNotFound
}

func (s *Service) ensureTopicPermission(ctx context.Context, chatID int64) error {
	if s == nil || s.api == nil {
		return errors.New("Telegram permission client is not configured")
	}
	if chatID == 0 {
		return errors.New("forum chat id is invalid")
	}
	me, err := s.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("get bot identity: %w", s.classifyTelegramError(err))
	}
	if me == nil || me.ID <= 0 {
		return errors.New("Telegram bot identity is unknown")
	}
	member, err := s.api.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: groupTelegramChatID(chatID),
		UserID: me.ID,
	})
	if err != nil {
		return fmt.Errorf("get bot chat member: %w", s.classifyTelegramError(err))
	}
	if member == nil {
		return errors.New("Telegram bot chat member is unknown")
	}
	switch typed := member.(type) {
	case *telego.ChatMemberOwner:
		if typed.Status == telego.MemberStatusCreator {
			return nil
		}
	case *telego.ChatMemberAdministrator:
		if typed.Status == telego.MemberStatusAdministrator {
			if typed.CanManageTopics {
				return nil
			}
			return ErrTopicPermissionDenied
		}
	default:
		return ErrTopicPermissionDenied
	}
	return ErrTopicPermissionDenied
}

// CheckTopicPermission validates the bot's current forum-topic administrator
// permission for a configured group before the WebApp creates an assignment.
func (s *Service) CheckTopicPermission(ctx context.Context, groupID int64) error {
	if s == nil || s.groups == nil {
		return errors.New("group repository is not configured")
	}
	group, err := s.groups.GetByID(groupID)
	if err != nil {
		return fmt.Errorf("load group for topic permission: %w", err)
	}
	if err := s.ensureTopicPermission(ctx, group.TelegramChatID); err != nil {
		return fmt.Errorf("check topic permission: %w", err)
	}
	return nil
}

func groupTelegramChatID(id int64) telego.ChatID {
	return telego.ChatID{ID: id}
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max])
	}
	return value
}

func isUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "401") || strings.Contains(message, "unauthorized") || strings.Contains(message, "token is invalid")
}

func (s *Service) classifyTelegramError(err error) error {
	if err == nil {
		return nil
	}
	if !isUnauthorizedError(err) {
		return err
	}
	revoked := fmt.Errorf("%w: %w", ErrTokenRevoked, err)
	s.logf("FATAL: Bot token has been revoked (401 Unauthorized). Shutting down.")
	if s.onTokenRevoked != nil {
		s.onTokenRevoked(revoked)
	}
	return revoked
}

func (s *Service) logf(format string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

// jsonUnmarshal is a small seam for tests while keeping encoding details out
// of the update routing logic.
var jsonUnmarshal = func(data []byte, target any) error {
	return json.Unmarshal(data, target)
}
