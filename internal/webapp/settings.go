package webapp

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
)

const settingsConfigKey = "webapp_settings"

var digestTimePattern = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d$`)

type settingsPayload struct {
	DigestTime   string `json:"digest_time"`
	Timezone     string `json:"timezone"`
	DefaultModel string `json:"default_model"`
	Version      int64  `json:"version"`
}

func defaultSettings() settingsPayload {
	return settingsPayload{
		DigestTime:   "21:00",
		Timezone:     "Europe/Moscow",
		DefaultModel: "openai/gpt-oss-120b",
		Version:      1,
	}
}

func (s *Server) loadSettings() (settingsPayload, error) {
	defaults := defaultSettings()
	value, version, err := s.settingsRepository().GetWithVersion(settingsConfigKey)
	if errors.Is(err, db.ErrNotFound) {
		encoded, marshalErr := json.Marshal(defaults)
		if marshalErr != nil {
			return settingsPayload{}, marshalErr
		}
		if err := s.settingsRepository().Set(settingsConfigKey, string(encoded)); err != nil {
			return settingsPayload{}, err
		}
		return defaults, nil
	}
	if err != nil {
		return settingsPayload{}, err
	}
	var settings settingsPayload
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return settingsPayload{}, err
	}
	if settings.DigestTime == "" {
		settings.DigestTime = defaults.DigestTime
	}
	if settings.Timezone == "" {
		settings.Timezone = defaults.Timezone
	}
	if settings.DefaultModel == "" {
		settings.DefaultModel = defaults.DefaultModel
	}
	settings.Version = version
	return settings, nil
}

func (s *Server) settingsRepository() *db.ConfigRepository {
	return s.database.Config
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings, err := s.loadSettings()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Не удалось загрузить настройки"})
			return
		}
		writeJSON(w, http.StatusOK, settings)
	case http.MethodPut:
		var input settingsPayload
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный JSON"})
			return
		}
		if input.Version <= 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Для сохранения настроек требуется текущая положительная версия."})
			return
		}
		if err := validateSettings(input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		_, currentVersion, err := s.settingsRepository().GetWithVersion(settingsConfigKey)
		if errors.Is(err, db.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Сначала загрузите текущую версию настроек."})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Не удалось проверить версию настроек"})
			return
		}
		if currentVersion != input.Version {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Configuration was modified by another session. Please reload and try again."})
			return
		}
		var (
			version int64
		)
		if s.settingsApplier != nil {
			version, err = s.settingsApplier(r.Context(), SettingsMutation{
				DigestTime:   strings.TrimSpace(input.DigestTime),
				Timezone:     strings.TrimSpace(input.Timezone),
				DefaultModel: strings.TrimSpace(input.DefaultModel),
				Version:      input.Version,
			})
		} else {
			encoded, marshalErr := json.Marshal(settingsPayload{
				DigestTime: input.DigestTime, Timezone: input.Timezone, DefaultModel: strings.TrimSpace(input.DefaultModel),
			})
			if marshalErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Не удалось сохранить настройки"})
				return
			}
			version, err = s.settingsRepository().SetOptimistic(settingsConfigKey, string(encoded), input.Version)
		}
		if err != nil {
			if errors.Is(err, db.ErrConflict) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "Configuration was modified by another session. Please reload and try again."})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Не удалось сохранить настройки"})
			return
		}
		saved := settingsPayload{DigestTime: input.DigestTime, Timezone: input.Timezone, DefaultModel: strings.TrimSpace(input.DefaultModel), Version: version}
		writeJSON(w, http.StatusOK, saved)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func validateSettings(input settingsPayload) error {
	if !digestTimePattern.MatchString(strings.TrimSpace(input.DigestTime)) {
		return errors.New("digest_time должен быть в формате HH:MM")
	}
	timezone := strings.TrimSpace(input.Timezone)
	if timezone == "" {
		return errors.New("Неверный часовой пояс")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return errors.New("Неверный часовой пояс")
	}
	if strings.TrimSpace(input.DefaultModel) == "" {
		return errors.New("default_model обязателен")
	}
	if len(strings.TrimSpace(input.DefaultModel)) > 200 {
		return errors.New("default_model слишком длинный")
	}
	return nil
}
