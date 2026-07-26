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

func TestDesktopTaskCompleteMarksAssistantTurnCompleted(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-completed-assistant"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"App request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"App response"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "App request"},
		{SessionID: threadID, Role: "assistant", Content: "App response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestDesktopTaskCompleteWithoutAssistantEmitsTurnCompletedMarker(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-completed-empty"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"App request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "App request"},
		{SessionID: threadID, TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestDesktopTaskCompleteWithCanceledOrErroredTurnEmitsTurnCompletedMarker(t *testing.T) {
	tests := []struct {
		name         string
		taskComplete string
	}{
		{
			name:         "canceled",
			taskComplete: `{"type":"event_msg","payload":{"type":"task_complete","error":{"message":"Canceled by user.","codex_error_info":null}}}`,
		},
		{
			name:         "errored",
			taskComplete: `{"type":"event_msg","payload":{"type":"task_complete","error":{"message":"stream disconnected","codex_error_info":null}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, transcript, threadID := newCompatibilityPollFixture(t)
			appendTranscript(t, transcript,
				`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-completed-error"}}`,
				`{"type":"event_msg","payload":{"type":"user_message","message":"App request"}}`,
				tt.taskComplete,
			)

			events, err := a.PollExternalConversation(context.Background(), threadID)
			if err != nil {
				t.Fatal(err)
			}
			want := []core.ExternalConversationEvent{
				{SessionID: threadID, Role: "user", Content: "App request"},
				{SessionID: threadID, TurnCompleted: true},
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events = %#v, want %#v", events, want)
			}
		})
	}
}

func TestDesktopTurnCompletedWaitsForTaskCompleteAcrossPolls(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-completed-split"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"App request"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "App request"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events before task_complete = %#v, want %#v", events, want)
	}

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"App response"}}`,
	)
	events, err = a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want = []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "assistant", Content: "App response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events after task_complete = %#v, want %#v", events, want)
	}
}

func TestDesktopTaskCompleteDoesNotLeakTurnCompletedForInternalTurn(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	a.desktopOrigins.register(threadID, "turn-completed-internal")
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-completed-internal"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"internal request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("internal events = %#v, want none", events)
	}
}

func TestDesktopDeferredTurnCompletedMatchesResolvedExternalTurn(t *testing.T) {
	tests := []struct {
		name         string
		taskComplete string
		want         []core.ExternalConversationEvent
	}{
		{
			name:         "assistant response",
			taskComplete: `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"App response"}}`,
			want: []core.ExternalConversationEvent{
				{Role: "user", Content: "App request"},
				{Role: "assistant", Content: "App response", TurnCompleted: true},
			},
		},
		{
			name:         "empty assistant",
			taskComplete: `{"type":"event_msg","payload":{"type":"task_complete"}}`,
			want: []core.ExternalConversationEvent{
				{Role: "user", Content: "App request"},
				{TurnCompleted: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, transcript, threadID := newCompatibilityPollFixture(t)
			ticket := a.desktopOrigins.begin(threadID)
			appendTranscript(t, transcript,
				`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-completed-deferred"}}`,
				`{"type":"event_msg","payload":{"type":"user_message","message":"App request"}}`,
				tt.taskComplete,
			)
			if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
				t.Fatalf("pending events = %#v, %v; want deferred", events, err)
			}

			a.desktopOrigins.cancel(ticket)
			events, err := a.PollExternalConversation(context.Background(), threadID)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]core.ExternalConversationEvent(nil), tt.want...)
			for i := range want {
				want[i].SessionID = threadID
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("resolved events = %#v, want %#v", events, want)
			}
		})
	}
}

func TestDesktopTaskCompleteUsesTurnIDForInternalThenExternalInterleaving(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	a.desktopOrigins.register(threadID, "turn-internal-a")
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-internal-a"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"internal request"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-external-b"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"external request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-internal-a","last_agent_message":"internal response"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-external-b","last_agent_message":"external response"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "external request"},
		{SessionID: threadID, Role: "assistant", Content: "external response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestDesktopTaskCompleteUsesTurnIDForExternalThenInternalInterleaving(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	a.desktopOrigins.register(threadID, "turn-internal-b")
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-external-a"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"external request"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-internal-b"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"internal request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-internal-b","last_agent_message":"internal response"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-external-a","last_agent_message":"external response"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "external request"},
		{SessionID: threadID, Role: "assistant", Content: "external response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestDesktopDeferredAndDirectTurnsCompleteOnceByTurnID(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	ticket := a.desktopOrigins.begin(threadID)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-deferred-a"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"deferred request"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("pending events = %#v, %v; want deferred", events, err)
	}

	a.desktopOrigins.cancel(ticket)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-direct-b"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"direct request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-direct-b","last_agent_message":"direct response"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-deferred-a","last_agent_message":"deferred response"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "direct request"},
		{SessionID: threadID, Role: "assistant", Content: "direct response", TurnCompleted: true},
		{SessionID: threadID, Role: "user", Content: "deferred request"},
		{SessionID: threadID, Role: "assistant", Content: "deferred response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-direct-b","last_agent_message":"duplicate direct response"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-deferred-a","last_agent_message":"duplicate deferred response"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("events after completed turns = %#v, %v; want none", events, err)
	}
}

func TestDesktopTaskCompleteIgnoresUnknownAndNoUserTurnIDs(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-external-a"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"external request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-unknown","last_agent_message":"unknown response"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-no-user-b"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-no-user-b","last_agent_message":"no-user response"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-external-a","last_agent_message":"external response"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-external-a","last_agent_message":"duplicate response"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "external request"},
		{SessionID: threadID, Role: "assistant", Content: "external response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-legacy-c"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"legacy request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"legacy response"}}`,
	)
	events, err = a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want = []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "legacy request"},
		{SessionID: threadID, Role: "assistant", Content: "legacy response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("legacy events after completed turns = %#v, want %#v", events, want)
	}
}

func TestDesktopTaskCompleteWithoutTurnIDFailsSafeWhenAmbiguous(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-external-a"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"request a"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-external-b"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"request b"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"ambiguous response"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-external-a","last_agent_message":"response a"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-external-b","last_agent_message":"response b"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "request a"},
		{SessionID: threadID, Role: "user", Content: "request b"},
		{SessionID: threadID, Role: "assistant", Content: "response a", TurnCompleted: true},
		{SessionID: threadID, Role: "assistant", Content: "response b", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestDesktopUserMessageFallsBackToLatestInFlightTurn(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-a"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-b"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-b"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"request a"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-a","last_agent_message":"response a"}}`,
	)

	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "request a"},
		{SessionID: threadID, Role: "assistant", Content: "response a", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestDesktopInFlightTurnsPruneOldestAtCapacity(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	lines := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-%03d"}}`,
			i,
		))
	}
	appendTranscript(t, transcript, lines...)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("start-only events = %#v, %v; want none", events, err)
	}

	a.desktopPollMu.Lock()
	state := a.desktopPolls[threadID]
	turnCount := len(state.turns)
	_, oldestRemains := state.turns["turn-000"]
	_, newestRemains := state.turns["turn-299"]
	a.desktopPollMu.Unlock()
	if turnCount != 256 || oldestRemains || !newestRemains {
		t.Fatalf("in-flight state count=%d oldest=%v newest=%v; want 256, false, true", turnCount, oldestRemains, newestRemains)
	}

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"user_message","message":"latest request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-299","last_agent_message":"latest response"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "latest request"},
		{SessionID: threadID, Role: "assistant", Content: "latest response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("latest events = %#v, want %#v", events, want)
	}
}

func TestDesktopDeferredTurnsPruneOldestAtCapacityWithoutDuplicates(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	ticket := a.desktopOrigins.begin(threadID)
	lines := make([]string, 0, 260*3)
	for i := 0; i < 260; i++ {
		turnID := fmt.Sprintf("turn-deferred-%03d", i)
		lines = append(lines,
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"%s"}}`, turnID),
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"user_message","message":"request %03d"}}`, i),
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"%s","last_agent_message":"response %03d"}}`, turnID, i),
		)
	}
	appendTranscript(t, transcript, lines...)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("deferred events = %#v, %v; want pending", events, err)
	}

	a.desktopPollMu.Lock()
	state := a.desktopPolls[threadID]
	deferredCount := len(state.deferred)
	firstTurnID := state.deferred[0].turnID
	lastTurnID := state.deferred[len(state.deferred)-1].turnID
	a.desktopPollMu.Unlock()
	if deferredCount != 256 || firstTurnID != "turn-deferred-004" || lastTurnID != "turn-deferred-259" {
		t.Fatalf("deferred state count=%d first=%q last=%q", deferredCount, firstTurnID, lastTurnID)
	}

	a.desktopOrigins.cancel(ticket)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 512 {
		t.Fatalf("resolved events = %d, want 512", len(events))
	}
	for i := 0; i < 256; i++ {
		user, assistant := events[i*2], events[i*2+1]
		wantIndex := i + 4
		if user.Content != fmt.Sprintf("request %03d", wantIndex) ||
			assistant.Content != fmt.Sprintf("response %03d", wantIndex) ||
			user.TurnCompleted || !assistant.TurnCompleted {
			t.Fatalf("resolved pair %d = %#v, %#v", i, user, assistant)
		}
	}
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("events after deferred resolution = %#v, %v; want none", events, err)
	}
}

func TestDesktopPollPrunesExpiredKeyedTurnBeforeLegacyCompletion(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-stale"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("stale start events = %#v, %v; want none", events, err)
	}

	a.desktopPollMu.Lock()
	a.desktopPolls[threadID].turns["turn-stale"].startedAt = time.Now().Add(-25 * time.Hour)
	a.desktopPollMu.Unlock()
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("prune poll events = %#v, %v; want none", events, err)
	}

	a.desktopPollMu.Lock()
	_, staleRemains := a.desktopPolls[threadID].turns["turn-stale"]
	a.desktopPollMu.Unlock()
	if staleRemains {
		t.Fatal("expired keyed turn remained after poll")
	}

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-live"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"live request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"live response"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "live request"},
		{SessionID: threadID, Role: "assistant", Content: "live response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestDesktopPollPrunesExpiredLegacyTurnBeforeUniqueFallback(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"user_message","client_id":"legacy-client","message":"stale legacy request"}}`,
	)
	events, err := a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "stale legacy request"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("legacy events = %#v, want %#v", events, want)
	}

	a.desktopPollMu.Lock()
	a.desktopPolls[threadID].legacyTurn.startedAt = time.Now().Add(-25 * time.Hour)
	a.desktopPollMu.Unlock()
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("prune poll events = %#v, %v; want none", events, err)
	}

	a.desktopPollMu.Lock()
	legacyRemains := a.desktopPolls[threadID].legacyTurn != nil
	a.desktopPollMu.Unlock()
	if legacyRemains {
		t.Fatal("expired legacy turn remained after poll")
	}

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-live"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"live request"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"live response"}}`,
	)
	events, err = a.PollExternalConversation(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	want = []core.ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "live request"},
		{SessionID: threadID, Role: "assistant", Content: "live response", TurnCompleted: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

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
		{SessionID: threadID, Role: "assistant", Content: "App 回答", TurnCompleted: true},
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
		{SessionID: threadID, Role: "assistant", Content: "active answer", TurnCompleted: true},
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
		{SessionID: threadID, Role: "assistant", Content: "App 回答", TurnCompleted: true},
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
		{SessionID: threadID, Role: "assistant", Content: "App 回答", TurnCompleted: true},
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
