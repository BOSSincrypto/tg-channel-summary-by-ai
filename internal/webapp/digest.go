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

type typedDigestRunner interface {
	GenerateManualResult(int64) (*digest.Digest, error)
}

type digestJob struct {
	ID             string   `json:"job_id"`
	GroupID        int64    `json:"group_id"`
	Status         string   `json:"status"`
	Outcome        string   `json:"outcome,omitempty"`
	Message        string   `json:"message,omitempty"`
	PostCount      int      `json:"post_count"`
	ChannelCount   int      `json:"channel_count"`
	FailedChannels []string `json:"failed_channels,omitempty"`
	MessageID      *int64   `json:"message_id,omitempty"`
	MessageURL     string   `json:"message_url,omitempty"`
	SummariesSaved bool     `json:"summaries_saved"`
	Delivered      bool     `json:"delivered"`
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
	if isTerminalDigestStatus(job) {
		delete(s.running, job.GroupID)
	}
}

func isTerminalDigestStatus(job *digestJob) bool {
	if job == nil {
		return false
	}
	switch job.Status {
	case "completed", "error":
		return true
	default:
		return false
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
	var (
		result *digest.Digest
		err    error
	)
	if runner, ok := s.digestRunner.(typedDigestRunner); ok {
		result, err = runner.GenerateManualResult(groupID)
	} else {
		result, err = s.digestRunner.GenerateManual(groupID)
	}
	if err != nil {
		s.digestJobs.update(jobID, func(job *digestJob) {
			if result != nil && isDigestOutcome(result.Outcome) {
				applyDigestResult(job, result)
				job.Status = digestJobStatus(result.Outcome)
				return
			}
			job.Status = "error"
			job.Outcome = digest.OutcomeAIFailed
			job.Message = "Не удалось выполнить дайджест: " + safeDigestError(err)
		})
		return
	}
	_ = ctx
	s.digestJobs.update(jobID, func(job *digestJob) {
		if result == nil {
			job.Status = "error"
			job.Outcome = digest.OutcomeAIFailed
			job.Message = "Не удалось выполнить дайджест: пустой результат."
			return
		}
		applyDigestResult(job, result)
	})
}

func isDigestOutcome(outcome string) bool {
	switch outcome {
	case digest.OutcomeSucceeded, digest.OutcomeNoPosts, digest.OutcomePartial,
		digest.OutcomeAllChannelsFailed, digest.OutcomeAIFailed, digest.OutcomeDeliveryFailed:
		return true
	default:
		return false
	}
}

func applyDigestResult(job *digestJob, result *digest.Digest) {
	job.Outcome = result.Outcome
	if job.Outcome == "" {
		job.Outcome = digest.OutcomeSucceeded
	}
	job.Status = digestJobStatus(job.Outcome)
	job.Message = result.Message
	if job.Message == "" && job.Outcome == digest.OutcomeSucceeded {
		job.Message = "Дайджест отправлен."
	}
	job.PostCount = result.PostCount
	job.ChannelCount = result.ChannelCount
	job.FailedChannels = append([]string(nil), result.FailedChannels...)
	job.MessageID = result.MessageID
	job.MessageURL = result.MessageURL
	job.SummariesSaved = result.SummariesSaved
	job.Delivered = result.Delivered
}

func digestJobStatus(outcome string) string {
	switch outcome {
	case digest.OutcomeSucceeded, digest.OutcomeNoPosts, digest.OutcomePartial:
		return "completed"
	case digest.OutcomeAllChannelsFailed, digest.OutcomeAIFailed, digest.OutcomeDeliveryFailed:
		return "error"
	default:
		return "error"
	}
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
