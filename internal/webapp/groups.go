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
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/go-chi/chi/v5"
)

// GroupVerifier validates that the bot can access a Telegram group and returns
// its display title. The default verifier is permissive for deployments that
// already validate membership when the bot joins a group.
type GroupVerifier interface {
	Verify(int64) (string, error)
}

type permissiveGroupVerifier struct{}

func (permissiveGroupVerifier) Verify(chatID int64) (string, error) {
	return strconv.FormatInt(chatID, 10), nil
}

type telegramGroupVerifier struct {
	token  string
	client *http.Client
}

func (v telegramGroupVerifier) Verify(chatID int64) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint := "https://api.telegram.org/bot" + v.token + "/getChat?chat_id=" + url.QueryEscape(strconv.FormatInt(chatID, 10))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errors.New("telegram group verification failed")
	}
	response, err := v.client.Do(request)
	if err != nil {
		return "", errors.New("telegram group verification failed")
	}
	defer response.Body.Close()
	var payload struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Title string `json:"title"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || !payload.OK {
		return "", errors.New("telegram group verification failed")
	}
	if strings.TrimSpace(payload.Result.Title) == "" {
		return strconv.FormatInt(chatID, 10), nil
	}
	return payload.Result.Title, nil
}

type GroupService struct {
	repository dbGroupRepository
	channels   dbChannelLookup
	verifier   GroupVerifier
}

type dbGroupRepository interface {
	Insert(*model.Group) (int64, error)
	GetByID(int64) (*model.Group, error)
	GetByChatID(int64) (*model.Group, error)
	List() ([]model.Group, error)
	Delete(int64) error
	GetChannelAssignments(int64) ([]model.GroupChannel, error)
	AssignChannel(int64, int64, *int64) error
	UnassignChannel(int64, int64) error
}

type dbChannelLookup interface {
	GetByID(int64) (*model.Channel, error)
}

func NewGroupService(repository dbGroupRepository, channels dbChannelLookup) *GroupService {
	return &GroupService{repository: repository, channels: channels, verifier: permissiveGroupVerifier{}}
}

type groupInput struct {
	ChatID string `json:"chat_id"`
}

type assignmentInput struct {
	ChannelID     string          `json:"channel_id"`
	TopicThreadID json.RawMessage `json:"topic_thread_id"`
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
	if s.verifier != nil {
		title, err = s.verifier.Verify(chatID)
		if err != nil {
			return nil, fmt.Errorf("group verification failed: %w", err)
		}
	}
	group := &model.Group{TelegramChatID: chatID, Title: strings.TrimSpace(title), Status: model.GroupStatusActive}
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
		"assignments":      assignments,
	}
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
			assignments, err := s.groupAssignments(group.ID)
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
		group, err := s.groupService.Create(input.ChatID)
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
		assignments, err := s.groupAssignments(id)
		if err != nil {
			writeGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, groupJSON(*group, assignments))
	case http.MethodDelete:
		if err := s.groupService.repository.Delete(id); err != nil {
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
	if _, err := s.groupService.repository.GetByID(groupID); err != nil {
		writeGroupError(w, err)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input assignmentInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный JSON"})
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
	if err := s.groupService.repository.AssignChannel(groupID, channelID, topic); err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "assigned"})
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
	if err := s.groupService.repository.UnassignChannel(groupID, channelID); err != nil {
		writeGroupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) groupAssignments(groupID int64) ([]map[string]any, error) {
	assignments, err := s.groupService.repository.GetChannelAssignments(groupID)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(assignments))
	for _, assignment := range assignments {
		item := map[string]any{
			"channel_id":      assignment.ChannelID,
			"topic_thread_id": assignment.TopicThreadID,
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
	if _, err := parsePositiveID(chi.URLParam(r, "id")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный ID группы"})
		return
	}
	// Topic discovery is supplied by the Telegram bot update stream. The API
	// returns a stable empty list until topic metadata is available.
	writeJSON(w, http.StatusOK, []map[string]any{})
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
	if err != nil || id < 0 {
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
	case strings.Contains(strings.ToLower(err.Error()), "chat_id"):
		status, message = http.StatusBadRequest, "Chat ID должен быть числом (например, -1001234567890)"
	case strings.Contains(strings.ToLower(err.Error()), "verification"):
		status, message = http.StatusBadRequest, "Бот не является участником этой группы. Добавьте бота в группу и попробуйте снова."
	}
	writeJSON(w, status, map[string]string{"error": message})
}
