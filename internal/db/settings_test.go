package db

import (
	"errors"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestApplySettingsTransactionUpdatesGlobalAndGroupsAtomically(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	firstID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1001, Title: "first"})
	if err != nil {
		t.Fatalf("insert first group: %v", err)
	}
	secondID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1002, Title: "second"})
	if err != nil {
		t.Fatalf("insert second group: %v", err)
	}
	if err := store.Config.Set("webapp_settings", `{"digest_time":"21:00"}`); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	updatedVersion, err := store.ApplySettingsTransaction(SettingsUpdate{
		ConfigKey:       "webapp_settings",
		ConfigValue:     `{"digest_time":"09:30"}`,
		ExpectedVersion: 1,
		GroupSettings: []*model.GroupSettings{
			{GroupID: firstID, DigestTime: "09:30", Timezone: "UTC"},
			{GroupID: secondID, DigestTime: "09:30", Timezone: "UTC"},
		},
		PendingKey:   "webapp_settings_sync_pending",
		PendingValue: `{"digest_time":"09:30"}`,
	})
	if err != nil {
		t.Fatalf("apply settings transaction: %v", err)
	}
	if updatedVersion != 2 {
		t.Fatalf("updated version = %d, want 2", updatedVersion)
	}

	value, version, err := store.Config.GetWithVersion("webapp_settings")
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if value != `{"digest_time":"09:30"}` || version != 2 {
		t.Fatalf("settings = %q version %d, want updated value version 2", value, version)
	}
	for _, groupID := range []int64{firstID, secondID} {
		settings, err := store.Groups.GetGroupSettings(groupID)
		if err != nil {
			t.Fatalf("load group %d settings: %v", groupID, err)
		}
		if settings.DigestTime != "09:30" || settings.Timezone != "UTC" {
			t.Fatalf("group %d settings = %+v, want updated values", groupID, settings)
		}
	}
	pending, err := store.Config.Get("webapp_settings_sync_pending")
	if err != nil {
		t.Fatalf("load pending sync: %v", err)
	}
	if pending != `{"digest_time":"09:30"}` {
		t.Fatalf("pending sync = %q", pending)
	}
}

func TestSetOptimisticRejectsNonPositiveAndMissingVersions(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	for _, version := range []int64{0, -1} {
		if _, err := store.Config.SetOptimistic("missing", "value", version); !errors.Is(err, ErrConflict) {
			t.Fatalf("version %d error = %v, want conflict", version, err)
		}
	}
}

func TestApplySettingsTransactionRollsBackGlobalAndGroupWrites(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	firstID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1003, Title: "first"})
	if err != nil {
		t.Fatalf("insert first group: %v", err)
	}
	secondID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1004, Title: "second"})
	if err != nil {
		t.Fatalf("insert second group: %v", err)
	}
	if err := store.Config.Set("webapp_settings", `{"digest_time":"21:00"}`); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	if _, err := store.Conn().Exec(`
		CREATE TRIGGER reject_second_settings
		BEFORE INSERT ON group_settings
		WHEN NEW.group_id = ` + jsonInt(secondID) + `
		BEGIN
			SELECT RAISE(ABORT, 'injected group settings failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	_, err = store.ApplySettingsTransaction(SettingsUpdate{
		ConfigKey:       "webapp_settings",
		ConfigValue:     `{"digest_time":"09:30"}`,
		ExpectedVersion: 1,
		GroupSettings: []*model.GroupSettings{
			{GroupID: firstID, DigestTime: "09:30", Timezone: "UTC"},
			{GroupID: secondID, DigestTime: "09:30", Timezone: "UTC"},
		},
		PendingKey:   "webapp_settings_sync_pending",
		PendingValue: `{"digest_time":"09:30"}`,
	})
	if err == nil {
		t.Fatal("settings transaction succeeded despite injected failure")
	}

	value, version, err := store.Config.GetWithVersion("webapp_settings")
	if err != nil {
		t.Fatalf("load rolled back settings: %v", err)
	}
	if value != `{"digest_time":"21:00"}` || version != 1 {
		t.Fatalf("settings after rollback = %q version %d, want original value version 1", value, version)
	}
	firstSettings, err := store.Groups.GetGroupSettings(firstID)
	if err != nil {
		t.Fatalf("load first rolled back settings: %v", err)
	}
	if firstSettings.DigestTime != "21:00" || firstSettings.Timezone != "Europe/Moscow" {
		t.Fatalf("first group settings after rollback = %+v", firstSettings)
	}
	if _, err := store.Config.Get("webapp_settings_sync_pending"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending sync after rollback = %v, want not found", err)
	}
}

func jsonInt(value int64) string {
	if value < 0 {
		return "-" + jsonInt(-value)
	}
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
