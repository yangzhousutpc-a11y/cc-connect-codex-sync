package core

import (
	"context"
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
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	const sourceKey = "wecom:g:chat-a"
	e.sessions.NewSession(sourceKey, "现有会话")
	beforeCount := len(e.sessions.ListSessions(sourceKey))
	beforeTotal := len(e.sessions.AllSessions())
	messageID := "message-manual-new"

	e.cmdNew(p, &Message{
		SessionKey: sourceKey,
		UserID:     "user-1",
		MessageID:  messageID,
		ReplyCtx:   "ctx-a",
	}, []string{"新项目"})

	if got := len(e.sessions.ListSessions(sourceKey)); got != beforeCount {
		t.Fatalf("source session count = %d, want unchanged %d", got, beforeCount)
	}
	if got := len(e.sessions.AllSessions()); got != beforeTotal {
		t.Fatalf("total session count = %d, want unchanged %d", got, beforeTotal)
	}
	if len(p.requests) != 0 {
		t.Fatalf("SpawnConversation calls = %d, want 0", len(p.requests))
	}
	if startCalls != 0 {
		t.Fatalf("StartSession calls = %d, want 0", startCalls)
	}
	if _, ok := store.Get(NewOperationID("test", sourceKey, messageID)); ok {
		t.Fatal("new operation was created")
	}
	replies := p.getSent()
	if len(replies) != 1 {
		t.Fatalf("reply count = %d, want 1: %#v", len(replies), replies)
	}
	if !strings.Contains(replies[0], "手动新建企业微信群") {
		t.Fatalf("reply = %q, want manual WeCom group guide", replies[0])
	}
}
