package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestPollExternalConversationParsesDesktopImagesAndFilesWithoutForwardingPaths(t *testing.T) {
	codexHome, transcript, threadID := newDesktopMediaTranscript(t)
	imagePath := writeDesktopMediaFile(t, "picture.png", []byte("\x89PNG\r\n\x1a\nimage"))
	filePath := writeDesktopMediaFile(t, "report.txt", []byte("report"))

	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	initializeDesktopMediaPoll(t, a, threadID)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-media"}}`,
		desktopUserMessageJSON(t, "请处理附件", []string{imagePath}, []desktopTestFile{
			{name: "picture.png", path: imagePath},
			{name: "report.txt", path: filePath},
		}),
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"处理完成"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2", len(events))
	}
	user := events[0]
	if user.Content != "请处理附件" {
		t.Fatalf("content = %q, want safe request body", user.Content)
	}
	if strings.Contains(user.Content, imagePath) || strings.Contains(user.Content, filePath) ||
		strings.Contains(user.Content, "Files mentioned by the user") {
		t.Fatal("forwarded content leaked a local path or machine file list")
	}
	if got, want := user.Images, []core.ImageAttachment{{
		MimeType: "image/png", Data: []byte("\x89PNG\r\n\x1a\nimage"), FileName: "picture.png",
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("images = %#v, want %#v", got, want)
	}
	if got, want := user.Files, []core.FileAttachment{{
		MimeType: "text/plain; charset=utf-8", Data: []byte("report"), FileName: "report.txt",
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %#v, want %#v", got, want)
	}
}

func TestDesktopLocalImagesRejectUnsafePaths(t *testing.T) {
	regular := writeDesktopMediaFile(t, "not-image.txt", []byte("plain text"))
	directory := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing.png")
	symlink := filepath.Join(t.TempDir(), "link.png")
	image := writeDesktopMediaFile(t, "target.png", []byte("\x89PNG\r\n\x1a\nimage"))
	if err := os.Symlink(image, symlink); err != nil {
		t.Fatal(err)
	}
	oversize := filepath.Join(t.TempDir(), "oversize.png")
	f, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(desktopAttachmentMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	event := desktopUserEvent("thread-safe-images", "safe body", []string{
		"relative.png", regular, directory, missing, symlink, oversize,
	})
	if len(event.Images) != 0 {
		t.Fatalf("unsafe local images = %#v, want rejected", event.Images)
	}
	if event.Content != "safe body" {
		t.Fatalf("content = %q, want safe body preserved", event.Content)
	}
}

func TestPollExternalConversationAllowsDesktopMediaOnly(t *testing.T) {
	codexHome, transcript, threadID := newDesktopMediaTranscript(t)
	imagePath := writeDesktopMediaFile(t, "only.png", []byte("\x89PNG\r\n\x1a\nimage"))
	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	initializeDesktopMediaPoll(t, a, threadID)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-media-only"}}`,
		desktopUserMessageJSON(t, "", []string{imagePath}, nil),
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Content != "" || len(events[0].Images) != 1 {
		t.Fatalf("media-only events = %#v", events)
	}
}

func TestPollExternalConversationPreservesDeferredDesktopAttachments(t *testing.T) {
	codexHome, transcript, threadID := newDesktopMediaTranscript(t)
	imagePath := writeDesktopMediaFile(t, "deferred.png", []byte("\x89PNG\r\n\x1a\nimage"))
	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	initializeDesktopMediaPoll(t, a, threadID)
	ticket := a.desktopOrigins.begin(threadID)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-deferred-media"}}`,
		desktopUserMessageJSON(t, "稍后发送", []string{imagePath}, nil),
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"完成"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("pending events = %#v, %v; want deferred", events, err)
	}

	a.desktopOrigins.cancel(ticket)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || len(events[0].Images) != 1 || events[0].Content != "稍后发送" {
		t.Fatalf("resolved deferred events = %#v", events)
	}
}

func TestPollExternalConversationDoesNotReadInternalOriginAttachments(t *testing.T) {
	codexHome, transcript, threadID := newDesktopMediaTranscript(t)
	imagePath := writeDesktopMediaFile(t, "internal.png", []byte("\x89PNG\r\n\x1a\nimage"))
	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	initializeDesktopMediaPoll(t, a, threadID)
	a.desktopOrigins.register(threadID, "turn-internal-media")
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-internal-media"}}`,
		desktopUserMessageJSON(t, "内部消息", []string{imagePath}, nil),
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"内部回答"}}`,
	)
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("internal-origin events = %#v, want fail-closed", events)
	}
}

func TestDesktopFileListRejectsUnsafeOrNonMachinePaths(t *testing.T) {
	regular := writeDesktopMediaFile(t, "valid.txt", []byte("valid"))
	directory := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing.txt")
	symlink := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	oversize := filepath.Join(t.TempDir(), "oversize.bin")
	f, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(desktopAttachmentMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	unreadable := writeDesktopMediaFile(t, "unreadable.txt", []byte("private"))
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })

	tests := []struct {
		name    string
		message string
	}{
		{name: "fake prose", message: "# Files mentioned by the user:\n\nnot a machine entry: " + regular + "\n\nkeep this"},
		{name: "relative", message: desktopFileMessage("keep relative", []desktopTestFile{{name: "valid.txt", path: "relative.txt"}})},
		{name: "basename mismatch", message: desktopFileMessage("keep mismatch", []desktopTestFile{{name: "other.txt", path: regular}})},
		{name: "missing", message: desktopFileMessage("keep missing", []desktopTestFile{{name: "missing.txt", path: missing}})},
		{name: "directory", message: desktopFileMessage("keep directory", []desktopTestFile{{name: filepath.Base(directory), path: directory}})},
		{name: "symlink", message: desktopFileMessage("keep symlink", []desktopTestFile{{name: "link.txt", path: symlink}})},
		{name: "oversize", message: desktopFileMessage("keep oversize", []desktopTestFile{{name: "oversize.bin", path: oversize}})},
		{name: "unreadable", message: desktopFileMessage("keep unreadable", []desktopTestFile{{name: "unreadable.txt", path: unreadable}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, files := parseDesktopFileMentions(tt.message, nil)
			if len(files) != 0 {
				t.Fatalf("files = %#v, want rejected", files)
			}
			if strings.TrimSpace(content) == "" {
				t.Fatal("unsafe input discarded all safe body text")
			}
		})
	}
}

type desktopTestFile struct {
	name string
	path string
}

func newDesktopMediaTranscript(t *testing.T) (string, string, string) {
	t.Helper()
	codexHome := t.TempDir()
	dir := filepath.Join(codexHome, "sessions", "2026", "07", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "thread-desktop-media"
	transcript := filepath.Join(dir, "rollout-"+threadID+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return codexHome, transcript, threadID
}

func initializeDesktopMediaPoll(t *testing.T, a *Agent, threadID string) {
	t.Helper()
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("initial poll = %#v, %v", events, err)
	}
}

func writeDesktopMediaFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func desktopUserMessageJSON(t *testing.T, request string, images []string, files []desktopTestFile) string {
	t.Helper()
	payload := map[string]any{
		"type":         "user_message",
		"message":      desktopFileMessage(request, files),
		"local_images": images,
	}
	raw, err := jsonMarshalDesktopTestEvent(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func desktopFileMessage(request string, files []desktopTestFile) string {
	if len(files) == 0 {
		return request
	}
	var b strings.Builder
	b.WriteString("\n# Files mentioned by the user:\n")
	for _, file := range files {
		b.WriteString("\n## ")
		b.WriteString(file.name)
		b.WriteString(": ")
		b.WriteString(file.path)
		b.WriteByte('\n')
	}
	b.WriteString("\n## My request for Codex:\n")
	b.WriteString(request)
	b.WriteByte('\n')
	return b.String()
}

func jsonMarshalDesktopTestEvent(payload map[string]any) (string, error) {
	raw, err := json.Marshal(map[string]any{"type": "event_msg", "payload": payload})
	return string(raw), err
}
