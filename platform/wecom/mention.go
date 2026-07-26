package wecom

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// stripWeComAtMentions removes only exact configured bot mentions from the
// leading mention run in a WeCom callback. WeCom does not include a mention
// list, so an unconfigured display name must never be guessed: it may belong
// to another member of the group.
func stripWeComAtMentions(content string, botNames ...string) string {
	content = strings.TrimSpace(content)
	botNames = normalizedWeComBotNames(botNames)
	if len(botNames) == 0 || content == "" {
		return content
	}

	var out strings.Builder
	for content != "" && (strings.HasPrefix(content, "@") || strings.HasPrefix(content, "＠")) {
		if end, ok := matchedWeComBotMention(content, botNames); ok {
			content = strings.TrimLeftFunc(content[end:], unicode.IsSpace)
			continue
		}

		// Keep an unconfigured member mention verbatim, then inspect a following
		// leading mention. This allows "@成员 @机器人 正文" while never deleting
		// @成员 merely because it appears before the bot.
		end := strings.IndexFunc(content, unicode.IsSpace)
		if end < 0 {
			break
		}
		out.WriteString(content[:end])
		out.WriteByte(' ')
		content = strings.TrimLeftFunc(content[end:], unicode.IsSpace)
	}
	out.WriteString(content)
	return strings.TrimSpace(out.String())
}

func normalizedWeComBotNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimLeft(strings.TrimSpace(name), "@＠")
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, name)
	}
	return result
}

func matchedWeComBotMention(content string, botNames []string) (int, bool) {
	if content == "" || (!strings.HasPrefix(content, "@") && !strings.HasPrefix(content, "＠")) {
		return 0, false
	}
	prefixLen := len("@")
	if strings.HasPrefix(content, "＠") {
		prefixLen = len("＠")
	}
	for _, name := range botNames {
		end := prefixLen + len(name)
		if len(content) < end || !strings.EqualFold(content[prefixLen:end], name) {
			continue
		}
		if len(content) == end {
			return end, true
		}
		next, _ := utf8.DecodeRuneInString(content[end:])
		if unicode.IsSpace(next) || next == '@' || next == '＠' {
			return end, true
		}
	}
	return 0, false
}

func splitByBytes(content string, maxBytes int) []string {
	if maxBytes <= 0 || len(content) <= maxBytes {
		return []string{content}
	}
	chunks := make([]string, 0, len(content)/maxBytes+1)
	for len(content) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
		if cut == 0 {
			_, cut = utf8.DecodeRuneInString(content)
		}
		chunks = append(chunks, content[:cut])
		content = content[cut:]
	}
	if content != "" {
		chunks = append(chunks, content)
	}
	return chunks
}
