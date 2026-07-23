package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/config"
)

func writeWeComCLIConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[[projects]]
name = "alpha"

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "/tmp/alpha"

[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
app_id = "keep"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestRunWeComSetupSavesCredentialsWithoutPrintingSecret(t *testing.T) {
	path := writeWeComCLIConfig(t)
	var stdout, stderr bytes.Buffer
	secret := "super-sensitive-secret"

	err := runWeComCommand([]string{
		"setup",
		"--config", path,
		"--project", "alpha",
		"--bot-id", "bot-12345678",
		"--bot-secret", secret,
		"--allow-from", "zhangsan,lisi",
	}, &stdout, &stderr, func() (string, error) {
		t.Fatal("bot ID reader should not be called when --bot-id is set")
		return "", nil
	}, func() (string, error) {
		t.Fatal("password reader should not be called when --bot-secret is set")
		return "", nil
	})
	if err != nil {
		t.Fatalf("runWeComCommand returned error: %v, stderr=%s", err, stderr.String())
	}

	output := stdout.String() + stderr.String()
	if strings.Contains(output, secret) {
		t.Fatalf("command output leaked bot secret: %q", output)
	}
	if strings.Contains(output, "bot-12345678") {
		t.Fatalf("command output exposed full bot id: %q", output)
	}
	if !strings.Contains(output, "5678") {
		t.Fatalf("command output missing bot id suffix: %q", output)
	}
	if !strings.Contains(output, "重启服务") || !strings.Contains(output, "@机器人") {
		t.Fatalf("command output missing next-step guidance: %q", output)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load saved config: %v", err)
	}
	if len(cfg.Projects[0].Platforms) != 2 {
		t.Fatalf("platform count = %d, want 2", len(cfg.Projects[0].Platforms))
	}
	p := cfg.Projects[0].Platforms[1]
	if p.Type != "wecom" {
		t.Fatalf("platform type = %q, want wecom", p.Type)
	}
	if got, _ := p.Options["mode"].(string); got != "websocket" {
		t.Fatalf("mode = %q, want websocket", got)
	}
	if got, _ := p.Options["bot_secret"].(string); got != secret {
		t.Fatalf("bot_secret was not saved")
	}
	if got, _ := p.Options["allow_from"].(string); got != "zhangsan,lisi" {
		t.Fatalf("allow_from = %q, want zhangsan,lisi", got)
	}
}

func TestRunWeComBindReadsMissingSecretWithoutEchoingIt(t *testing.T) {
	path := writeWeComCLIConfig(t)
	var stdout, stderr bytes.Buffer
	secret := "read-from-terminal"
	readCalls := 0

	err := runWeComCommand([]string{
		"bind",
		"--config", path,
		"--project", "alpha",
		"--bot-id", "bot-abcd",
	}, &stdout, &stderr, func() (string, error) {
		t.Fatal("bot ID reader should not be called when --bot-id is set")
		return "", nil
	}, func() (string, error) {
		readCalls++
		return secret, nil
	})
	if err != nil {
		t.Fatalf("runWeComCommand returned error: %v", err)
	}
	if readCalls != 1 {
		t.Fatalf("password reader calls = %d, want 1", readCalls)
	}
	if output := stdout.String() + stderr.String(); strings.Contains(output, secret) {
		t.Fatalf("command output leaked terminal-read secret: %q", output)
	}
}

func TestRunWeComCommandRejectsEmptyCredentials(t *testing.T) {
	path := writeWeComCLIConfig(t)
	var stdout, stderr bytes.Buffer

	err := runWeComCommand([]string{
		"setup",
		"--config", path,
		"--project", "alpha",
		"--bot-id", " ",
	}, &stdout, &stderr, func() (string, error) {
		return "", nil
	}, func() (string, error) {
		return "", nil
	})
	if err == nil {
		t.Fatal("runWeComCommand returned nil error for empty credentials")
	}
}

func TestRunWeComSetupPromptsForBotIDAndSecretWhenOmitted(t *testing.T) {
	path := writeWeComCLIConfig(t)
	var stdout, stderr bytes.Buffer
	botID := "interactive-bot-9876"
	secret := "interactive-secret"
	botIDCalls := 0
	secretCalls := 0

	err := runWeComCommand([]string{
		"setup",
		"--config", path,
		"--project", "alpha",
	}, &stdout, &stderr, func() (string, error) {
		botIDCalls++
		return botID, nil
	}, func() (string, error) {
		secretCalls++
		return secret, nil
	})
	if err != nil {
		t.Fatalf("runWeComCommand returned error: %v", err)
	}
	if botIDCalls != 1 || secretCalls != 1 {
		t.Fatalf("reader calls = (%d, %d), want (1, 1)", botIDCalls, secretCalls)
	}
	output := stdout.String() + stderr.String()
	if strings.Contains(output, botID) || strings.Contains(output, secret) {
		t.Fatalf("interactive credentials leaked in output: %q", output)
	}
	if !strings.Contains(output, "9876") {
		t.Fatalf("output missing Bot ID suffix: %q", output)
	}
}

func TestRunWeComSetupInvalidPlatformIndexDoesNotModifyConfig(t *testing.T) {
	path := writeWeComCLIConfig(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config before command: %v", err)
	}
	var stdout, stderr bytes.Buffer

	err = runWeComCommand([]string{
		"setup",
		"--config", path,
		"--project", "alpha",
		"--platform-index", "2",
		"--bot-id", "bot-abcd",
		"--bot-secret", "secret",
	}, &stdout, &stderr, nil, nil)
	if err == nil {
		t.Fatal("runWeComCommand returned nil error for invalid platform index")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config after command: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("invalid platform index modified config\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestRunWeComSubcommandHelpReturnsSuccess(t *testing.T) {
	for _, command := range []string{"setup", "bind"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runWeComCommand([]string{command, "--help"}, &stdout, &stderr, nil, nil)
			if err != nil {
				t.Fatalf("runWeComCommand returned error for --help: %v", err)
			}
			if strings.Contains(stderr.String(), "Error:") {
				t.Fatalf("--help output contains error prefix: %q", stderr.String())
			}
		})
	}
}
