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
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/mymmrac/telego"
)

var ErrTokenRevoked = errors.New("bot token revoked")

type logger interface {
	Printf(format string, args ...any)
}

type telegramClient interface {
	GetMe(context.Context) (*telego.User, error)
	SetMyCommands(context.Context, *telego.SetMyCommandsParams) error
	AnswerCallbackQuery(context.Context, *telego.AnswerCallbackQueryParams) error
	SendMessage(context.Context, *telego.SendMessageParams) (*telego.Message, error)
	GetChat(context.Context, *telego.GetChatParams) (*telego.ChatFullInfo, error)
	CreateForumTopic(context.Context, *telego.CreateForumTopicParams) (*telego.ForumTopic, error)
	CloseForumTopic(context.Context, *telego.CloseForumTopicParams) error
	DeleteForumTopic(context.Context, *telego.DeleteForumTopicParams) error
	EditForumTopic(context.Context, *telego.EditForumTopicParams) error
}

type updatePoller interface {
	UpdatesViaLongPolling(context.Context, *telego.GetUpdatesParams, ...telego.LongPollingOption) (<-chan telego.Update, error)
}

type ownerNotifier interface {
	NotifyOwner(context.Context, string) error
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
	api       telegramClient
	poller    updatePoller
	groups    *db.GroupRepository
	channels  *db.ChannelRepository
	notifier  ownerNotifier
	lifecycle GroupLifecycle
	logger    logger
	ownerID   string
	botName   string
	webAppURL string
	commands  map[string]CommandHandler
	applyData func(context.Context, *telego.Message, BotSettings) error

	ctx    context.Context
	cancel context.CancelFunc
	stopMu sync.Mutex
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
		if isUnauthorizedError(err) {
			s.logf("FATAL: Bot token has been revoked. Shutting down.")
			return fmt.Errorf("%w: getMe: %v", ErrTokenRevoked, err)
		}
		return fmt.Errorf("verify bot identity with getMe: %w", err)
	}
	if me == nil || me.ID == 0 || strings.TrimSpace(me.Username) == "" {
		return errors.New("verify bot identity with getMe: incomplete identity")
	}
	s.botName = strings.TrimPrefix(strings.ToLower(me.Username), "@")
	s.logf("Bot identity verified: @%s (ID: %d)", me.Username, me.ID)
	if err := s.registerCommands(ctx); err != nil {
		if isUnauthorizedError(err) {
			s.logf("FATAL: Bot token has been revoked (401 Unauthorized). Shutting down.")
			return fmt.Errorf("%w: register commands: %v", ErrTokenRevoked, err)
		}
		return fmt.Errorf("register bot commands: %w", err)
	}

	updates, err := s.poller.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		Limit:          100,
		Timeout:        30,
		AllowedUpdates: []string{"message", "callback_query", "my_chat_member"},
	}, telego.WithLongPollingRetryTimeout(0))
	if err != nil {
		if isUnauthorizedError(err) {
			s.logf("FATAL: Bot token has been revoked. Shutting down.")
			return fmt.Errorf("%w: start long polling: %v", ErrTokenRevoked, err)
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
					if isUnauthorizedError(probeErr) {
						s.logf("FATAL: Bot token has been revoked (401 Unauthorized). Shutting down.")
						return fmt.Errorf("%w: long polling stopped: %v", ErrTokenRevoked, probeErr)
					}
					return fmt.Errorf("long polling stopped: verify bot identity: %w", probeErr)
				}
				return nil
			}
			if err := s.HandleUpdate(ctx, &update); err != nil {
				if isUnauthorizedError(err) {
					s.logf("FATAL: Bot token has been revoked (401 Unauthorized). Shutting down.")
					return fmt.Errorf("%w: update %d: %v", ErrTokenRevoked, update.UpdateID, err)
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
	commands := []telego.BotCommand{
		{Command: "start", Description: "Start the bot"},
		{Command: "settings", Description: "Configure bot settings"},
	}
	scope := &telego.BotCommandScopeAllPrivateChats{Type: "all_private_chats"}
	if err := s.api.SetMyCommands(ctx, &telego.SetMyCommandsParams{Commands: commands, Scope: scope}); err != nil {
		return err
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
				return err
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
	return err
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
	DigestTime string   `json:"digest_time"`
	Channels   []string `json:"channels"`
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
	return nil
}

func (s *Service) sendPlain(ctx context.Context, chatID telego.ChatID, text string) error {
	_, err := s.api.SendMessage(ctx, &telego.SendMessageParams{ChatID: chatID, Text: text})
	return err
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
	if s.groups == nil {
		return errors.New("group repository is not configured")
	}
	fullChat, err := s.api.GetChat(ctx, &telego.GetChatParams{ChatID: chat.ChatID()})
	if err != nil {
		return fmt.Errorf("get chat %d: %w", chat.ID, err)
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
			return fmt.Errorf("send forum requirement: %w", sendErr)
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
	topic, err := s.api.CreateForumTopic(ctx, &telego.CreateForumTopicParams{
		ChatID: groupTelegramChatID(group.TelegramChatID),
		Name:   name,
	})
	if err != nil {
		return fmt.Errorf("create topic for channel %d: %w", channelID, err)
	}
	if topic == nil || topic.MessageThreadID <= 0 {
		return errors.New("create topic returned an invalid message thread id")
	}
	if err := s.groups.UpdateChannelTopic(groupID, channelID, int64(topic.MessageThreadID)); err != nil {
		cleanupErr := s.api.DeleteForumTopic(ctx, &telego.DeleteForumTopicParams{
			ChatID:          groupTelegramChatID(group.TelegramChatID),
			MessageThreadID: int(topic.MessageThreadID),
		})
		if cleanupErr != nil {
			return fmt.Errorf("persist topic for channel %d: %w; rollback topic: %v", channelID, err, cleanupErr)
		}
		return fmt.Errorf("persist topic for channel %d: %w", channelID, err)
	}
	return nil
}

// RenameChannelTopic updates the Telegram topic name for an assignment.
func (s *Service) RenameChannelTopic(ctx context.Context, groupID, channelID int64, name string) error {
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
	if err := s.api.EditForumTopic(ctx, &telego.EditForumTopicParams{
		ChatID:          groupTelegramChatID(chatID),
		MessageThreadID: int(threadID),
		Name:            name,
	}); err != nil {
		return fmt.Errorf("rename topic: %w", err)
	}
	return nil
}

// RemoveChannelTopic closes the topic before removing its persisted assignment.
func (s *Service) RemoveChannelTopic(ctx context.Context, groupID, channelID int64) error {
	if s.groups == nil {
		return errors.New("group repository is not configured")
	}
	threadID, chatID, err := s.topicAssignment(groupID, channelID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return s.groups.UnassignChannel(groupID, channelID)
		}
		return err
	}
	if err := s.groups.UnassignChannel(groupID, channelID); err != nil {
		return fmt.Errorf("remove topic assignment: %w", err)
	}
	if err := s.api.CloseForumTopic(ctx, &telego.CloseForumTopicParams{
		ChatID:          groupTelegramChatID(chatID),
		MessageThreadID: int(threadID),
	}); err != nil {
		rollbackErr := s.groups.AssignChannel(groupID, channelID, &threadID)
		if rollbackErr != nil {
			return fmt.Errorf("close topic: %w; rollback topic assignment: %v", err, rollbackErr)
		}
		return fmt.Errorf("close topic: %w", err)
	}
	return nil
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
