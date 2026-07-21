package digest

import (
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestFormatDigestMessageGroupsPostsAndEscapesMarkdownV2(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100100,
		Title:          "Daily [Digest]!",
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	firstChannelID, err := store.Channels.Insert(&model.Channel{
		Username: "first_channel",
		Title:    "News (Official)",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("insert first channel: %v", err)
	}
	secondChannelID, err := store.Channels.Insert(&model.Channel{
		Username: "second_channel",
		Title:    "Updates",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("insert second channel: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, firstChannelID, nil); err != nil {
		t.Fatalf("assign first channel: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, secondChannelID, nil); err != nil {
		t.Fatalf("assign second channel: %v", err)
	}

	firstSummary := `AI *summary* with [literal] _characters_ and a \ slash.`
	secondSummary := "Second summary."
	posts := []model.Post{
		{
			ID:        1,
			ChannelID: firstChannelID,
			Summary:   &firstSummary,
			URL:       "https://t.me/first_channel/1",
		},
		{
			ID:        2,
			ChannelID: secondChannelID,
			Summary:   &secondSummary,
			URL:       "https://t.me/second_channel/2",
		},
		{
			ID:        3,
			ChannelID: firstChannelID,
			Summary:   &firstSummary,
			URL:       "https://t.me/first_channel/3",
		},
	}

	service := NewWithProcessor(store, nil)
	message := service.formatDigestMessage(groupID, posts)

	for _, want := range []string{
		"📋 *Daily \\[Digest\\]\\!*",
		"*News \\(Official\\)*",
		"*Updates*",
		"• [AI \\*summary\\* with \\[literal\\] \\_characters\\_ and a \\\\ slash\\.](https://t.me/first_channel/1)",
		"• [Second summary\\.](https://t.me/second_channel/2)",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("formatted message missing %q:\n%s", want, message)
		}
	}
	firstHeader := strings.Index(message, "*News \\(Official\\)*")
	secondHeader := strings.Index(message, "*Updates*")
	if firstHeader < 0 || secondHeader < 0 || firstHeader > secondHeader {
		t.Fatalf("channel groups are not ordered by first post:\n%s", message)
	}
	if strings.Count(message, "*News \\(Official\\)*") != 1 {
		t.Fatalf("first channel header repeated:\n%s", message)
	}
}

func TestSplitDigestMessageAddsPartHeadersAndPreservesAllLines(t *testing.T) {
	const limit = 120
	lines := []string{
		"📋 *Group*",
		"",
		"*Channel A*",
		"• [first summary](https://t.me/channel/1)",
		"• [second summary](https://t.me/channel/2)",
		"*Channel B*",
		"• [third summary](https://t.me/channel/3)",
	}
	original := strings.Join(lines, "\n")
	parts := splitDigestMessage(original, limit)
	if len(parts) < 2 {
		t.Fatalf("split parts = %d, want at least two", len(parts))
	}
	for index, part := range parts {
		if runeCount(part) > limit {
			t.Fatalf("part %d has %d runes, limit %d:\n%s", index+1, runeCount(part), limit, part)
		}
		wantHeader := "📋 *Group* \\(Часть " + string(rune('1'+index)) + "/"
		if !strings.Contains(part, wantHeader) {
			t.Fatalf("part %d missing part header %q:\n%s", index+1, wantHeader, part)
		}
	}
	joined := strings.Join(parts, "\n")
	for _, want := range []string{
		"• [first summary](https://t.me/channel/1)",
		"• [second summary](https://t.me/channel/2)",
		"• [third summary](https://t.me/channel/3)",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("split output lost %q:\n%s", want, joined)
		}
	}
}

func TestSplitDigestMessageKeepsOversizedPostLinksParseable(t *testing.T) {
	original := "📋 *Group*\n\n*Channel*\n• [" +
		strings.Repeat("summary ", 50) +
		"](https://t.me/channel/1)"
	parts := splitDigestMessage(original, 120)
	if len(parts) < 2 {
		t.Fatalf("split parts = %d, want multiple parts", len(parts))
	}
	for index, part := range parts {
		if runeCount(part) > 120 {
			t.Fatalf("part %d has %d runes, want <= 120", index+1, runeCount(part))
		}
		if strings.Count(part, "[") != strings.Count(part, "](") ||
			strings.Count(part, "](") != strings.Count(part, ")")-1 {
			t.Fatalf("part %d contains an unbalanced MarkdownV2 link:\n%s", index+1, part)
		}
	}
}
