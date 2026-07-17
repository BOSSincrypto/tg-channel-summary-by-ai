package webapp

import (
	"context"
	"net/http"
	"strconv"
	"sync"

	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
)

// DigestRunner is the narrow production dependency needed by the manual
// WebApp action.
type DigestRunner interface {
	GenerateManual(int64) (*digest.Digest, error)
}

type digestJob struct {
	ID           string `json:"job_id"`
	GroupID      int64  `json:"group_id"`
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
	PostCount    int    `json:"post_count,omitempty"`
	ChannelCount int    `json:"channel_count,omitempty"`
}

type digestJobStore struct {
	mu      sync.RWMutex
	nextID  int64
	jobs    map[string]*digestJob
	running map[int64]string
}

func newDigestJobStore() *digestJobStore {
	return &digestJobStore{jobs: make(map[string]*digestJob), running: make(map[int64]string)}
}

func (s *digestJobStore) create(groupID int64) *digestJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.running[groupID]; existing != "" {
		return nil
	}
	s.nextID++
	job := &digestJob{ID: strconv.FormatInt(s.nextID, 10), GroupID: groupID, Status: "parsing"}
	s.jobs[job.ID] = job
	s.running[groupID] = job.ID
	return cloneDigestJob(job)
}

func (s *digestJobStore) update(id string, update func(*digestJob)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return
	}
	update(job)
	if job.Status == "completed" || job.Status == "error" {
		delete(s.running, job.GroupID)
	}
}

func (s *digestJobStore) get(id string) *digestJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneDigestJob(s.jobs[id])
}

func cloneDigestJob(job *digestJob) *digestJob {
	if job == nil {
		return nil
	}
	copy := *job
	return &copy
}

func (s *Server) handleDigestTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.digestRunner == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "digest service is not configured"})
		return
	}
	var input struct {
		GroupID string `json:"group_id"`
	}
	if err := decodeJSON(r, w, &input); err != nil {
		return
	}
	groupID, err := parsePositiveID(input.GroupID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "group_id обязателен"})
		return
	}
	job := s.digestJobs.create(groupID)
	if job == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Дайджест для этой группы уже выполняется. Дождитесь завершения."})
		return
	}
	go s.runDigestJob(context.Background(), job.ID, groupID)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) runDigestJob(ctx context.Context, jobID string, groupID int64) {
	s.digestJobs.update(jobID, func(job *digestJob) {
		job.Status = "summarizing"
	})
	result, err := s.digestRunner.GenerateManual(groupID)
	if err != nil {
		s.digestJobs.update(jobID, func(job *digestJob) {
			job.Status = "error"
			job.Message = "Не удалось выполнить дайджест: " + safeDigestError(err)
		})
		return
	}
	_ = ctx
	s.digestJobs.update(jobID, func(job *digestJob) {
		job.Status = "completed"
		job.Message = "Дайджест отправлен."
		if result != nil {
			job.PostCount = result.PostCount
		}
	})
}

func (s *Server) handleDigestStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	job := s.digestJobs.get(r.URL.Query().Get("id"))
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Дайджест не найден"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func safeDigestError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 300 {
		return message[:300]
	}
	return message
}
