package webapp

import (
	"errors"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
)

type typedDigestRunnerFixture struct {
	result *digest.Digest
	err    error
}

func (f typedDigestRunnerFixture) GenerateManual(int64) (*digest.Digest, error) {
	return f.result, f.err
}

func (f typedDigestRunnerFixture) GenerateManualResult(int64) (*digest.Digest, error) {
	return f.result, f.err
}

func TestDigestJobExposesTypedTerminalOutcomes(t *testing.T) {
	tests := []struct {
		name           string
		outcome        string
		status         string
		message        string
		failedChannels []string
		messageID      *int64
		summariesSaved bool
		delivered      bool
		wantMessageID  *int64
	}{
		{
			name: "succeeded", outcome: digest.OutcomeSucceeded, status: "completed",
			message: "Дайджест отправлен.", messageID: int64Ptr(91), delivered: true,
		},
		{
			name: "no posts", outcome: digest.OutcomeNoPosts, status: "completed",
			message: "Нет новых постов для дайджеста.",
		},
		{
			name: "partial", outcome: digest.OutcomePartial, status: "completed",
			message: "Дайджест отправлен частично.", failedChannels: []string{"@broken"},
			messageID: int64Ptr(92), delivered: true,
		},
		{
			name: "all channels failed", outcome: digest.OutcomeAllChannelsFailed, status: "error",
			message: "Все каналы недоступны.", failedChannels: []string{"@one", "@two"},
		},
		{
			name: "ai failed", outcome: digest.OutcomeAIFailed, status: "error",
			message: "Ошибка AI.",
		},
		{
			name: "delivery failed", outcome: digest.OutcomeDeliveryFailed, status: "error",
			message: "Telegram недоступен.", messageID: nil, summariesSaved: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{digestJobs: newDigestJobStore(), digestRunner: typedDigestRunnerFixture{
				result: &digest.Digest{
					GroupID: 7, PostCount: 3, ChannelCount: 2, Outcome: test.outcome,
					Message: test.message, FailedChannels: test.failedChannels,
					MessageID: test.messageID, SummariesSaved: test.summariesSaved, Delivered: test.delivered,
				},
			}}
			job := server.digestJobs.create(7)
			if job == nil {
				t.Fatal("create returned nil")
			}
			server.runDigestJob(nil, job.ID, 7)
			got := server.digestJobs.get(job.ID)
			if got == nil {
				t.Fatal("job disappeared")
			}
			if got.Status != test.status || got.Outcome != test.outcome {
				t.Fatalf("job = %+v, want status=%q outcome=%q", got, test.status, test.outcome)
			}
			if got.Message != test.message || got.PostCount != 3 || got.ChannelCount != 2 {
				t.Fatalf("job fields = %+v", got)
			}
			if len(got.FailedChannels) != len(test.failedChannels) {
				t.Fatalf("failed channels = %v, want %v", got.FailedChannels, test.failedChannels)
			}
			if got.SummariesSaved != test.summariesSaved || got.Delivered != test.delivered {
				t.Fatalf("delivery fields = saved:%v delivered:%v", got.SummariesSaved, got.Delivered)
			}
			if test.messageID == nil && got.MessageID != nil {
				t.Fatalf("message ID = %v, want nil", got.MessageID)
			}
			if test.messageID != nil && (got.MessageID == nil || *got.MessageID != *test.messageID) {
				t.Fatalf("message ID = %v, want %v", got.MessageID, *test.messageID)
			}
		})
	}
}

func TestDigestJobUsesTypedResultWhenRunnerReturnsTerminalError(t *testing.T) {
	messageID := int64(12)
	server := &Server{
		digestJobs: newDigestJobStore(),
		digestRunner: typedDigestRunnerFixture{
			result: &digest.Digest{
				Outcome: digest.OutcomeDeliveryFailed, Message: "send failed",
				SummariesSaved: true, MessageID: &messageID,
			},
			err: errors.New("send failed"),
		},
	}
	job := server.digestJobs.create(8)
	server.runDigestJob(nil, job.ID, 8)
	got := server.digestJobs.get(job.ID)
	if got.Outcome != digest.OutcomeDeliveryFailed || got.Status != "error" {
		t.Fatalf("outcome = %q status=%q, want delivery_failed/error", got.Outcome, got.Status)
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}
