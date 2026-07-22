package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewOperationStore_CreateOrGetPersistsWithoutOverwritingProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	store, err := NewNewOperationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := NewOperation{
		ID:              "op-1",
		Project:         "project-1",
		Platform:        "feishu",
		SourceMessageID: "message-1",
		Step:            NewOperationAccepted,
	}
	created, isNew, err := store.CreateOrGet(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !isNew || created.ID != seed.ID {
		t.Fatalf("created = %+v, isNew = %v", created, isNew)
	}
	if _, err := store.Update(seed.ID, func(op *NewOperation) error {
		op.TargetSessionKey = "feishu:chat-b:user-1"
		op.Step = NewOperationConversationSpawned
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewNewOperationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, isNew, err := reloaded.CreateOrGet(seed)
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Fatal("reloaded operation was created again")
	}
	if got.TargetSessionKey != "feishu:chat-b:user-1" || got.Step != NewOperationConversationSpawned {
		t.Fatalf("persisted progress was overwritten: %+v", got)
	}
}

func TestNewOperationStore_RejectsStepRegression(t *testing.T) {
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{ID: "op-1", Step: NewOperationSessionBound}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("op-1", func(op *NewOperation) error {
		op.Step = NewOperationAccepted
		return nil
	}); err == nil {
		t.Fatal("step regression was accepted")
	}
	got, _ := store.Get("op-1")
	if got.Step != NewOperationSessionBound {
		t.Fatalf("step = %q, want %q", got.Step, NewOperationSessionBound)
	}
}

func TestInPlaceOperationStepTransitions(t *testing.T) {
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{ID: "in-place", Mode: NewOperationModeInPlaceSession, Step: NewOperationNameSynced}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("in-place", func(op *NewOperation) error {
		op.Step = NewOperationActivated
		return nil
	}); err != nil {
		t.Fatalf("name_synced -> activated: %v", err)
	}
	if _, err := store.Update("in-place", func(op *NewOperation) error {
		op.Step = NewOperationCompleted
		return nil
	}); err != nil {
		t.Fatalf("activated -> completed: %v", err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{ID: "external", Step: NewOperationNameSynced}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("external", func(op *NewOperation) error {
		op.Step = NewOperationCompleted
		return nil
	}); err != nil {
		t.Fatalf("external name_synced -> completed: %v", err)
	}
}

func TestNewOperationEffectiveModeDefaultsToExternalConversation(t *testing.T) {
	if got := (NewOperation{}).EffectiveMode(); got != NewOperationModeExternalConversation {
		t.Fatalf("empty mode = %q, want %q", got, NewOperationModeExternalConversation)
	}
}

func TestNewOperationStoreUpdatesLegacySnapshotWithoutChangingVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	legacy := `{"version":1,"operations":{"legacy":{"id":"legacy","step":"name_synced","status":"running"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewNewOperationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	op, ok := store.Get("legacy")
	if !ok || op.EffectiveMode() != NewOperationModeExternalConversation {
		t.Fatalf("legacy operation = %+v, exists=%v", op, ok)
	}
	if _, err := store.Update("legacy", func(op *NewOperation) error {
		op.LastError = "retry"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot newOperationSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != newOperationSnapshotVersion {
		t.Fatalf("snapshot version = %d, want %d", snapshot.Version, newOperationSnapshotVersion)
	}
}

func TestInPlaceOperationDoesNotPublishExternalTargetRecovery(t *testing.T) {
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	const sessionKey = "weixin:dm:user-1"
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               "in-place",
		Mode:             NewOperationModeInPlaceSession,
		SourceSessionKey: sessionKey,
		TargetSessionKey: sessionKey,
		AgentRecoveryKey: "cc-connect/new/in-place",
		Step:             NewOperationNameSynced,
		Status:           NewOperationRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishTargetRecoveryKey("in-place", sessionKey, "cc-connect/new/in-place"); err == nil {
		t.Fatal("publishing an in-place target succeeded")
	}
	if got := store.RecoveryKeyForTarget(sessionKey); got != "" {
		t.Fatalf("recovery key = %q, want empty", got)
	}
	if store.HasIncompleteTarget(sessionKey) {
		t.Fatal("in-place operation was indexed as an incomplete external target")
	}
	if _, ok := store.provisionalTargetKeys[sessionKey]; ok {
		t.Fatal("in-place operation was indexed as a provisional external target")
	}
}

func TestInPlaceOperationDoesNotBlockExternalRecoveryGate(t *testing.T) {
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               "in-place",
		Platform:         "weixin",
		Mode:             NewOperationModeInPlaceSession,
		SourceSessionKey: "weixin:dm:user-1",
		Step:             NewOperationAccepted,
		Status:           NewOperationRunning,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !store.WaitUntilMessageRecoverySafe(ctx, "weixin", "weixin:dm:user-2") {
		t.Fatal("in-place operation blocked an unrelated conversation recovery gate")
	}
}

func TestNewOperationStore_ReadyFiltersPlatformStatusAndTime(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	seeds := []NewOperation{
		{ID: "ready", Platform: "feishu", Step: NewOperationAccepted, Status: NewOperationRunning},
		{ID: "due", Platform: "feishu", Step: NewOperationAccepted, Status: NewOperationRetryWait, NextAttemptAt: now},
		{ID: "later", Platform: "feishu", Step: NewOperationAccepted, Status: NewOperationRetryWait, NextAttemptAt: now.Add(time.Minute)},
		{ID: "manual", Platform: "feishu", Step: NewOperationAccepted, Status: NewOperationManualAction},
		{ID: "done", Platform: "feishu", Step: NewOperationCompleted, Status: NewOperationStatusCompleted},
		{ID: "other", Platform: "slack", Step: NewOperationAccepted, Status: NewOperationRunning},
	}
	for _, seed := range seeds {
		if _, _, err := store.CreateOrGet(seed); err != nil {
			t.Fatal(err)
		}
	}
	ready := store.Ready("feishu", now)
	if len(ready) != 2 || ready[0].ID != "due" || ready[1].ID != "ready" {
		t.Fatalf("ready = %+v, want due and ready in ID order", ready)
	}
}

func TestNewOperationStore_PruneCompletedBeforeCutoff(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []NewOperation{
		{ID: "old", Step: NewOperationCompleted, Status: NewOperationStatusCompleted, CompletedAt: now.Add(-8 * 24 * time.Hour)},
		{ID: "recent", Step: NewOperationCompleted, Status: NewOperationStatusCompleted, CompletedAt: now.Add(-6 * 24 * time.Hour)},
		{ID: "manual", Step: NewOperationAccepted, Status: NewOperationManualAction, UpdatedAt: now.Add(-30 * 24 * time.Hour)},
	} {
		if _, _, err := store.CreateOrGet(seed); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PruneCompleted(now.Add(-7 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("old"); ok {
		t.Fatal("old completed operation was not pruned")
	}
	for _, id := range []string{"recent", "manual"} {
		if _, ok := store.Get(id); !ok {
			t.Fatalf("operation %q was pruned", id)
		}
	}
}

func TestNewOperationStore_CorruptFileFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNewOperationStore(path); err == nil {
		t.Fatal("corrupt store was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{" {
		t.Fatalf("corrupt store was overwritten: %q", got)
	}
}

func TestNewOperationStore_WriteFailureRollsBackMemoryAndPreservesSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.json")
	backup := filepath.Join(dir, "ops.backup.json")
	store, err := NewNewOperationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{ID: "op-1", Step: NewOperationAccepted}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update("op-1", func(op *NewOperation) error {
		op.Step = NewOperationConversationSpawned
		return nil
	}); err == nil {
		t.Fatal("Update succeeded despite an unwritable snapshot target")
	}
	got, _ := store.Get("op-1")
	if got.Step != NewOperationAccepted {
		t.Fatalf("in-memory step = %q, want rollback to %q", got.Step, NewOperationAccepted)
	}
	preserved, err := NewNewOperationStore(backup)
	if err != nil {
		t.Fatalf("preserved snapshot is unreadable: %v", err)
	}
	if got, ok := preserved.Get("op-1"); !ok || got.Step != NewOperationAccepted {
		t.Fatalf("preserved snapshot = %+v, exists=%v", got, ok)
	}
}

func TestNewOperationStore_ConcurrentUpdatesRemainReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	store, err := NewNewOperationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{ID: "op-1", Step: NewOperationAccepted}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = store.Update("op-1", func(op *NewOperation) error {
				op.LastError = fmt.Sprintf("error-%d", i)
				return nil
			})
		}(i)
	}
	wg.Wait()
	reloaded, err := NewNewOperationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("op-1")
	if !ok || got.Step != NewOperationAccepted || !strings.HasPrefix(got.LastError, "error-") {
		t.Fatalf("operation lost or corrupt: %+v", got)
	}
}

func TestNewOperationIDIsStableAndScoped(t *testing.T) {
	first := NewOperationID("project", "feishu:chat-a:user", "message-1")
	second := NewOperationID("project", "feishu:chat-a:user", "message-1")
	other := NewOperationID("project", "feishu:chat-a:user", "message-2")
	if first == "" || first != second || first == other {
		t.Fatalf("IDs = %q, %q, %q", first, second, other)
	}
	if strings.Contains(first, "chat-a") || strings.Contains(first, "message-1") {
		t.Fatalf("operation ID exposes source identifiers: %q", first)
	}
}

func TestNewOperationStore_RecoveryGateKeepsAllSourceConversationsAvailable(t *testing.T) {
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []NewOperation{
		{ID: "op-a", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user", Step: NewOperationAccepted, Status: NewOperationRunning},
		{ID: "op-b", Platform: "feishu", SourceSessionKey: "feishu:chat-b:user", Step: NewOperationAccepted, Status: NewOperationRunning},
	} {
		if _, _, err := store.CreateOrGet(op); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !store.WaitUntilMessageRecoverySafe(ctx, "feishu", "feishu:chat-a:user") {
		t.Fatal("source conversation was blocked by another unresolved /new operation")
	}
}

func TestNewOperationStore_RecoveryGateDoesNotBlockOnManualAction(t *testing.T) {
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               "op-manual",
		Platform:         "feishu",
		SourceSessionKey: "feishu:chat-a:user",
		Step:             NewOperationAccepted,
		Status:           NewOperationManualAction,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !store.WaitUntilMessageRecoverySafe(ctx, "feishu", "feishu:chat-unrelated:user") {
		t.Fatal("manual-action operation blocked an unrelated conversation")
	}
}
