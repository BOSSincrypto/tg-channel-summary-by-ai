package digest

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

const telegramMessageLimit = 4096

// formatDigestMessage creates the un-split MarkdownV2 digest. Delivery splits
// this stable representation so pending messages can be retried identically
// after a process restart.
func (s *Service) formatDigestMessage(groupID int64, posts []model.Post) string {
	var builder strings.Builder
	groupTitle := escapeMarkdownV2(s.groupTitle(groupID))
	timestamp := escapeMarkdownV2(time.Now().Format("02.01.2006 15:04"))
	fmt.Fprintf(&builder, "📋 *%s* — %s", groupTitle, timestamp)

	channelTitles := make(map[int64]string)
	channelOrder := make([]int64, 0)
	channelPostCounts := make(map[int64]int)
	seenChannels := make(map[int64]struct{})
	for _, post := range posts {
		channelPostCounts[post.ChannelID]++
		if _, seen := seenChannels[post.ChannelID]; seen {
			continue
		}
		seenChannels[post.ChannelID] = struct{}{}
		channelOrder = append(channelOrder, post.ChannelID)
		channelTitles[post.ChannelID] = s.channelTitle(post.ChannelID)
	}

	fmt.Fprintf(
		&builder,
		"\n\n📊 Всего: %d %s из %d %s",
		len(posts),
		russianCountWord(len(posts), "пост", "поста", "постов"),
		len(channelOrder),
		russianCountWord(len(channelOrder), "канал", "канала", "каналов"),
	)
	for _, channelID := range channelOrder {
		fmt.Fprintf(
			&builder,
			"\n\n📢 *%s* — %d %s",
			escapeMarkdownV2(channelTitles[channelID]),
			channelPostCounts[channelID],
			russianCountWord(channelPostCounts[channelID], "пост", "поста", "постов"),
		)
		for _, post := range posts {
			if post.ChannelID != channelID {
				continue
			}
			summary := post.Summary
			if s != nil && s.database != nil && s.database.Posts != nil && post.ID > 0 {
				if refreshed, err := s.database.Posts.GetByID(post.ID); err == nil {
					summary = refreshed.Summary
				}
			}
			text := post.Text
			if summary != nil && strings.TrimSpace(*summary) != "" {
				text = *summary
			}
			text = strings.TrimSpace(text)
			if text == "" {
				text = "Без текста"
			}
			builder.WriteString("\n• ")
			builder.WriteString(escapeMarkdownV2(text))
			if url := strings.TrimSpace(post.URL); url != "" {
				builder.WriteString(" ")
				builder.WriteString("[🔗 Открыть](")
				builder.WriteString(escapeMarkdownV2URL(url))
				builder.WriteByte(')')
			}
		}
	}

	return strings.TrimSpace(builder.String())
}

func russianCountWord(value int, one, few, many string) string {
	mod100 := value % 100
	if mod100 >= 11 && mod100 <= 14 {
		return many
	}
	switch value % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}

func (s *Service) channelTitle(channelID int64) string {
	if s != nil && s.database != nil && s.database.Channels != nil && channelID > 0 {
		if channel, err := s.database.Channels.GetByID(channelID); err == nil && channel != nil {
			if title := strings.TrimSpace(channel.Title); title != "" {
				return title
			}
			if username := strings.TrimSpace(channel.Username); username != "" {
				return "@" + username
			}
		}
	}
	return fmt.Sprintf("Канал %d", channelID)
}

func escapeMarkdownV2(value string) string {
	const special = `\_*[]()~` + "`" + `>#+-=|{}.!`
	var escaped strings.Builder
	escaped.Grow(len(value))
	for _, char := range value {
		if strings.ContainsRune(special, char) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(char)
	}
	return escaped.String()
}

func escapeMarkdownV2URL(value string) string {
	var escaped strings.Builder
	escaped.Grow(len(value))
	for _, char := range value {
		if char == '\\' || char == '(' || char == ')' {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(char)
	}
	return escaped.String()
}

// SplitDigestMessage returns one or more complete MarkdownV2 messages. It
// packs whole lines whenever possible, keeping the digest header and a
// Telegram-safe Part X/N marker on every split part.
func SplitDigestMessage(message string) []string {
	return splitDigestMessage(message, telegramMessageLimit)
}

func splitDigestMessage(message string, limit int) []string {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil
	}
	if limit <= 0 {
		limit = telegramMessageLimit
	}
	if runeCount(message) <= limit {
		return []string{message}
	}

	lines := strings.Split(message, "\n")
	header := lines[0]
	body := strings.Join(lines[1:], "\n")
	total := 1
	var chunks []string
	for attempt := 0; attempt < 8; attempt++ {
		partHeader := formatPartHeader(header, total, total)
		bodyLimit := limit - runeCount(partHeader) - 2
		if bodyLimit < 1 {
			bodyLimit = 1
		}
		chunks = packDigestLines(body, bodyLimit)
		nextTotal := len(chunks)
		if nextTotal == 0 {
			nextTotal = 1
		}
		if nextTotal == total {
			break
		}
		total = nextTotal
	}
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	total = len(chunks)
	parts := make([]string, 0, total)
	for index, chunk := range chunks {
		partHeader := formatPartHeader(header, index+1, total)
		part := partHeader
		if chunk != "" {
			part += "\n\n" + chunk
		}
		parts = append(parts, part)
	}
	return parts
}

func formatPartHeader(header string, part, total int) string {
	return fmt.Sprintf("%s \\(Часть %d/%d\\)", header, part, total)
}

func packDigestLines(body string, limit int) []string {
	if body == "" {
		return []string{""}
	}
	lines := strings.Split(body, "\n")
	chunks := make([]string, 0, len(lines))
	var current strings.Builder
	flush := func() {
		chunks = append(chunks, current.String())
		current.Reset()
	}
	for _, line := range lines {
		lineRunes := []rune(line)
		if len(lineRunes) > limit {
			if current.Len() > 0 {
				flush()
			}
			safeLines := splitOversizedDigestLine(line, limit)
			for _, safeLine := range safeLines {
				chunks = append(chunks, safeLine)
			}
			continue
		}
		candidateLength := runeCount(current.String())
		if current.Len() > 0 {
			candidateLength++
		}
		candidateLength += len(lineRunes)
		if current.Len() > 0 && candidateLength > limit {
			flush()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(line)
	}
	if current.Len() > 0 || len(chunks) == 0 {
		flush()
	}
	return chunks
}

func splitOversizedDigestLine(line string, limit int) []string {
	const compactLinkMarker = " [🔗 Открыть]("
	if marker := strings.LastIndex(line, compactLinkMarker); marker >= 0 &&
		strings.HasSuffix(line, ")") {
		link := line[marker+1:]
		if runeCount(link) <= limit {
			summaryChunks := splitOversizedDigestLine(line[:marker], limit)
			if len(summaryChunks) == 0 {
				return []string{link}
			}
			last := len(summaryChunks) - 1
			if runeCount(summaryChunks[last])+1+runeCount(link) <= limit {
				summaryChunks[last] += " " + link
			} else {
				summaryChunks = append(summaryChunks, link)
			}
			return summaryChunks
		}
	}

	const linkPrefix = "• ["
	if strings.HasPrefix(line, linkPrefix) {
		marker := strings.Index(line[len(linkPrefix):], "](")
		if marker >= 0 {
			labelEnd := len(linkPrefix) + marker
			urlStart := labelEnd + 2
			if strings.HasSuffix(line, ")") && urlStart < len(line)-1 {
				label := line[len(linkPrefix):labelEnd]
				url := line[urlStart : len(line)-1]
				overhead := runeCount("• [](" + url + ")")
				if overhead < limit {
					labelRunes := []rune(label)
					chunkSize := limit - overhead
					result := make([]string, 0, (len(labelRunes)/chunkSize)+1)
					for len(labelRunes) > 0 {
						size := chunkSize
						if len(labelRunes) < size {
							size = len(labelRunes)
						}
						result = append(result, "• ["+string(labelRunes[:size])+"]("+url+")")
						labelRunes = labelRunes[size:]
					}
					return result
				}
			}
		}
	}
	result := make([]string, 0, (runeCount(line)/limit)+1)
	lineRunes := []rune(line)
	for len(lineRunes) > 0 {
		size := limit
		if len(lineRunes) < size {
			size = len(lineRunes)
		}
		result = append(result, string(lineRunes[:size]))
		lineRunes = lineRunes[size:]
	}
	return result
}

func runeCount(value string) int {
	return utf8.RuneCountInString(value)
}
