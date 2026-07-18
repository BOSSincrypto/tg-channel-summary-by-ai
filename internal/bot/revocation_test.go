package bot

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/lifecycle"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
	"github.com/mymmrac/telego"
)

func TestServicePreservesTokenRevocationAcrossProductionTelegramPaths(t *testing.T) {
	const unauthorized = "401 Unauthorized"
	tests := []struct {
		name string
		call func(*Service, *fakeTelegramClient, int64, int64) error
	}{
		{
			name: "setMyCommands",
			call: func(service *Service, _ *fakeTelegramClient, _, _ int64) error {
				return service.Start()
			},
		},
		{
			name: "direct SendMessage",
			call: func(service *Service, _ *fakeTelegramClient, _, _ int64) error {
				return service.sendPlain(context.Background(), telego.ChatID{ID: 10}, "hello")
			},
		},
		{
			name: "scheduled SendMessage",
			call: func(service *Service, _ *fakeTelegramClient, groupID, _ int64) error {
				_, err := service.Deliver(context.Background(), groupID, &digest.Digest{Text: "digest"})
				return err
			},
		},
		{
			name: "answerCallbackQuery",
			call: func(service *Service, _ *fakeTelegramClient, _, _ int64) error {
				return service.HandleUpdate(context.Background(), &telego.Update{
					CallbackQuery: &telego.CallbackQuery{
						ID:   "callback",
						From: telego.User{ID: 123},
					},
				})
			},
		},
		{
			name: "getChat",
			call: func(service *Service, _ *fakeTelegramClient, _, _ int64) error {
				return service.handleGroupJoin(context.Background(), telego.Chat{ID: -100, Type: "supergroup"})
			},
		},
		{
			name: "createForumTopic",
			call: func(service *Service, _ *fakeTelegramClient, groupID, channelID int64) error {
				return service.CreateChannelTopic(context.Background(), groupID, channelID)
			},
		},
		{
			name: "editForumTopic",
			call: func(service *Service, _ *fakeTelegramClient, groupID, channelID int64) error {
				return service.RenameChannelTopic(context.Background(), groupID, channelID, "renamed")
			},
		},
		{
			name: "closeForumTopic",
			call: func(service *Service, _ *fakeTelegramClient, groupID, channelID int64) error {
				return service.RemoveChannelTopic(context.Background(), groupID, channelID)
			},
		},
		{
			name: "deleteForumTopic",
			call: func(service *Service, _ *fakeTelegramClient, groupID, channelID int64) error {
				return service.CreateChannelTopic(context.Background(), groupID, channelID)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := db.Open(":memory:")
			if err != nil {
				t.Fatalf("open database: %v", err)
			}
			defer store.Close()
			groupID, err := store.Groups.Insert(&model.Group{
				TelegramChatID: -100,
				Title:          "Forum",
				Status:         model.GroupStatusActive,
			})
			if err != nil {
				t.Fatalf("insert group: %v", err)
			}
			channelID, err := store.Channels.Insert(&model.Channel{Username: "news", Title: "News", Enabled: true})
			if err != nil {
				t.Fatalf("insert channel: %v", err)
			}
			threadID := int64(77)
			var assignmentThread *int64
			if test.name != "createForumTopic" && test.name != "deleteForumTopic" {
				assignmentThread = &threadID
			}
			if err := store.Groups.AssignChannel(groupID, channelID, assignmentThread); err != nil {
				t.Fatalf("assign channel: %v", err)
			}
			if test.name == "deleteForumTopic" {
				if _, err := store.Conn().Exec(`
					CREATE TRIGGER reject_topic_persistence
					BEFORE UPDATE OF topic_thread_id ON group_channels
					BEGIN
						SELECT RAISE(ABORT, 'persist failure');
					END`); err != nil {
					t.Fatalf("create persistence trigger: %v", err)
				}
			}
			api := &fakeTelegramClient{
				me:      &telego.User{ID: 123, Username: "DigestBot"},
				chats:   map[int64]*telego.ChatFullInfo{},
				updates: make(chan telego.Update),
			}
			if test.name == "deleteForumTopic" {
				api.deleteErr = errors.New(unauthorized)
			} else {
				api.apiErr = errors.New(unauthorized)
			}
			service := newServiceForTest(api, api)
			service.groups = store.Groups
			service.channels = store.Channels
			service.ownerID = "123"
			revoked := make(chan error, 1)
			service.SetTokenRevocationHandler(func(err error) {
				select {
				case revoked <- err:
				default:
				}
			})
			if test.name == "setMyCommands" {
				close(api.updates)
			}
			err = test.call(service, api, groupID, channelID)
			if !errors.Is(err, ErrTokenRevoked) {
				t.Fatalf("%s error = %v, want ErrTokenRevoked", test.name, err)
			}
			select {
			case callbackErr := <-revoked:
				if !errors.Is(callbackErr, ErrTokenRevoked) {
					t.Fatalf("callback error = %v, want ErrTokenRevoked", callbackErr)
				}
			default:
				t.Fatalf("%s did not notify lifecycle callback", test.name)
			}
		})
	}
}

type revocationStopper struct {
	stopped chan struct{}
}

func (s *revocationStopper) Stop() {
	select {
	case <-s.stopped:
	default:
		close(s.stopped)
	}
}

func TestTokenRevocationCallbackCoordinatesApplicationComponents(t *testing.T) {
	api := &fakeTelegramClient{
		me:     &telego.User{ID: 123, Username: "DigestBot"},
		chats:  map[int64]*telego.ChatFullInfo{},
		apiErr: errors.New("401 Unauthorized"),
	}
	service := newServiceForTest(api, api)
	server := webapp.New()
	supervisor := lifecycle.New(time.Second)
	scheduler := &revocationStopper{stopped: make(chan struct{})}
	maintenance := &revocationStopper{stopped: make(chan struct{})}
	supervisor.Add(service)
	supervisor.Add(server)
	supervisor.Add(scheduler)
	supervisor.Add(maintenance)
	service.SetTokenRevocationHandler(func(err error) {
		server.EnterTerminal(err)
		supervisor.TokenRevoked(err)
	})

	err := service.sendPlain(context.Background(), telego.ChatID{ID: 10}, "direct")
	if !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("direct send error = %v, want ErrTokenRevoked", err)
	}
	select {
	case <-supervisor.Done():
	case <-time.After(time.Second):
		t.Fatal("application lifecycle did not complete")
	}
	terminal, _ := supervisor.Terminal()
	if !terminal {
		t.Fatal("application did not enter terminal state")
	}
	select {
	case <-scheduler.stopped:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	select {
	case <-maintenance.stopped:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop")
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/health", nil))
	if recorder.Code != 503 {
		t.Fatalf("terminal health status = %d, want 503", recorder.Code)
	}
}
