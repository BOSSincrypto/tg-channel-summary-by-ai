package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/telegram"
	"github.com/go-chi/chi/v5"
)

// GroupVerifier validates that the bot can access a Telegram group and returns
// its display title. The default verifier is permissive for deployments that
// already validate membership when the bot joins a group.
type GroupVerifier interface {
	Verify(int64) (string, error)
}

type forumGroupVerifier interface {
	VerifyGroup(int64) (string, bool, error)
}

// TopicLifecycle is the production boundary for forum topic mutations.
// Implementations own the Telegram API call and the corresponding persisted
// topic assignment, keeping WebApp mutations independent from bot internals.
type TopicLifecycle interface {
	CreateChannelTopic(context.Context, int64, int64) error
	RemoveChannelTopic(context.Context, int64, int64) error
	CheckTopicPermission(context.Context, int64) error
}

// VersionedTopicLifecycle performs an unassignment together with the
// aggregate-version check before any Telegram topic side effect.
type VersionedTopicLifecycle interface {
	RemoveChannelTopicWithVersion(context.Context, int64, int64, int64) (int64, error)
}

// TransactionalTopicAssignmentLifecycle commits a forum assignment together
// with externally created-topic ownership and the group aggregate version.
type TransactionalTopicAssignmentLifecycle interface {
	AssignChannelTopicWithVersion(context.Context, int64, int64, *int64, int64) (int64, error)
}

// Topic describes a selectable forum topic. MessageThreadID is always
// positive; the WebApp never exposes the general topic as a placeholder ID.
type Topic struct {
	MessageThreadID int64  `json:"message_thread_id"`
	Name            string `json:"name"`
}

// TopicCatalog provides forum topics observed through the Telegram lifecycle
// boundary. Telegram has no Bot API method for listing historical topics, so
// production implementations must read the durable observed registry.
type TopicCatalog interface {
	ListTopics(context.Context, int64) ([]Topic, error)
}

type permissiveGroupVerifier struct{}

func (permissiveGroupVerifier) Verify(chatID int64) (string, error) {
	return strconv.FormatInt(chatID, 10), nil
}

type telegramGroupVerifier struct {
	token          string
	client         *http.Client
	onTokenRevoked func(error)
}

func (v *telegramGroupVerifier) SetTokenRevocationHandler(handler func(error)) {
	if v != nil {
		v.onTokenRevoked = handler
	}
}

func (v telegramGroupVerifier) Verify(chatID int64) (string, error) {
	title, _, err := v.VerifyGroup(chatID)
	return title, err
}

func (v telegramGroupVerifier) VerifyGroup(chatID int64) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint := "https://api.telegram.org/bot" + v.token + "/getChat?chat_id=" + url.QueryEscape(strconv.FormatInt(chatID, 10))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, errors.New("telegram group verification failed")
	}
	response, err := v.client.Do(request)
	if err != nil {
		return "", false, errors.New("telegram group verification failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		revoked := fmt.Errorf("%w: Telegram getChat returned %s", telegram.ErrTokenRevoked, response.Status)
		if v.onTokenRevoked != nil {
			v.onTokenRevoked(revoked)
		}
		return "", false, revoked
	}
	if response.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("telegram group verification failed: getChat returned %s", response.Status)
	}
	var payload struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Title   string `json:"title"`
			IsForum bool   `json:"is_forum"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || !payload.OK {
		return "", false, errors.New("telegram group verification failed")
	}
	if strings.TrimSpace(payload.Result.Title) == "" {
		return strconv.FormatInt(chatID, 10), payload.Result.IsForum, nil
	}
	return payload.Result.Title, payload.Result.IsForum, nil
}

type GroupService struct {
	repository   dbGroupRepository
	channels     dbChannelLookup
	verifier     GroupVerifier
	topics       TopicLifecycle
	catalog      TopicCatalog
	assignmentMu sync.Mutex
}

type dbGroupRepository interface {
	Insert(*model.Group) (int64, error)
	GetByID(int64) (*model.Group, error)
	GetByChatID(int64) (*model.Group, error)
	List() ([]model.Group, error)
	Delete(int64) error
	GetChannelAssignments(int64) ([]model.GroupChannel, error)
	ListForumTopics(int64) ([]model.ForumTopic, error)
	AssignChannel(int64, int64, *int64) error
	UnassignChannel(int64, int64) error
}

type optimisticGroupRepository interface {
	DeleteOptimistic(int64, int64) error
}

type optimisticAssignmentRepository interface {
	AssignChannelOptimistic(int64, int64, *int64, int64) (int64, error)
	UnassignChannelOptimistic(int64, int64, int64) (int64, error)
	RollbackAssignmentOptimistic(int64, int64, int64) error
}

type forumTopicStateLookup interface {
	GetForumTopic(int64, int64) (*model.ForumTopic, error)
}

type dbChannelLookup interface {
	GetByID(int64) (*model.Channel, error)
}

func NewGroupService(repository dbGroupRepository, channels dbChannelLookup) *GroupService {
	service := &GroupService{repository: repository, channels: channels, verifier: permissiveGroupVerifier{}}
	service.catalog = persistedTopicCatalog{repository: repository}
	return service
}

func (s *GroupService) SetTopicLifecycle(lifecycle TopicLifecycle) {
	if s != nil {
		s.topics = lifecycle
	}
}

// SetTopicCatalog replaces the forum topic discovery boundary.
func (s *GroupService) SetTopicCatalog(catalog TopicCatalog) {
	if s == nil {
		return
	}
	if catalog == nil {
		s.catalog = persistedTopicCatalog{repository: s.repository}
		return
	}
	s.catalog = catalog
}

type persistedTopicCatalog struct {
	repository dbGroupRepository
}

func (c persistedTopicCatalog) ListTopics(_ context.Context, groupID int64) ([]Topic, error) {
	registry, err := c.repository.ListForumTopics(groupID)
	if err != nil {
		return nil, err
	}
	topics := make([]Topic, 0, len(registry))
	for _, topic := range registry {
		if topic.MessageThreadID <= 0 || strings.TrimSpace(topic.Name) == "" {
			continue
		}
		topics = append(topics, Topic{
			MessageThreadID: topic.MessageThreadID,
			Name:            strings.TrimSpace(topic.Name),
		})
	}
	return topics, nil
}

type groupInput struct {
	ChatID string `json:"chat_id"`
}

type assignmentInput struct {
	ChannelID     string          `json:"channel_id"`
	TopicThreadID json.RawMessage `json:"topic_thread_id"`
	Version       int64           `json:"version"`
}

func (s *GroupService) Create(chatIDValue string) (*model.Group, error) {
	chatIDValue = strings.TrimSpace(chatIDValue)
	chatID, err := strconv.ParseInt(chatIDValue, 10, 64)
	if err != nil || chatID == 0 {
		return nil, errors.New("chat_id must be a numeric string")
	}
	if _, err := s.repository.GetByChatID(chatID); err == nil {
		return nil, db.ErrDuplicate
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, err
	}
	title := chatIDValue
	status := model.GroupStatusActive
	if s.verifier != nil {
		if verifier, ok := s.verifier.(forumGroupVerifier); ok {
			var forum bool
			title, forum, err = verifier.VerifyGroup(chatID)
			if err == nil && !forum {
				status = model.GroupStatusIneligible
			}
		} else {
			title, err = s.verifier.Verify(chatID)
		}
		if err != nil {
			return nil, fmt.Errorf("group verification failed: %w", err)
		}
	}
	group := &model.Group{TelegramChatID: chatID, Title: strings.TrimSpace(title), Status: status}
	id, err := s.repository.Insert(group)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, db.ErrDuplicate
		}
		return nil, err
	}
	return s.repository.GetByID(id)
}

func groupJSON(group model.Group, assignments []map[string]any) map[string]any {
	return map[string]any{
		"id":               group.ID,
		"version":          group.Version,
		"telegram_chat_id": strconv.FormatInt(group.TelegramChatID, 10),
		"chat_id":          strconv.FormatInt(group.TelegramChatID, 10),
		"title":            group.Title,
		"status":           group.Status,
		"is_forum":         isForumGroup(group),
		"assignments":      assignments,
	}
}

func isForumGroup(group model.Group) bool {
	return group.Status == "" || group.Status == model.GroupStatusActive
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	if s.groupService == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "group service is not configured"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		groups, err := s.groupService.repository.List()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Не удалось загрузить группы"})
			return
		}
		body := make([]map[string]any, 0, len(groups))
		for _, group := range groups {
			assignments, err := s.groupAssignments(group.ID, isForumGroup(group))
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Не удалось загрузить назначения каналов"})
				return
			}
			body = append(body, groupJSON(group, assignments))
		}
		writeJSON(w, http.StatusOK, body)
	case http.MethodPost:
		var input groupInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный JSON"})
			return
		}
		var group *model.Group
		err := s.withGroupSchedulerLifecycle(func() error {
			var err error
			group, err = s.groupService.Create(input.ChatID)
			if err != nil {
				return err
			}
			if s.groupScheduler != nil && isForumGroup(*group) {
				repository, ok := s.groupService.repository.(groupSchedulerRepository)
				if !ok {
					return errors.New("group scheduler persistence is not configured")
				}
				if err := restoreScheduledGroup(s.groupScheduler, group.ID); err != nil {
					if recordErr := repository.RecordSchedulerSync(group.ID); recordErr != nil {
						return fmt.Errorf("register group scheduler: %w; record recovery: %v", err, recordErr)
					}
					return fmt.Errorf("register group scheduler: %w", err)
				}
				if err := repository.ClearSchedulerSync(group.ID); err != nil {
					return fmt.Errorf("clear group scheduler recovery: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			writeGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, groupJSON(*group, []map[string]any{}))
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGroupByID(w http.ResponseWriter, r *http.Request) {
	id, err := parsePositiveID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный ID группы"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		group, err := s.groupService.repository.GetByID(id)
		if err != nil {
			writeGroupError(w, err)
			return
		}
		assignments, err := s.groupAssignments(id, isForumGroup(*group))
		if err != nil {
			writeGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, groupJSON(*group, assignments))
	case http.MethodDelete:
		var input versionInput
		if err := decodeJSON(r, w, &input); err != nil {
			return
		}
		if input.Version <= 0 {
			writeGroupError(w, db.ErrConflict)
			return
		}
		strict, ok := s.groupService.repository.(optimisticGroupRepository)
		if !ok {
			writeGroupError(w, errors.New("group deletion locking is not configured"))
			return
		}
		if s.forumFence != nil {
			s.forumFence.Lock()
			defer s.forumFence.Unlock()
		}
		err := s.withGroupSchedulerLifecycle(func() error {
			group, err := s.groupService.repository.GetByID(id)
			if err != nil {
				return err
			}
			if group.Version != input.Version {
				return db.ErrConflict
			}
			if s.groupScheduler != nil {
				removeScheduledGroup(s.groupScheduler, id)
			}
			if err := strict.DeleteOptimistic(id, input.Version); err != nil {
				groupStillExists := true
				if _, lookupErr := s.groupService.repository.GetByID(id); errors.Is(lookupErr, db.ErrNotFound) {
					groupStillExists = false
				}
				if s.groupScheduler != nil && groupStillExists && isForumGroup(*group) {
					if restoreErr := restoreScheduledGroup(s.groupScheduler, id); restoreErr != nil {
						repository, repositoryOK := s.groupService.repository.(groupSchedulerRepository)
						if repositoryOK {
							if recordErr := repository.RecordSchedulerSync(id); recordErr != nil {
								err = fmt.Errorf("%w; restore scheduler: %v; record recovery: %v", err, restoreErr, recordErr)
							} else {
								err = fmt.Errorf("%w; restore scheduler: %v", err, restoreErr)
							}
						} else {
							err = fmt.Errorf("%w; restore scheduler: %v", err, restoreErr)
						}
					}
				}
				if !groupStillExists {
					if repository, ok := s.groupService.repository.(groupSchedulerRepository); ok {
						if clearErr := repository.ClearSchedulerSync(id); clearErr != nil {
							if recordErr := repository.RecordSchedulerRemoval(id); recordErr != nil {
								err = fmt.Errorf("%w; clear scheduler recovery: %v; record removal: %v", err, clearErr, recordErr)
							} else {
								err = fmt.Errorf("%w; clear scheduler recovery: %v", err, clearErr)
							}
						}
					}
				}
				return err
			}
			if repository, ok := s.groupService.repository.(groupSchedulerRepository); ok {
				if err := repository.ClearSchedulerSync(id); err != nil {
					// The group and its live job are already gone. Keep a durable
					// removal intent so restart reconciliation can retry cleanup.
					if recordErr := repository.RecordSchedulerRemoval(id); recordErr != nil {
						return fmt.Errorf("clear group scheduler recovery: %w; record removal: %v", err, recordErr)
					}
					return fmt.Errorf("clear group scheduler recovery: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			writeGroupError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGroupChannels(w http.ResponseWriter, r *http.Request) {
	groupID, err := parsePositiveID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный ID группы"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.groupService.assignmentMu.Lock()
	defer s.groupService.assignmentMu.Unlock()
	var input assignmentInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный JSON"})
		return
	}
	if input.Version <= 0 {
		writeGroupError(w, db.ErrConflict)
		return
	}
	group, err := s.groupService.repository.GetByID(groupID)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	if group.Version != input.Version {
		writeGroupError(w, db.ErrConflict)
		return
	}
	channelID, err := parsePositiveID(input.ChannelID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный ID канала"})
		return
	}
	if _, err := s.groupService.channels.GetByID(channelID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Канал не найден"})
		return
	}
	assignments, err := s.groupService.repository.GetChannelAssignments(groupID)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	for _, assignment := range assignments {
		if assignment.ChannelID == channelID {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Канал уже назначен этой группе"})
			return
		}
	}
	topic, err := parseNullableInt(input.TopicThreadID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный topic_thread_id"})
		return
	}
	forum := isForumGroup(*group)
	if topic != nil && !forum {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic_thread_id доступен только для форумной группы"})
		return
	}
	if topic != nil && *topic <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic_thread_id должен быть положительным"})
		return
	}
	if forum {
		if lifecycle, ok := s.groupService.topics.(TransactionalTopicAssignmentLifecycle); ok {
			nextVersion, err := lifecycle.AssignChannelTopicWithVersion(
				r.Context(), groupID, channelID, topic, input.Version,
			)
			if err != nil {
				writeGroupError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"status": "assigned", "version": nextVersion})
			return
		}
	}
	if topic != nil {
		if s.groupService.topics == nil {
			writeGroupError(w, errors.New("topic permission lifecycle is not configured"))
			return
		}
		if err := s.groupService.topics.CheckTopicPermission(r.Context(), groupID); err != nil {
			writeGroupError(w, err)
			return
		}
		topics, err := s.groupService.listTopics(r.Context(), groupID)
		if err != nil {
			writeGroupError(w, err)
			return
		}
		found := false
		for _, candidate := range topics {
			if candidate.MessageThreadID == *topic {
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic_thread_id не найден в каталоге группы"})
			return
		}
	}
	if forum {
		if s.groupService.topics == nil {
			writeGroupError(w, errors.New("topic permission lifecycle is not configured"))
			return
		}
		if topic == nil {
			if err := s.groupService.topics.CheckTopicPermission(r.Context(), groupID); err != nil {
				writeGroupError(w, err)
				return
			}
		}
	}
	optimistic, ok := s.groupService.repository.(optimisticAssignmentRepository)
	if !ok {
		writeGroupError(w, errors.New("assignment locking is not configured"))
		return
	}
	nextVersion, err := optimistic.AssignChannelOptimistic(groupID, channelID, topic, input.Version)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	// A forum assignment without a selected topic is completed by the shared
	// Telegram lifecycle. If creation fails, compensate the provisional row
	// and restore the aggregate version.
	if topic == nil && forum {
		if err := s.groupService.topics.CreateChannelTopic(r.Context(), groupID, channelID); err != nil {
			if rollbackErr := optimistic.RollbackAssignmentOptimistic(groupID, channelID, nextVersion); rollbackErr != nil {
				err = fmt.Errorf("%w; rollback assignment: %v", err, rollbackErr)
			}
			writeGroupError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "assigned", "version": nextVersion})
}

func (s *Server) handleGroupChannelByID(w http.ResponseWriter, r *http.Request) {
	groupID, err := parsePositiveID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный ID группы"})
		return
	}
	channelID, err := parsePositiveID(chi.URLParam(r, "channelID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный ID канала"})
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input versionInput
	if err := decodeJSON(r, w, &input); err != nil {
		return
	}
	if input.Version <= 0 {
		writeGroupError(w, db.ErrConflict)
		return
	}
	group, err := s.groupService.repository.GetByID(groupID)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	if group.Version != input.Version {
		writeGroupError(w, db.ErrConflict)
		return
	}
	assignments, err := s.groupService.repository.GetChannelAssignments(groupID)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	var assignment *model.GroupChannel
	for index := range assignments {
		if assignments[index].ChannelID == channelID {
			assignment = &assignments[index]
			break
		}
	}
	if assignment == nil {
		writeGroupError(w, db.ErrNotFound)
		return
	}
	if assignment.TopicThreadID != nil && s.groupService.topics != nil &&
		(group.Status == "" || group.Status == model.GroupStatusActive) {
		versioned, ok := s.groupService.topics.(VersionedTopicLifecycle)
		if !ok {
			writeGroupError(w, errors.New("versioned topic lifecycle is not configured"))
			return
		}
		nextVersion, err := versioned.RemoveChannelTopicWithVersion(r.Context(), groupID, channelID, input.Version)
		if err != nil {
			writeGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "unassigned", "version": nextVersion})
	} else {
		optimistic, ok := s.groupService.repository.(optimisticAssignmentRepository)
		if !ok {
			writeGroupError(w, errors.New("assignment locking is not configured"))
			return
		}
		nextVersion, err := optimistic.UnassignChannelOptimistic(groupID, channelID, input.Version)
		if err != nil {
			writeGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "unassigned", "version": nextVersion})
	}
}

func (s *Server) groupAssignments(groupID int64, forum bool) ([]map[string]any, error) {
	assignments, err := s.groupService.repository.GetChannelAssignments(groupID)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(assignments))
	for _, assignment := range assignments {
		if forum && assignment.TopicThreadID != nil {
			if lookup, ok := s.groupService.repository.(forumTopicStateLookup); ok {
				topic, topicErr := lookup.GetForumTopic(groupID, *assignment.TopicThreadID)
				if topicErr == nil && (topic.Closed || topic.ClosePending) {
					continue
				}
				if topicErr != nil && !errors.Is(topicErr, db.ErrNotFound) {
					return nil, topicErr
				}
			}
		}
		item := map[string]any{"channel_id": assignment.ChannelID}
		if forum && assignment.TopicThreadID != nil && *assignment.TopicThreadID > 0 {
			item["topic_thread_id"] = *assignment.TopicThreadID
		}
		if channel, err := s.groupService.channels.GetByID(assignment.ChannelID); err == nil {
			item["username"] = channel.Username
			item["title"] = channel.Title
		} else if !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *Server) handleGroupTopics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := parsePositiveID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный ID группы"})
		return
	}
	group, err := s.groupService.repository.GetByID(id)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	if !isForumGroup(*group) {
		writeJSON(w, http.StatusOK, []Topic{})
		return
	}
	topics, err := s.groupService.listTopics(r.Context(), id)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, topics)
}

func (s *GroupService) listTopics(ctx context.Context, groupID int64) ([]Topic, error) {
	if s == nil || s.catalog == nil {
		return []Topic{}, nil
	}
	topics, err := s.catalog.ListTopics(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("list forum topics: %w", err)
	}
	result := make([]Topic, 0, len(topics))
	seen := make(map[int64]struct{}, len(topics))
	for _, topic := range topics {
		if topic.MessageThreadID <= 0 {
			continue
		}
		if _, ok := seen[topic.MessageThreadID]; ok {
			continue
		}
		seen[topic.MessageThreadID] = struct{}{}
		if strings.TrimSpace(topic.Name) == "" {
			continue
		}
		result = append(result, topic)
	}
	return result, nil
}

func (s *Server) handleAvailableGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, []map[string]any{})
}

func parsePositiveID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func parseNullableInt(raw json.RawMessage) (*int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var value string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
	} else {
		value = string(raw)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return nil, errors.New("invalid integer")
	}
	return &id, nil
}

func writeGroupError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "Не удалось обработать группу"
	switch {
	case errors.Is(err, db.ErrNotFound):
		status, message = http.StatusNotFound, "Группа не найдена"
	case errors.Is(err, db.ErrDuplicate):
		status, message = http.StatusConflict, "Группа уже добавлена"
	case errors.Is(err, db.ErrConflict):
		status, message = http.StatusConflict, "Топик больше недоступен для назначения"
	case strings.Contains(strings.ToLower(err.Error()), "database table is locked"),
		strings.Contains(strings.ToLower(err.Error()), "database is locked"):
		status, message = http.StatusConflict, "Группа была изменена в другой сессии. Обновите страницу."
	case strings.Contains(strings.ToLower(err.Error()), "chat_id"):
		status, message = http.StatusBadRequest, "Chat ID должен быть числом (например, -1001234567890)"
	case strings.Contains(strings.ToLower(err.Error()), "verification"):
		status, message = http.StatusBadRequest, "Бот не является участником этой группы. Добавьте бота в группу и попробуйте снова."
	case strings.Contains(strings.ToLower(err.Error()), "scheduler"):
		status, message = http.StatusBadGateway, "Не удалось синхронизировать расписание группы"
	case strings.Contains(strings.ToLower(err.Error()), "topic"),
		strings.Contains(strings.ToLower(err.Error()), "telegram"):
		status, message = http.StatusBadGateway, "Не удалось обновить топик группы"
	}
	writeJSON(w, status, map[string]string{"error": message})
}
