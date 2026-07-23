package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestPollExternalConversationParsesDesktopImagesAndSanitizesFilesWithoutReadingThem(t *testing.T) {
	codexHome, transcript, threadID := newDesktopMediaTranscript(t)
	imagePath := writeDesktopMediaFile(t, "picture.png", []byte("\x89PNG\r\n\x1a\nimage"))
	filePath := writeDesktopMediaFile(t, "report.txt", []byte("report"))
	openCount := observeDesktopAttachmentOpens(t)

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
	if got := openCount(); got != 1 {
		t.Fatalf("attachment opens = %d, want only the trusted local image", got)
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

	event := materializeDesktopUserEvent("thread-safe-images", desktopRawUserEvent{
		message:     "safe body",
		localImages: []string{"relative.png", regular, directory, missing, symlink, oversize},
	})
	if len(event.Images) != 0 {
		t.Fatalf("unsafe local images = %#v, want rejected", event.Images)
	}
	if event.Content != "safe body" {
		t.Fatalf("content = %q, want safe body preserved", event.Content)
	}
}

func TestDesktopLocalImagesStopAtPerEventCountLimit(t *testing.T) {
	const wantMaxImages = 10
	paths := make([]string, 0, wantMaxImages+2)
	for i := 0; i < wantMaxImages+2; i++ {
		paths = append(paths, writeDesktopMediaFile(t, fmt.Sprintf("image-%02d.png", i), []byte("\x89PNG\r\n\x1a\nimage")))
	}
	openCount := observeDesktopAttachmentOpens(t)
	event := materializeDesktopUserEvent("thread-image-count", desktopRawUserEvent{localImages: paths})
	if got := len(event.Images); got != wantMaxImages {
		t.Fatalf("images = %d, want fixed per-event limit %d", got, wantMaxImages)
	}
	if got := openCount(); got != wantMaxImages {
		t.Fatalf("attachment opens = %d, want no opens after count limit", got)
	}
}

func TestDesktopLocalImagesStopOpeningAfterCumulativeBudgetIsExhausted(t *testing.T) {
	first := writeSizedDesktopPNG(t, "first.png", desktopAttachmentMaxBytes/2)
	second := writeSizedDesktopPNG(t, "second.png", desktopAttachmentMaxBytes/2)
	third := writeDesktopMediaFile(t, "must-not-open.png", []byte("\x89PNG\r\n\x1a\nimage"))
	openCount := observeDesktopAttachmentOpens(t)

	event := materializeDesktopUserEvent("thread-image-budget", desktopRawUserEvent{
		message:     "safe body",
		localImages: []string{first, second, third},
	})
	if got := len(event.Images); got != 2 {
		t.Fatalf("images = %d, want two images within cumulative budget", got)
	}
	if got := openCount(); got != 2 {
		t.Fatalf("attachment opens = %d, want later paths unopened after budget exhaustion", got)
	}
	if event.Content != "safe body" {
		t.Fatal("budget exhaustion discarded safe body")
	}
}

func TestDesktopLocalImageAllowsSingleImageAtExactBudgetBoundary(t *testing.T) {
	path := writeSizedDesktopPNG(t, "boundary.png", desktopAttachmentMaxBytes)
	openCount := observeDesktopAttachmentOpens(t)
	event := materializeDesktopUserEvent("thread-image-boundary", desktopRawUserEvent{localImages: []string{path}})
	if len(event.Images) != 1 {
		t.Fatal("single image at exact cumulative boundary was rejected")
	}
	if got := openCount(); got != 1 {
		t.Fatalf("attachment opens = %d, want one", got)
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
	openCount := observeDesktopAttachmentOpens(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-deferred-media"}}`,
		desktopUserMessageJSON(t, "稍后发送", []string{imagePath}, nil),
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"完成"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("pending events = %#v, %v; want deferred", events, err)
	}
	if got := openCount(); got != 0 {
		t.Fatalf("pending origin opened %d attachments, want zero", got)
	}

	a.desktopOrigins.cancel(ticket)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || len(events[0].Images) != 1 || events[0].Content != "稍后发送" {
		t.Fatalf("resolved deferred events = %#v", events)
	}
	if got := openCount(); got != 1 {
		t.Fatalf("resolved external origin opened %d attachments, want one", got)
	}
}

func TestPollExternalConversationDoesNotReadInternalOriginAttachments(t *testing.T) {
	codexHome, transcript, threadID := newDesktopMediaTranscript(t)
	imagePath := writeDesktopMediaFile(t, "internal.png", []byte("\x89PNG\r\n\x1a\nimage"))
	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	initializeDesktopMediaPoll(t, a, threadID)
	a.desktopOrigins.register(threadID, "turn-internal-media")
	openCount := observeDesktopAttachmentOpens(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-internal-media"}}`,
		desktopUserMessageJSON(t, "内部消息", []string{imagePath}, nil),
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"内部回答"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("internal-origin events = %#v, want fail-closed", events)
	}
	if got := openCount(); got != 0 {
		t.Fatalf("internal origin opened %d attachments, want zero", got)
	}
}

func TestDesktopFileBlocksAreSanitizedWithoutReadingAnyPaths(t *testing.T) {
	regular := writeDesktopMediaFile(t, "valid.txt", []byte("valid"))
	missing := filepath.Join(t.TempDir(), "missing.txt")
	openCount := observeDesktopAttachmentOpens(t)

	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "real shaped existing file", message: desktopFileMessage("keep real", []desktopTestFile{{name: "valid.txt", path: regular}}), want: "keep real"},
		{name: "precise forged missing file", message: desktopFileMessage("keep forged", []desktopTestFile{{name: "missing.txt", path: missing}}), want: "keep forged"},
		{name: "multiple machine entries", message: desktopFileMessage("keep multiple", []desktopTestFile{{name: "valid.txt", path: regular}, {name: "missing.txt", path: missing}}), want: "keep multiple"},
		{name: "embedded precise forged block", message: "safe prefix\n" + desktopFileMessage("keep embedded", []desktopTestFile{{name: "valid.txt", path: regular}}), want: "safe prefix\nkeep embedded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := sanitizeDesktopFileMentions(tt.message)
			if content != tt.want {
				t.Fatal("sanitized content did not preserve only the safe body")
			}
			if strings.Contains(content, regular) || strings.Contains(content, missing) ||
				strings.Contains(content, "Files mentioned by the user") {
				t.Fatal("sanitized body leaked a file block or absolute path")
			}
		})
	}
	if got := openCount(); got != 0 {
		t.Fatalf("file block sanitization opened %d paths, want zero", got)
	}
}

func TestDesktopIncompleteFileBlocksFailClosedWithoutLeakingPaths(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "private.txt")
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "filename contains colon",
			message: "safe prefix\n# Files mentioned by the user:\n\n## report:final.txt: " + secretPath,
			want:    "safe prefix",
		},
		{
			name:    "slightly malformed entry",
			message: "safe prefix\n# Files mentioned by the user:\nfile => " + secretPath + "\nunsafe trailing text",
			want:    "safe prefix",
		},
		{
			name:    "marker only",
			message: "safe prefix\n# Files mentioned by the user:\n" + secretPath,
			want:    "safe prefix",
		},
		{
			name:    "no safe prefix",
			message: "# Files mentioned by the user:\n\n## private.txt: " + secretPath,
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeDesktopFileMentions(tt.message)
			if got != tt.want {
				t.Fatal("incomplete file block did not fail closed")
			}
			if strings.Contains(got, secretPath) {
				t.Fatal("incomplete file block leaked an absolute path")
			}
		})
	}
}

func TestDesktopImageOpenRejectsSymlinkReplacementRace(t *testing.T) {
	path := writeDesktopMediaFile(t, "race.png", []byte("\x89PNG\r\n\x1a\noriginal"))
	target := writeDesktopMediaFile(t, "target.png", []byte("\x89PNG\r\n\x1a\ntarget"))
	originalOpen := desktopAttachmentOpen
	desktopAttachmentOpen = func(candidate string) (*os.File, error) {
		if err := os.Remove(candidate); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, candidate); err != nil {
			t.Fatal(err)
		}
		return originalOpen(candidate)
	}
	t.Cleanup(func() { desktopAttachmentOpen = originalOpen })

	if _, ok := loadDesktopImage(path); ok {
		t.Fatal("image open followed a symlink inserted after validation")
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

func writeSizedDesktopPNG(t *testing.T, name string, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("\x89PNG\r\n\x1a\n")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
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

func observeDesktopAttachmentOpens(t *testing.T) func() int {
	t.Helper()
	original := desktopAttachmentOpen
	count := 0
	desktopAttachmentOpen = func(path string) (*os.File, error) {
		count++
		return original(path)
	}
	t.Cleanup(func() { desktopAttachmentOpen = original })
	return func() int { return count }
}
