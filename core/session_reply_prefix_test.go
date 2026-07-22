package core

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type sessionReplyPrefixPlatform struct {
	stubPlatformEngine
}

func (p *sessionReplyPrefixPlatform) ReplySessionKey(replyCtx any) string {
	key, _ := replyCtx.(string)
	return key
}

func (p *sessionReplyPrefixPlatform) FormatSessionReply(sessionName, content string) string {
	return "[" + sessionName + "] " + content
}

type sessionReplyPrefixDesktopPlatform struct {
	sessionReplyPrefixPlatform
	sentCtx []any
}

type sessionReplyPrefixReconstructingPlatform struct {
	sessionReplyPrefixPlatform
}

type blockingQueueSessionReplyPrefixPlatform struct {
	sessionReplyPrefixPlatform
	calls        atomic.Int32
	firstReached chan struct{}
	releaseFirst chan struct{}
}

func (p *blockingQueueSessionReplyPrefixPlatform) ReplySessionKey(replyCtx any) string {
	if p.calls.Add(1) == 1 {
		close(p.firstReached)
		<-p.releaseFirst
	}
	key, _ := replyCtx.(string)
	return key
}

func (p *sessionReplyPrefixReconstructingPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return sessionKey, nil
}

func (p *sessionReplyPrefixDesktopPlatform) ExternalConversationRelayEnabled() bool { return true }

func (p *sessionReplyPrefixDesktopPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return sessionKey, nil
}

func (p *sessionReplyPrefixDesktopPlatform) Send(_ context.Context, replyCtx any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	p.sentCtx = append(p.sentCtx, replyCtx)
	return nil
}

func TestSessionReplyPrefix_SendAndReplyFollowActiveSession(t *testing.T) {
	p := &sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	key := "weixin:dm:user-1"
	a := e.sessions.NewSession(key, "A")
	a.SetAgentSessionID("agent-a", "codex")

	if err := e.sendWithError(p, key, "send A"); err != nil {
		t.Fatal(err)
	}
	if err := e.replyWithError(p, key, "reply A"); err != nil {
		t.Fatal(err)
	}

	b := e.sessions.NewSession(key, "B")
	b.SetAgentSessionID("agent-b", "codex")
	if err := e.sendWithError(p, key, "send B"); err != nil {
		t.Fatal(err)
	}
	e.sessions.SetSessionName("agent-b", "Custom B")
	if err := e.sendWithError(p, key, "send custom B"); err != nil {
		t.Fatal(err)
	}

	want := []string{"[A] send A", "[A] reply A", "[B] send B", "[Custom B] send custom B"}
	if got := p.getSent(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %#v, want %#v", got, want)
	}
}

func TestSessionReplyPrefix_UsesWorkspaceSessionManager(t *testing.T) {
	baseDir := t.TempDir()
	e := newTestEngineWithMultiWorkspaceAgent(t, baseDir)
	t.Cleanup(func() { _ = e.Stop() })
	p := &sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}
	key := "weixin:dm:user-1"
	workspace := t.TempDir()
	e.bindSendWorkDir(key, workspace)
	_, workspaceSessions, err := e.getOrCreateWorkspaceAgent(workspace)
	if err != nil {
		t.Fatal(err)
	}
	e.sessions.NewSession(key, "Root")
	workspaceSessions.NewSession(key, "Workspace")

	if err := e.sendWithError(p, key, "hello"); err != nil {
		t.Fatal(err)
	}
	if got := p.getSent(); !reflect.DeepEqual(got, []string{"[Workspace] hello"}) {
		t.Fatalf("sent = %#v, want workspace prefix", got)
	}
}

func TestSessionReplyPrefix_LeavesUnsupportedMissingKeyAndEmptyContentUnchanged(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	key := "weixin:dm:user-1"
	e.sessions.NewSession(key, "A")
	prefixer := &sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}
	plain := &stubPlatformEngine{n: "telegram"}

	if err := e.sendWithError(plain, key, "plain"); err != nil {
		t.Fatal(err)
	}
	if err := e.sendWithError(prefixer, struct{}{}, "missing key"); err != nil {
		t.Fatal(err)
	}
	if err := e.sendWithError(prefixer, key, ""); err != nil {
		t.Fatal(err)
	}

	if got := plain.getSent(); !reflect.DeepEqual(got, []string{"plain"}) {
		t.Fatalf("plain sent = %#v", got)
	}
	if got := prefixer.getSent(); !reflect.DeepEqual(got, []string{"missing key", ""}) {
		t.Fatalf("prefixer sent = %#v", got)
	}
}

func TestSessionReplyPrefix_AppliesToBuiltInCommandAndError(t *testing.T) {
	p := &sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	key := "weixin:dm:user-1"
	e.sessions.NewSession(key, "A")
	msg := &Message{SessionKey: key, UserID: "user-1", Platform: p.Name(), ReplyCtx: key}
	oldVersionInfo := VersionInfo
	VersionInfo = "test-version"
	t.Cleanup(func() { VersionInfo = oldVersionInfo })

	e.handleCommand(p, msg, "/version")
	e.handleCommand(p, msg, "/shell echo hello")

	got := p.getSent()
	if len(got) != 2 {
		t.Fatalf("sent = %#v, want command and error", got)
	}
	if got[0] != "[A] test-version" {
		t.Fatalf("command reply = %q", got[0])
	}
	if !strings.HasPrefix(got[1], "[A] ") || !strings.Contains(got[1], "/shell") {
		t.Fatalf("error reply = %q, want session prefix and command", got[1])
	}
}

func TestSessionReplyPrefix_DesktopLiveSyncPrecedesCodexLabel(t *testing.T) {
	agent := &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
		"thread-1": {{SessionID: "thread-1", Role: "assistant", Content: "App reply"}},
	}}
	p := &sessionReplyPrefixDesktopPlatform{sessionReplyPrefixPlatform: sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}}
	e := NewEngine("test", agent, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	key := "weixin:dm:user-1"
	s := e.sessions.NewSession(key, "A")
	s.SetAgentSessionID("thread-1", "codex")
	e.platformLifecycleMu.Lock()
	e.platformReady[p] = true
	e.platformLifecycleMu.Unlock()

	e.pollDesktopLiveSyncRoutes(context.Background(), agent, e.sessions)

	if got := p.getSent(); !reflect.DeepEqual(got, []string{"[A] ✣ Codex · 回复\nApp reply"}) {
		t.Fatalf("sent = %#v", got)
	}
}

func TestSessionReplyPrefix_SendToSessionDecoratesTextOnly(t *testing.T) {
	p := &sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	key := "weixin:dm:user-1"
	e.sessions.NewSession(key, "A")
	e.interactiveStates[key] = &interactiveState{platform: p, replyCtx: key}

	if err := e.SendToSessionWithOptions(key, "side channel", nil, nil, SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := p.getSent(); !reflect.DeepEqual(got, []string{"[A] side channel"}) {
		t.Fatalf("sent = %#v", got)
	}
}

func TestSessionReplyPrefix_RestartNotificationUsesActiveSession(t *testing.T) {
	p := &sessionReplyPrefixReconstructingPlatform{sessionReplyPrefixPlatform: sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	key := "weixin:dm:user-1"
	e.sessions.NewSession(key, "A")
	e.platformLifecycleMu.Lock()
	e.platformReady[p] = true
	e.platformLifecycleMu.Unlock()

	if err := e.dispatchRestartNotify(&RestartRequest{Platform: p.Name(), SessionKey: key}); err != nil {
		t.Fatal(err)
	}
	got := p.getSent()
	if len(got) != 1 || !strings.HasPrefix(got[0], "[A] ") || !strings.Contains(got[0], "重启成功") {
		t.Fatalf("restart notification = %#v", got)
	}
}

func TestSessionReplyPrefix_NewOperationNotificationsUseSourceAndTargetSessions(t *testing.T) {
	p := &sessionReplyPrefixReconstructingPlatform{sessionReplyPrefixPlatform: sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	sourceKey := "weixin:dm:source"
	targetKey := "weixin:dm:target"
	e.sessions.NewSession(sourceKey, "Source")
	e.sessions.NewSession(targetKey, "Target")
	op, _, err := store.CreateOrGet(NewOperation{
		ID:               "prefix-notifications",
		Project:          "test",
		Platform:         p.Name(),
		SourceSessionKey: sourceKey,
		SourceMessageID:  "message-1",
		UserID:           "user-1",
		Name:             "[Codex] Target",
		Step:             NewOperationNameSynced,
		Status:           NewOperationRunning,
		TargetSessionKey: targetKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, op.ID, sourceKey, e.agent, e.sessions)

	got := p.getSent()
	if len(got) != 2 || !strings.HasPrefix(got[0], "[Target] ") || !strings.HasPrefix(got[1], "[Source] ") {
		t.Fatalf("new operation notifications = %#v", got)
	}
}

func TestSessionReplyPrefix_NewOperationFailureUsesSourceSession(t *testing.T) {
	p := &sessionReplyPrefixReconstructingPlatform{sessionReplyPrefixPlatform: sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	key := "weixin:dm:source"
	e.sessions.NewSession(key, "Source")
	op, _, err := store.CreateOrGet(NewOperation{
		ID:               "prefix-failure",
		Project:          "test",
		Platform:         p.Name(),
		SourceSessionKey: key,
		SourceMessageID:  "message-1",
		UserID:           "user-1",
		Name:             "[Codex] Target",
		Step:             NewOperationAccepted,
		Status:           NewOperationRunning,
	})
	if err != nil {
		t.Fatal(err)
	}

	e.failNewOperationWithNotice(context.Background(), p, op, key, NewPermanentOperationError(errors.New("permission denied")))

	got := p.getSent()
	if len(got) != 1 || !strings.HasPrefix(got[0], "[Source] ") {
		t.Fatalf("new operation failure notification = %#v", got)
	}
}

func TestSessionReplyPrefix_RelayVisibilityUsesActiveSession(t *testing.T) {
	p := &sessionReplyPrefixReconstructingPlatform{sessionReplyPrefixPlatform: sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	key := "weixin:dm:user-1"
	e.sessions.NewSession(key, "A")
	rm := NewRelayManager("")

	rm.sendToGroup(context.Background(), e, p.Name(), key, "relay result")

	if got := p.getSent(); !reflect.DeepEqual(got, []string{"[A] relay result"}) {
		t.Fatalf("relay visibility = %#v", got)
	}
}

func TestQueueMessageForBusySession_ConcurrentPrefixedRepliesDoNotDeadlock(t *testing.T) {
	p := &blockingQueueSessionReplyPrefixPlatform{
		sessionReplyPrefixPlatform: sessionReplyPrefixPlatform{stubPlatformEngine: stubPlatformEngine{n: "weixin"}},
		firstReached:               make(chan struct{}),
		releaseFirst:               make(chan struct{}),
	}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	defer e.cancel()
	e.SetMultiWorkspace(t.TempDir(), filepath.Join(t.TempDir(), "bindings.json"))
	key := "weixin:dm:user-1"
	e.sessions.NewSession(key, "A")
	state := &interactiveState{platform: p, replyCtx: key}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	results := make(chan bool, 2)
	go func() {
		results <- e.queueMessageForBusySession(p, &Message{SessionKey: key, MessageID: "m1", Content: "one", ReplyCtx: key}, key)
	}()
	select {
	case <-p.firstReached:
	case <-time.After(time.Second):
		t.Fatal("first queued reply did not reach prefix formatting")
	}
	go func() {
		results <- e.queueMessageForBusySession(p, &Message{SessionKey: key, MessageID: "m2", Content: "two", ReplyCtx: key}, key)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case result := <-results:
			results <- result
			deadline = time.Time{}
		default:
			if !e.interactiveMu.TryLock() {
				deadline = time.Time{}
				break
			}
			e.interactiveMu.Unlock()
			time.Sleep(time.Millisecond)
		}
		if deadline.IsZero() {
			break
		}
	}
	close(p.releaseFirst)

	for i := 0; i < 2; i++ {
		select {
		case ok := <-results:
			if !ok {
				t.Fatal("concurrent busy-session message was not queued")
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("concurrent busy-session queue timed out: lock-order deadlock")
		}
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.pendingMessages) != 2 || state.pendingMessages[0].content != "one" || state.pendingMessages[1].content != "two" {
		t.Fatalf("pending messages = %#v, want FIFO [one two]", state.pendingMessages)
	}
}
