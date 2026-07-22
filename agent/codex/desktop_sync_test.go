package codex

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestPollExternalConversationOnlyReturnsCodexAppTurns(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "07", "21")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "thread-sync-1"
	transcript := filepath.Join(sessionsDir, "rollout-2026-07-21T00-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"event_msg","payload":{"type":"user_message","client_id":"existing","message":"历史消息"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("initial poll = %#v, %v; want tail-only", events, err)
	}
	a.desktopOrigins.register(threadID, "turn-feishu")

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-app"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"从 App 发送"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"App 回答"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-feishu"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","client_id":"future-api-client","message":"从飞书发送"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"飞书回答"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "从 App 发送"},
		{SessionID: threadID, Role: "assistant", Content: "App 回答"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestPollExternalConversationDropsMessagesWrittenWhileRouteInactive(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "07", "21")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "thread-sync-route-reset"
	transcript := filepath.Join(sessionsDir, "rollout-2026-07-21T00-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"event_msg","payload":{"type":"user_message","client_id":"history","message":"历史"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	tracker, ok := any(a).(interface{ SetExternalConversationRoutes([]string) })
	if !ok {
		t.Fatal("Codex agent does not support external conversation route tracking")
	}
	tracker.SetExternalConversationRoutes([]string{threadID})
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("initial poll = %#v, %v; want tail-only", events, err)
	}

	tracker.SetExternalConversationRoutes(nil)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"user_message","client_id":"desktop-client","message":"inactive message"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"inactive answer"}}`,
	)
	tracker.SetExternalConversationRoutes([]string{threadID})
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("reactivation poll = %#v, %v; want inactive backlog discarded", events, err)
	}

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"user_message","client_id":"desktop-client","message":"active message"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"active answer"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "active message"},
		{SessionID: threadID, Role: "assistant", Content: "active answer"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("active events = %#v, want %#v", events, want)
	}
}

func TestPollExternalConversationDefersAmbiguousAppTurnUntilInternalTurnIDArrives(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "07", "21")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "thread-race"
	transcript := filepath.Join(sessionsDir, "rollout-2026-07-21T00-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("initial poll = %#v, %v; want tail-only", events, err)
	}
	ticket := a.desktopOrigins.begin(threadID)

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-app"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"相同内容"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"App 回答"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("poll while turn/start pending = %#v, %v; want deferred", events, err)
	}

	a.desktopOrigins.complete(ticket, "turn-feishu")
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "相同内容"},
		{SessionID: threadID, Role: "assistant", Content: "App 回答"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("resolved app events = %#v, want %#v", events, want)
	}

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-feishu"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"相同内容"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"飞书回答"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("internal turn events = %#v, %v; want filtered", events, err)
	}
}

func TestPollExternalConversationReleasesDeferredAppTurnAfterInternalSendFails(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "07", "21")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "thread-cancel"
	transcript := filepath.Join(sessionsDir, "rollout-2026-07-21T00-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{codexHome: codexHome, desktopLiveSync: true}
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("initial poll = %#v, %v; want tail-only", events, err)
	}
	ticket := a.desktopOrigins.begin(threadID)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-app"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"App 消息"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"App 回答"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("poll while turn/start pending = %#v, %v; want deferred", events, err)
	}

	a.desktopOrigins.cancel(ticket)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "App 消息"},
		{SessionID: threadID, Role: "assistant", Content: "App 回答"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events after failed internal send = %#v, want %#v", events, want)
	}
}

func TestDesktopOriginTrackerIsBoundedAndRouteResetClearsUnpolledTurns(t *testing.T) {
	a := &Agent{desktopLiveSync: true, desktopRoutes: map[string]struct{}{"thread-bounded": {}}}
	for i := 0; i < 400; i++ {
		a.desktopOrigins.register("thread-bounded", fmt.Sprintf("turn-%d", i))
	}
	a.desktopOrigins.mu.Lock()
	stored := len(a.desktopOrigins.sessions["thread-bounded"].turns)
	a.desktopOrigins.mu.Unlock()
	if stored > 256 {
		t.Fatalf("stored origin turns = %d, want bounded at 256", stored)
	}

	a.SetExternalConversationRoutes(nil)
	if internal, pending := a.desktopOrigins.classify("thread-bounded", "turn-399"); internal || pending {
		t.Fatalf("route reset classify = internal %v, pending %v; want cleared", internal, pending)
	}
}

func TestDesktopOriginTrackerExpiresStaleState(t *testing.T) {
	tracker := &desktopOriginTracker{}
	ticket := tracker.begin("thread-expired")
	tracker.complete(ticket, "turn-expired")

	tracker.mu.Lock()
	tracker.sessions["thread-expired"].turns["turn-expired"] = time.Now().Add(-desktopOriginTTL - time.Second)
	tracker.mu.Unlock()

	if internal, pending := tracker.classify("thread-expired", "turn-expired"); internal || pending {
		t.Fatalf("expired classify = internal %v, pending %v; want cleared", internal, pending)
	}
}

func TestDesktopOriginTrackerFailsClosedAfterUnreconciledTicketExpires(t *testing.T) {
	tracker := &desktopOriginTracker{}
	ticket := tracker.begin("thread-unreconciled")

	tracker.mu.Lock()
	tracker.sessions["thread-unreconciled"].pending[ticket.id] = time.Now().Add(-desktopOriginTTL - time.Second)
	tracker.mu.Unlock()

	if internal, pending := tracker.classify("thread-unreconciled", "turn-after-expiry"); !internal || pending {
		t.Fatalf("expired unreconciled classify = internal %v, pending %v; want fail-closed internal", internal, pending)
	}
}

func appendTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPollExternalConversationMarksSupportedSequenceHealthy(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-app"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"App marker"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"App answer"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil || len(events) != 2 {
		t.Fatalf("poll = %#v, %v", events, err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")
}

func TestPollExternalConversationKeepsHealthyWhenRouteAddedAfterSupport(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "07", "21")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	routeA := "thread-compatibility-route-a"
	routeB := "thread-compatibility-route-b"
	transcriptA := filepath.Join(sessionsDir, "rollout-2026-07-21T00-00-00-"+routeA+".jsonl")
	if err := os.WriteFile(transcriptA, []byte(`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		codexHome:       codexHome,
		desktopLiveSync: true,
		compatibility:   newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now }),
	}
	a.SetExternalConversationRoutes([]string{routeA})
	if events, err := a.PollExternalConversation(context.Background(), routeA); err != nil || len(events) != 0 {
		t.Fatalf("initial route A poll = %#v, %v; want tail-only", events, err)
	}
	appendTranscript(t, transcriptA,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-route-a"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"route A marker"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"route A answer"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), routeA); err != nil || len(events) != 2 {
		t.Fatalf("supported route A poll = %#v, %v", events, err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")

	a.SetExternalConversationRoutes([]string{routeA, routeB})
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")

	now = now.Add(compatibilityTranscriptGrace + time.Second)
	if _, err := a.PollExternalConversation(context.Background(), routeB); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")
}

func TestPollExternalConversationAggregatesCompatibilityAcrossRoutes(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "07", "21")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	routeA := "thread-compatibility-route-a"
	routeB := "thread-compatibility-route-b"
	transcriptA := filepath.Join(sessionsDir, "rollout-2026-07-21T00-00-00-"+routeA+".jsonl")
	if err := os.WriteFile(transcriptA, []byte(`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		codexHome:       codexHome,
		desktopLiveSync: true,
		compatibility:   newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now }),
	}
	a.SetExternalConversationRoutes([]string{routeA, routeB})
	if events, err := a.PollExternalConversation(context.Background(), routeA); err != nil || len(events) != 0 {
		t.Fatalf("initial route A poll = %#v, %v; want tail-only", events, err)
	}

	appendTranscript(t, transcriptA,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-route-a"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"route A marker"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"route A answer"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), routeA); err != nil || len(events) != 2 {
		t.Fatalf("supported route A poll = %#v, %v", events, err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")

	now = now.Add(compatibilityTranscriptGrace + time.Second)
	if _, err := a.PollExternalConversation(context.Background(), routeB); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")

	if err := os.Remove(transcriptA); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PollExternalConversation(context.Background(), routeA); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityDegraded, "transcript_missing")
}

func TestPollExternalConversationFailsClosedWithoutTurnIdentity(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"must-not-leak"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"must-not-forward"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("unsafe events forwarded: %#v", events)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityIncompatible, "unsafe_turn_identity")
}

func TestPollExternalConversationClearsTurnIdentityAfterTranscriptTruncation(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-before-truncation"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("poll before truncation = %#v, %v", events, err)
	}

	if err := os.Truncate(transcript, 0); err != nil {
		t.Fatal(err)
	}
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("truncation poll = %#v, %v", events, err)
	}
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"user_message","message":"must-not-leak-after-truncation"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"must-not-forward-after-truncation"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("unsafe events forwarded after truncation: %#v", events)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityIncompatible, "unsafe_turn_identity")
}

func TestPollExternalConversationAcceptsClientIDAsLegacySourceMarker(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"user_message","client_id":"desktop-client","message":"legacy marker"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"legacy answer"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil || len(events) != 2 {
		t.Fatalf("legacy marker poll = %#v, %v", events, err)
	}
}

func TestPollExternalConversationReportsInvalidJSONAndRecovers(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript, `{not-json`)
	if _, err := a.PollExternalConversation(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityDegraded, "transcript_json_invalid")

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-recovery"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"recovery marker"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"recovered"}}`,
	)
	if _, err := a.PollExternalConversation(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")
}

func TestPollExternalConversationReportsOpenFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce Unix mode bits")
	}
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript, `{"type":"event_msg","payload":{"type":"task_complete"}}`)
	if err := os.Chmod(transcript, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(transcript, 0o644) })
	if _, err := a.PollExternalConversation(context.Background(), threadID); err == nil {
		t.Fatal("poll succeeded, want transcript open failure")
	}
	assertCompatibilityState(t, a.compatibility, compatibilityDegraded, "transcript_open_failed")
}

func TestPollExternalConversationReportsRotationAndRecovers(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-before-rotation"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"before rotation"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"before answer"}}`,
	)
	if _, err := a.PollExternalConversation(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")

	if err := os.Remove(transcript); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PollExternalConversation(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityDegraded, "transcript_missing")

	if err := os.WriteFile(transcript, []byte(`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.desktopPollMu.Lock()
	a.desktopPolls[threadID].lastLookup = time.Time{}
	a.desktopPollMu.Unlock()
	if _, err := a.PollExternalConversation(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-after-rotation"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"after rotation"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"after answer"}}`,
	)
	if _, err := a.PollExternalConversation(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityHealthy, "supported_turn")
}

func TestPollExternalConversationReportsMissingTranscriptAfterGrace(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	a := &Agent{
		codexHome:       t.TempDir(),
		desktopLiveSync: true,
		compatibility:   newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now }),
	}
	a.SetExternalConversationRoutes([]string{"missing-thread"})
	if _, err := a.PollExternalConversation(context.Background(), "missing-thread"); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityWaiting, "transcript_pending")
	now = now.Add(compatibilityTranscriptGrace + time.Second)
	if _, err := a.PollExternalConversation(context.Background(), "missing-thread"); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityState(t, a.compatibility, compatibilityDegraded, "transcript_missing")
}

func TestPollExternalConversationLogDoesNotContainContentOrIdentity(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	a, transcript, threadID := newCompatibilityPollFixture(t)
	logs.Reset()
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"secret-message"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	)
	if _, err := a.PollExternalConversation(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-message", threadID, transcript} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logs.String())
		}
	}
}

func newCompatibilityPollFixture(t *testing.T) (*Agent, string, string) {
	t.Helper()
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "07", "21")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "thread-compatibility-fixture"
	transcript := filepath.Join(sessionsDir, "rollout-2026-07-21T00-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		codexHome:       codexHome,
		desktopLiveSync: true,
		compatibility:   newCompatibilitySentinel(true, "codex-cli 1.0", time.Now),
	}
	a.SetExternalConversationRoutes([]string{threadID})
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("initial poll = %#v, %v", events, err)
	}
	return a, transcript, threadID
}
