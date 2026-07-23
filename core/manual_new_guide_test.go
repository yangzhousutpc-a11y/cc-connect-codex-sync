package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type manualNewGuidePlatform struct {
	*spawningPlatform
}

func (p *manualNewGuidePlatform) ManualNewConversationGuide() string {
	return "  请手动新建企业微信群，再将机器人添加到群聊。  "
}

func TestCmdNewManualGuideDoesNotCreateConversationOrOperation(t *testing.T) {
	p := &manualNewGuidePlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "wecom"},
		spawned: SpawnedConversation{
			SessionKey: "wecom:g:chat-b",
			ReplyCtx:   "ctx-b",
		},
	}}
	startCalls := 0
	agent := &controllableAgent{
		startSessionFn: func(context.Context, string) (AgentSession, error) {
			startCalls++
			return newControllableSession("thread-b"), nil
		},
	}
	e := NewEngine("test", agent, []Platform{p}, "", LangChinese)
	operationPath := filepath.Join(t.TempDir(), "operations.json")
	emptyOperationSnapshot, err := json.Marshal(newOperationSnapshot{
		Version:    newOperationSnapshotVersion,
		Operations: map[string]NewOperation{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operationPath, emptyOperationSnapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	const sourceKey = "wecom:g:chat-a"
	sourceSession := e.sessions.NewSession(sourceKey, "现有企业微信会话")
	sourceSession.SetAgentSessionID("thread-existing", "codex")
	sourceSession.SetActiveProvider("openai")
	sourceSession.AddHistory("user", "现有消息")
	sourceSessionID := sourceSession.ID
	beforeActiveSessionID := e.sessions.ActiveSessionID(sourceKey)
	beforeSourceRecord, err := json.Marshal(sourceSession)
	if err != nil {
		t.Fatal(err)
	}
	beforeSessionIDs := manualNewSessionRecordIDs(e.sessions)
	beforeIDToKey, beforeActiveIDs := e.sessions.SessionKeyMap()

	e.cmdNew(p, &Message{
		SessionKey: sourceKey,
		UserID:     "user-1",
		MessageID:  "message-manual-new",
		ReplyCtx:   "ctx-a",
	}, []string{"新项目"})

	if got := e.sessions.ActiveSessionID(sourceKey); got != beforeActiveSessionID || got != sourceSessionID {
		t.Fatalf("active source session = %q, want unchanged %q", got, sourceSessionID)
	}
	reloadedSource := e.sessions.FindByID(sourceSessionID)
	if reloadedSource == nil {
		t.Fatalf("source session %q no longer exists", sourceSessionID)
	}
	if got := reloadedSource.GetName(); got != "现有企业微信会话" {
		t.Fatalf("source session name = %q, want unchanged", got)
	}
	if got := reloadedSource.GetAgentSessionID(); got != "thread-existing" {
		t.Fatalf("source agent session ID = %q, want unchanged", got)
	}
	if got := reloadedSource.AgentType; got != "codex" {
		t.Fatalf("source agent type = %q, want unchanged", got)
	}
	if got := reloadedSource.GetActiveProvider(); got != "openai" {
		t.Fatalf("source active provider = %q, want unchanged", got)
	}
	afterSourceRecord, err := json.Marshal(reloadedSource)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterSourceRecord, beforeSourceRecord) {
		t.Fatalf("source session record changed:\nbefore: %s\nafter:  %s", beforeSourceRecord, afterSourceRecord)
	}
	if got := manualNewSessionRecordIDs(e.sessions); !reflect.DeepEqual(got, beforeSessionIDs) {
		t.Fatalf("session records = %#v, want unchanged %#v", got, beforeSessionIDs)
	}
	afterIDToKey, afterActiveIDs := e.sessions.SessionKeyMap()
	if !reflect.DeepEqual(afterIDToKey, beforeIDToKey) {
		t.Fatalf("session key map = %#v, want unchanged %#v", afterIDToKey, beforeIDToKey)
	}
	if !reflect.DeepEqual(afterActiveIDs, beforeActiveIDs) {
		t.Fatalf("active session identities = %#v, want unchanged %#v", afterActiveIDs, beforeActiveIDs)
	}
	if len(p.requests) != 0 {
		t.Fatalf("SpawnConversation calls = %d, want 0", len(p.requests))
	}
	if startCalls != 0 {
		t.Fatalf("StartSession calls = %d, want 0", startCalls)
	}
	store.mu.RLock()
	inMemoryOperationCount := len(store.operations)
	store.mu.RUnlock()
	if inMemoryOperationCount != 0 {
		t.Fatalf("in-memory new operation count = %d, want 0", inMemoryOperationCount)
	}
	persistedStore, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	persistedStore.mu.RLock()
	persistedOperationCount := len(persistedStore.operations)
	persistedStore.mu.RUnlock()
	if persistedOperationCount != 0 {
		t.Fatalf("persisted new operation count = %d, want 0", persistedOperationCount)
	}
	replies := p.getSent()
	if len(replies) != 1 {
		t.Fatalf("reply count = %d, want 1: %#v", len(replies), replies)
	}
	if !strings.Contains(replies[0], "手动新建企业微信群") {
		t.Fatalf("reply = %q, want manual WeCom group guide", replies[0])
	}
}

func manualNewSessionRecordIDs(sessions *SessionManager) []string {
	records := sessions.AllSessions()
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	sort.Strings(ids)
	return ids
}
