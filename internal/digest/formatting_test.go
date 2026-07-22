package digest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestFormatDigestMessageGroupsPostsWithCountsCompactLinksAndEscaping(t *testing.T) {
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
		"📋 *Daily \\[Digest\\]\\!* — ",
		"Всего: 3 поста из 2 канала",
		"📢 *News \\(Official\\)* — 2 поста",
		"📢 *Updates* — 1 пост",
		"• AI \\*summary\\* with \\[literal\\] \\_characters\\_ and a \\\\ slash\\.",
		"[🔗 Открыть](https://t.me/first_channel/1)",
		"[🔗 Открыть](https://t.me/second_channel/2)",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("formatted message missing %q:\n%s", want, message)
		}
	}
	firstHeader := strings.Index(message, "📢 *News \\(Official\\)* — 2 поста")
	secondHeader := strings.Index(message, "📢 *Updates* — 1 пост")
	if firstHeader < 0 || secondHeader < 0 || firstHeader > secondHeader {
		t.Fatalf("channel groups are not ordered by first post:\n%s", message)
	}
	if strings.Count(message, "📢 *News \\(Official\\)* — 2 поста") != 1 {
		t.Fatalf("first channel header repeated:\n%s", message)
	}
	if strings.Contains(message, "Обновлено:") {
		t.Fatalf("digest contains a second timestamp:\n%s", message)
	}
	if strings.Contains(message, "[AI \\*summary\\*") {
		t.Fatalf("summary text must not be the clickable link:\n%s", message)
	}
}

func TestFormatDigestMessageKeepsNoURLPostReadableWithoutLink(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100101, Title: "Fallback"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{
		Username: "fallback", Title: "Fallback channel", Enabled: true,
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	summary := "Readable summary without source."
	message := NewWithProcessor(store, nil).formatDigestMessage(groupID, []model.Post{{
		ChannelID: channelID, Summary: &summary,
	}})

	if !strings.Contains(message, "• Readable summary without source\\.") {
		t.Fatalf("missing plain summary:\n%s", message)
	}
	if strings.Contains(message, "Открыть") || strings.Contains(message, "🔗") ||
		strings.Contains(message, "](") {
		t.Fatalf("no-URL post contains a broken link:\n%s", message)
	}
}

func TestRussianCountWordUsesCorrectForms(t *testing.T) {
	tests := []struct {
		value int
		want  string
	}{
		{value: 1, want: "пост"},
		{value: 2, want: "поста"},
		{value: 5, want: "постов"},
		{value: 11, want: "постов"},
		{value: 21, want: "пост"},
	}
	for _, test := range tests {
		if got := russianCountWord(test.value, "пост", "поста", "постов"); got != test.want {
			t.Errorf("russianCountWord(%d) = %q, want %q", test.value, got, test.want)
		}
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

func TestSplitDigestMessageKeepsCompactPostLinksBalanced(t *testing.T) {
	original := "📋 *Group*\n\n📢 *Channel* — 1 пост\n• " +
		strings.Repeat("x", 87) +
		" [🔗 Открыть](https://t.me/channel/1)"
	parts := splitDigestMessage(original, 120)
	if len(parts) < 2 {
		t.Fatalf("split parts = %d, want multiple parts", len(parts))
	}
	foundLink := false
	for index, part := range parts {
		if runeCount(part) > 120 {
			t.Fatalf("part %d has %d runes, want <= 120", index+1, runeCount(part))
		}
		if strings.Count(part, "[🔗 Открыть](") != strings.Count(part, "https://t.me/channel/1") {
			t.Fatalf("part %d contains an incomplete compact link:\n%s", index+1, part)
		}
		if strings.Contains(part, "[🔗 Открыть](") {
			foundLink = true
			if !strings.Contains(part, "[🔗 Открыть](https://t.me/channel/1)") {
				t.Fatalf("part %d contains a malformed compact link:\n%s", index+1, part)
			}
		}
	}
	if !foundLink {
		t.Fatalf("split output lost compact link:\n%s", strings.Join(parts, "\n"))
	}
}

func TestFormatAndSplitDigestEscapesSensitiveURLAndPreservesTarget(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100102,
		Title:          "URL regression",
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{
		Username: "url_regression",
		Title:    "URL regression channel",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	const targetURL = `https://example.test/posts/(release)\candidate\final`
	const wantEscapedURL = `https://example.test/posts/\(release\)\\candidate\\final`
	summary := strings.Repeat("Длинное резюме проверяет перенос компактной ссылки. ", 20)
	message := NewWithProcessor(store, nil).formatDigestMessage(groupID, []model.Post{{
		ChannelID: channelID,
		Summary:   &summary,
		URL:       targetURL,
	}})
	wantLink := "[🔗 Открыть](" + wantEscapedURL + ")"
	if !strings.Contains(message, wantLink) {
		t.Fatalf("formatted message missing escaped URL link %q:\n%s", wantLink, message)
	}

	const limit = 160
	parts := splitDigestMessage(message, limit)
	if len(parts) < 2 {
		t.Fatalf("split parts = %d, want multiple parts", len(parts))
	}

	var targets []string
	for index, part := range parts {
		if runeCount(part) > limit {
			t.Fatalf("part %d has %d runes, limit %d:\n%s", index+1, runeCount(part), limit, part)
		}
		for _, line := range strings.Split(part, "\n") {
			if !strings.Contains(line, "[🔗 Открыть](") {
				continue
			}
			target, parseErr := parseCompactMarkdownV2Link(line)
			if parseErr != nil {
				t.Fatalf("part %d link is not balanced or parseable: %v\n%s", index+1, parseErr, part)
			}
			targets = append(targets, target)
		}
	}
	if len(targets) != 1 || targets[0] != targetURL {
		t.Fatalf("split link targets = %q, want one complete target %q:\n%s", targets, targetURL, strings.Join(parts, "\n"))
	}
}

func parseCompactMarkdownV2Link(line string) (string, error) {
	const marker = "[🔗 Открыть]("
	start := strings.Index(line, marker)
	if start < 0 {
		return "", fmt.Errorf("compact link marker is missing")
	}
	encoded := line[start+len(marker):]
	if !strings.HasSuffix(encoded, ")") {
		return "", fmt.Errorf("compact link has no closing parenthesis")
	}
	encoded = encoded[:len(encoded)-1]

	var target strings.Builder
	for index := 0; index < len(encoded); index++ {
		switch encoded[index] {
		case '\\':
			if index+1 >= len(encoded) {
				return "", fmt.Errorf("compact link ends with an escape")
			}
			target.WriteByte(encoded[index+1])
			index++
		case '(', ')':
			return "", fmt.Errorf("compact link contains an unescaped parenthesis")
		default:
			target.WriteByte(encoded[index])
		}
	}
	return target.String(), nil
}
