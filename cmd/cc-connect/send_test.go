package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/config"
	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestRunSendHelperProcess(t *testing.T) {
	if os.Getenv("CC_TEST_RUN_SEND_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		t.Fatal("missing helper argument separator")
	}
	runSend(os.Args[separator+1:])
}

func startCountingSendServer(t *testing.T, dataDir string, activeSessions int) *atomic.Int32 {
	t.Helper()
	if activeSessions < 1 {
		t.Fatal("test server requires at least one active session")
	}
	runDir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(runDir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Test-Active-Sessions", strconv.Itoa(activeSessions))
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
	})
	return &sends
}

func helperProcessEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[name]; !replaced {
			env = append(env, entry)
		}
	}
	for name, value := range overrides {
		env = append(env, name+"="+value)
	}
	return env
}

func setTestSendSession(t *testing.T) {
	t.Helper()
	t.Setenv("CC_SESSION_KEY", "test:session")
	t.Setenv("CODEX_THREAD_ID", "")
}

func TestRunSend_MissingContextNeverCallsAPIWithActiveSessions(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		activeSessions int
		threadID       string
	}{
		{name: "missing_thread_one_active_session", activeSessions: 1},
		{name: "invalid_thread_one_active_session", activeSessions: 1, threadID: "not-a-trusted-thread-id"},
		{name: "missing_thread_multiple_active_sessions", activeSessions: 2},
		{name: "invalid_thread_multiple_active_sessions", activeSessions: 2, threadID: "not-a-trusted-thread-id"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dataDir, err := os.MkdirTemp("", "cc-send-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
			sends := startCountingSendServer(t, dataDir, testCase.activeSessions)
			cmd := exec.Command(os.Args[0],
				"-test.run=^TestRunSendHelperProcess$",
				"--",
				"--data-dir", dataDir,
				"--message", "hello",
			)
			cmd.Env = helperProcessEnv(map[string]string{
				"CC_TEST_RUN_SEND_HELPER": "1",
				"CC_PROJECT":              "",
				"CC_SESSION_KEY":          "",
				"CODEX_THREAD_ID":         testCase.threadID,
			})
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("send unexpectedly succeeded with %d active sessions: %s", testCase.activeSessions, output)
			}
			if testCase.threadID != "" && strings.Contains(string(output), testCase.threadID) {
				t.Fatalf("CLI error leaked invalid thread value: %s", output)
			}
			if got := sends.Load(); got != 0 {
				t.Fatalf("API send calls = %d with %d active sessions, want 0", got, testCase.activeSessions)
			}
		})
	}
}

func TestParseSendArgs_AttachmentsWithoutMessage(t *testing.T) {
	setTestSendSession(t)
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "chart.png")
	docPath := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(imgPath, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := os.WriteFile(docPath, []byte("hello report"), 0o644); err != nil {
		t.Fatalf("write doc: %v", err)
	}

	req, dataDir, err := parseSendArgs([]string{"--image", imgPath, "--file", docPath})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if dataDir != "" {
		t.Fatalf("dataDir = %q, want empty", dataDir)
	}
	if req.Message != "" {
		t.Fatalf("message = %q, want empty", req.Message)
	}
	if len(req.Images) != 1 {
		t.Fatalf("images len = %d, want 1", len(req.Images))
	}
	if req.Images[0].FileName != "chart.png" {
		t.Fatalf("image filename = %q, want chart.png", req.Images[0].FileName)
	}
	if req.Images[0].MimeType != "image/png" {
		t.Fatalf("image mime = %q, want image/png", req.Images[0].MimeType)
	}
	if len(req.Files) != 1 {
		t.Fatalf("files len = %d, want 1", len(req.Files))
	}
	if req.Files[0].FileName != "report.txt" {
		t.Fatalf("file filename = %q, want report.txt", req.Files[0].FileName)
	}
}

func TestParseSendArgs_RequiresMessageOrAttachment(t *testing.T) {
	setTestSendSession(t)
	_, _, err := parseSendArgs(nil)
	if err == nil {
		t.Fatal("expected error for empty send args")
	}
}

func TestParseSendArgs_UsesSessionEnvFallback(t *testing.T) {
	t.Setenv("CC_PROJECT", "demo")
	t.Setenv("CC_SESSION_KEY", "telegram:123:456")
	t.Setenv("CODEX_THREAD_ID", "not-a-trusted-thread-id")

	dir := t.TempDir()
	imgPath := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(imgPath, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	req, _, err := parseSendArgs([]string{"--image", imgPath})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if req.Project != "demo" {
		t.Fatalf("project = %q, want demo", req.Project)
	}
	if req.SessionKey != "telegram:123:456" {
		t.Fatalf("session = %q, want telegram:123:456", req.SessionKey)
	}
	if req.AgentThreadID != "" {
		t.Fatalf("agent thread ID = %q, want empty when CC_SESSION_KEY is set", req.AgentThreadID)
	}
}

func TestParseSendArgs_UsesCodexThreadFallbackWithoutSession(t *testing.T) {
	t.Setenv("CC_PROJECT", "")
	t.Setenv("CC_SESSION_KEY", "")
	t.Setenv("CODEX_THREAD_ID", "00000000-0000-7000-8000-000000000001")

	req, _, err := parseSendArgs([]string{"--message", "hello"})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if req.SessionKey != "" {
		t.Fatalf("session = %q, want empty", req.SessionKey)
	}
	if req.AgentThreadID != "00000000-0000-7000-8000-000000000001" {
		t.Fatalf("agent thread ID = %q, want Codex thread fallback", req.AgentThreadID)
	}
}

func TestParseSendArgs_ExplicitSessionBeatsCodexThreadFallback(t *testing.T) {
	t.Setenv("CC_SESSION_KEY", "feishu:env-session")
	t.Setenv("CODEX_THREAD_ID", "not-a-trusted-thread-id")

	req, _, err := parseSendArgs([]string{"--session", "wecom:explicit-session", "--message", "hello"})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if req.SessionKey != "wecom:explicit-session" {
		t.Fatalf("session = %q, want explicit session", req.SessionKey)
	}
	if req.AgentThreadID != "" {
		t.Fatalf("agent thread ID = %q, want empty when --session is set", req.AgentThreadID)
	}
}

func TestParseSendArgs_RejectsInvalidCodexThreadFallback(t *testing.T) {
	t.Setenv("CC_SESSION_KEY", "")
	t.Setenv("CODEX_THREAD_ID", "not-a-trusted-thread-id")

	_, _, err := parseSendArgs([]string{"--message", "hello"})
	if err == nil {
		t.Fatal("expected invalid Codex thread fallback to fail closed")
	}
	if strings.Contains(err.Error(), "not-a-trusted-thread-id") {
		t.Fatalf("error leaked invalid thread value: %v", err)
	}
}

func TestParseSendArgs_RequiresSessionOrValidCodexThread(t *testing.T) {
	t.Setenv("CC_SESSION_KEY", "")
	t.Setenv("CODEX_THREAD_ID", "")

	_, _, err := parseSendArgs([]string{"--message", "hello"})
	if err == nil {
		t.Fatal("expected missing session context to fail closed")
	}
}

func TestParseSendArgs_WorkDirOption(t *testing.T) {
	setTestSendSession(t)
	workDir := t.TempDir()

	req, _, err := parseSendArgs([]string{"--cwd", workDir, "--message", "please check"})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if got := req.WorkDir; got != workDir {
		t.Fatalf("WorkDir = %q, want %q", got, workDir)
	}

	req, _, err = parseSendArgs([]string{"--work-dir", workDir, "please check"})
	if err != nil {
		t.Fatalf("parseSendArgs returned error for --work-dir: %v", err)
	}
	if got := req.WorkDir; got != workDir {
		t.Fatalf("WorkDir from --work-dir = %q, want %q", got, workDir)
	}
}

// TestParseSendArgs_AudioPopulatesAudios is the regression for
// t-20260615-cqjbk1: --audio and --video must NOT land in req.Files
// (which would route them through SendFile and skip
// AudioSender / VideoSender). They each get their own slice.
func TestParseSendArgs_AudioPopulatesAudios(t *testing.T) {
	setTestSendSession(t)
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "voice.opus")
	if err := os.WriteFile(audioPath, []byte("opus"), 0o644); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	req, _, err := parseSendArgs([]string{"--audio", audioPath})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if len(req.Files) != 0 {
		t.Fatalf("req.Files len = %d, want 0 (audio must NOT be appended into Files)", len(req.Files))
	}
	if len(req.Audios) != 1 {
		t.Fatalf("req.Audios len = %d, want 1", len(req.Audios))
	}
	if req.Audios[0].FileName != "voice.opus" {
		t.Fatalf("audio filename = %q, want voice.opus", req.Audios[0].FileName)
	}
	if string(req.Audios[0].Data) != "opus" {
		t.Fatalf("audio data = %q, want %q", req.Audios[0].Data, "opus")
	}
}

func TestParseSendArgs_VideoPopulatesVideos(t *testing.T) {
	setTestSendSession(t)
	dir := t.TempDir()
	videoPath := filepath.Join(dir, "demo.mp4")
	if err := os.WriteFile(videoPath, []byte("mp4"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}

	req, _, err := parseSendArgs([]string{"--video", videoPath})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if len(req.Files) != 0 {
		t.Fatalf("req.Files len = %d, want 0 (video must NOT be appended into Files)", len(req.Files))
	}
	if len(req.Videos) != 1 {
		t.Fatalf("req.Videos len = %d, want 1", len(req.Videos))
	}
	if req.Videos[0].FileName != "demo.mp4" {
		t.Fatalf("video filename = %q, want demo.mp4", req.Videos[0].FileName)
	}
}

func TestParseSendArgs_AudioVideoFileMixed_StaySeparate(t *testing.T) {
	setTestSendSession(t)
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "voice.opus")
	videoPath := filepath.Join(dir, "demo.mp4")
	docPath := filepath.Join(dir, "notes.md")
	for _, f := range [][2]string{
		{audioPath, "opus"},
		{videoPath, "mp4"},
		{docPath, "hello"},
	} {
		if err := os.WriteFile(f[0], []byte(f[1]), 0o644); err != nil {
			t.Fatalf("write %s: %v", f[0], err)
		}
	}

	req, _, err := parseSendArgs([]string{
		"--audio", audioPath,
		"--video", videoPath,
		"--file", docPath,
	})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if len(req.Audios) != 1 || req.Audios[0].FileName != "voice.opus" {
		t.Fatalf("req.Audios = %#v", req.Audios)
	}
	if len(req.Videos) != 1 || req.Videos[0].FileName != "demo.mp4" {
		t.Fatalf("req.Videos = %#v", req.Videos)
	}
	if len(req.Files) != 1 || req.Files[0].FileName != "notes.md" {
		t.Fatalf("req.Files = %#v (only the --file should land here)", req.Files)
	}
}

func TestParseSendArgs_TTSOnly(t *testing.T) {
	t.Setenv("CC_PROJECT", "demo")
	t.Setenv("CC_SESSION_KEY", "telegram:123:456")

	req, _, err := parseSendArgs([]string{"--tts", "hello voice"})
	if err != nil {
		t.Fatalf("parseSendArgs returned error: %v", err)
	}
	if req.Project != "demo" {
		t.Fatalf("project = %q, want demo", req.Project)
	}
	if req.SessionKey != "telegram:123:456" {
		t.Fatalf("session = %q, want telegram:123:456", req.SessionKey)
	}
	if req.TTSText != "hello voice" {
		t.Fatalf("tts text = %q", req.TTSText)
	}
	if req.Message != "" {
		t.Fatalf("message = %q, want empty", req.Message)
	}
}

func TestParseSendArgs_AudioRejectsNonAudio(t *testing.T) {
	setTestSendSession(t)
	dir := t.TempDir()
	docPath := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(docPath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write doc: %v", err)
	}

	_, _, err := parseSendArgs([]string{"--audio", docPath})
	if err == nil {
		t.Fatal("expected non-audio attachment to be rejected")
	}
	if !strings.Contains(err.Error(), "not audio media") {
		t.Fatalf("error = %q, want not audio media", err.Error())
	}
}

func TestDetectAttachmentMimeType_UsesExtensionFallback(t *testing.T) {
	mimeType := detectAttachmentMimeType("note.md", []byte("plain"))
	if mimeType != "text/markdown; charset=utf-8" && mimeType != "text/markdown" {
		t.Fatalf("mimeType = %q, want markdown mime", mimeType)
	}
}

func TestReadAttachment_SizeLimit(t *testing.T) {
	const limit int64 = 100 // tiny limit keeps the test fast and deterministic
	dir := t.TempDir()
	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readAttachment(small, limit); err != nil {
		t.Fatalf("small file should succeed: %v", err)
	}

	big := filepath.Join(dir, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(limit + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if _, _, _, err := readAttachment(big, limit); err == nil {
		t.Fatal("oversized file should be rejected")
	}
}

func TestReadAttachment_CleanPath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "test.txt")
	if err := os.WriteFile(f, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Path with ../ should still work after cleaning
	dirty := filepath.Join(sub, "..", "sub", "test.txt")
	data, name, _, err := readAttachment(dirty, core.DefaultMaxAttachmentSize)
	if err != nil {
		t.Fatalf("readAttachment with dirty path: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("unexpected data: %q", data)
	}
	if name != "test.txt" {
		t.Errorf("unexpected filename: %q", name)
	}
}

func TestResolveMaxAttachmentSize(t *testing.T) {
	// Default when neither env nor config sets it.
	t.Setenv("CC_MAX_ATTACHMENT_SIZE_MB", "")
	if got := resolveMaxAttachmentSize(nil); got != core.DefaultMaxAttachmentSize {
		t.Fatalf("default = %d, want %d", got, core.DefaultMaxAttachmentSize)
	}

	cfg := &config.Config{MaxAttachmentSizeMB: 100}

	// Config value (MiB → bytes) honoured when env is unset.
	if got, want := resolveMaxAttachmentSize(cfg), int64(100)<<20; got != want {
		t.Fatalf("from config = %d, want %d", got, want)
	}

	// Env var is MiB (same unit as the config field) and takes precedence.
	t.Setenv("CC_MAX_ATTACHMENT_SIZE_MB", "1024") // 1024 MiB = 1 GiB
	if got, want := resolveMaxAttachmentSize(cfg), int64(1<<30); got != want {
		t.Fatalf("env override = %d, want %d", got, want)
	}

	// Malformed env falls through to config rather than being fatal.
	t.Setenv("CC_MAX_ATTACHMENT_SIZE_MB", "not-a-number")
	if got, want := resolveMaxAttachmentSize(cfg), int64(100)<<20; got != want {
		t.Fatalf("malformed env should fall through to config = %d, want %d", got, want)
	}

	// Non-positive env also falls through.
	t.Setenv("CC_MAX_ATTACHMENT_SIZE_MB", "0")
	if got, want := resolveMaxAttachmentSize(cfg), int64(100)<<20; got != want {
		t.Fatalf("zero env should fall through to config = %d, want %d", got, want)
	}
}

func TestBuildSendPayload_JSONRoundTrip(t *testing.T) {
	req := core.SendRequest{
		Project:    "demo",
		SessionKey: "telegram:1:2",
		Message:    "done",
		TTSText:    "voice done",
		Images: []core.ImageAttachment{{
			MimeType: "image/png",
			Data:     []byte("img"),
			FileName: "a.png",
		}},
		Files: []core.FileAttachment{{
			MimeType: "text/plain",
			Data:     []byte("doc"),
			FileName: "a.txt",
		}},
	}

	body, err := buildSendPayload(req)
	if err != nil {
		t.Fatalf("buildSendPayload returned error: %v", err)
	}

	var decoded core.SendRequest
	if err := decodeSendPayload(body, &decoded); err != nil {
		t.Fatalf("decodeSendPayload returned error: %v", err)
	}
	if len(decoded.Images) != 1 || string(decoded.Images[0].Data) != "img" {
		t.Fatalf("decoded images = %#v", decoded.Images)
	}
	if decoded.TTSText != "voice done" {
		t.Fatalf("decoded tts_text = %q", decoded.TTSText)
	}
	if len(decoded.Files) != 1 || string(decoded.Files[0].Data) != "doc" {
		t.Fatalf("decoded files = %#v", decoded.Files)
	}
}
