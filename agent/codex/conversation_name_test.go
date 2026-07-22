package codex

import "testing"

func TestFormatConversationName(t *testing.T) {
	tests := map[string]string{
		"项目A":           "[Codex] 项目A",
		"  项目A  ":       "[Codex] 项目A",
		"[Codex] 项目A":   "[Codex] 项目A",
		"[CODEX]   项目A": "[Codex] 项目A",
		"[Codex]":       "[Codex]",
		"":              "",
	}

	agent := &Agent{}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			if got := agent.FormatConversationName(input); got != want {
				t.Fatalf("FormatConversationName(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestFormatConversationNameForPlatform(t *testing.T) {
	tests := []struct {
		platform string
		input    string
		want     string
	}{
		{platform: "feishu", input: "项目A", want: "[飞书-Codex] 项目A"},
		{platform: "weixin", input: "项目A", want: "[微信-Codex] 项目A"},
		{platform: "slack", input: "项目A", want: "[Codex] 项目A"},
		{platform: "feishu", input: "[Codex] 项目A", want: "[飞书-Codex] 项目A"},
		{platform: "weixin", input: "[微信-Codex] 项目A", want: "[微信-Codex] 项目A"},
		{platform: "feishu", input: "🪽 [飞书-Codex] 项目A", want: "[飞书-Codex] 项目A"},
		{platform: "weixin", input: "💬 [微信-Codex] 项目A", want: "[微信-Codex] 项目A"},
		{platform: "feishu", input: "", want: ""},
	}

	agent := &Agent{}
	for _, tt := range tests {
		t.Run(tt.platform+"/"+tt.input, func(t *testing.T) {
			if got := agent.FormatConversationNameForPlatform(tt.platform, tt.input); got != tt.want {
				t.Fatalf("FormatConversationNameForPlatform(%q, %q) = %q, want %q", tt.platform, tt.input, got, tt.want)
			}
		})
	}
}
