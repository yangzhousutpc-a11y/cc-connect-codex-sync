package core

import (
	"path/filepath"
	"testing"
)

type stableConversationResolverPlatform struct {
	*stubPlatformEngine
	resolver func(string) bool
}

func (p *stableConversationResolverPlatform) SetStableConversationSessionResolver(resolver func(string) bool) {
	p.resolver = resolver
}

func TestSetNewOperationStoreRegistersStableConversationResolver(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.json")
	operationPath := filepath.Join(dir, "new-operations.json")
	persistedSessions := NewSessionManager(sessionPath)
	persistedSessions.NewSession("feishu:oc_persisted", "persisted")
	persistedSessions.Save()

	persistedStore, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := persistedStore.CreateOrGet(NewOperation{
		ID:               "new-pending-target",
		Project:          "test",
		Platform:         "feishu",
		SourceSessionKey: "feishu:oc_source",
		SourceMessageID:  "om_new",
		UserID:           "ou_user",
		Name:             "[Codex] 新会话",
		Step:             NewOperationConversationSpawned,
		Status:           NewOperationRunning,
		TargetSessionKey: "feishu:oc_pending",
		AgentRecoveryKey: "cc-connect/new/new-pending-target",
	}); err != nil {
		t.Fatal(err)
	}

	p := &stableConversationResolverPlatform{stubPlatformEngine: &stubPlatformEngine{n: "feishu"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, sessionPath, LangChinese)
	reloadedStore, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(reloadedStore)
	if p.resolver == nil {
		t.Fatal("stable conversation resolver was not registered")
	}
	for _, sessionKey := range []string{"feishu:oc_persisted", "feishu:oc_pending"} {
		if !p.resolver(sessionKey) {
			t.Fatalf("resolver(%q) = false, want true", sessionKey)
		}
	}
	if p.resolver("feishu:oc_unknown") {
		t.Fatal("resolver accepted an unknown group key")
	}
}
