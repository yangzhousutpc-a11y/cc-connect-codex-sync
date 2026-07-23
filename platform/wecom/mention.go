package wecom

import (
	"strings"
	"unicode/utf8"
)

func stripWeComAtMentions(content string, botIDs ...string) string {
	content = strings.TrimSpace(content)
	for _, botID := range botIDs {
		botID = strings.TrimSpace(botID)
		if botID == "" {
			continue
		}
		for _, prefix := range []string{"@", "＠"} {
			needle := prefix + botID
			for {
				lower := strings.ToLower(content)
				index := strings.Index(lower, strings.ToLower(needle))
				if index < 0 {
					break
				}
				content = content[:index] + content[index+len(needle):]
			}
		}
	}
	content = strings.TrimSpace(content)
	for strings.Contains(content, "  ") {
		content = strings.ReplaceAll(content, "  ", " ")
	}
	return content
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
