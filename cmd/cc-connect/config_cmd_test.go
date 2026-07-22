package main

import "testing"

func TestParseConfigInitArgs(t *testing.T) {
	got, err := parseConfigInitArgs([]string{
		"--config", "/tmp/config.toml",
		"--project", "demo",
		"--work-dir", "/tmp/work dir",
	})
	if err != nil {
		t.Fatalf("parseConfigInitArgs() error: %v", err)
	}
	if got.configPath != "/tmp/config.toml" || got.project != "demo" || got.workDir != "/tmp/work dir" {
		t.Fatalf("parseConfigInitArgs() = %#v", got)
	}
}

func TestParseConfigInitArgsRequiresEveryFlag(t *testing.T) {
	tests := [][]string{
		nil,
		{"--config", "/tmp/config.toml"},
		{"--config", "/tmp/config.toml", "--project", "demo"},
	}
	for _, args := range tests {
		if _, err := parseConfigInitArgs(args); err == nil {
			t.Fatalf("parseConfigInitArgs(%q) error = nil", args)
		}
	}
}
