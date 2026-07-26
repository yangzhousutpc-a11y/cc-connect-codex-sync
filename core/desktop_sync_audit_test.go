package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type desktopSyncFailingPlatform struct{ desktopSyncPlatform }

type desktopSyncFlakyPlatform struct {
	desktopSyncPlatform
	failures int
	attempts []string
}

func (p *desktopSyncFlakyPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	p.attempts = append(p.attempts, content)
	if p.failures > 0 {
		p.failures--
		return errors.New("transient send failure")
	}
	return p.desktopSyncPlatform.Send(ctx, replyCtx, content)
}

type desktopSyncSpawningPlatform struct {
	desktopSyncPlatform
	spawned SpawnedConversation
}

func (p *desktopSyncSpawningPlatform) SpawnConversation(_ context.Context, _ ConversationSpawnRequest) (SpawnedConversation, error) {
	return p.spawned, nil
}

type trackingDesktopSyncAgent struct {
	*desktopSyncAgent
	routeSets [][]string
	pollCalls []string
}

type desktopNamingAgent struct {
	*trackingDesktopSyncAgent
	startSessionFn func(context.Context, string) (AgentSession, error)
	startCalls     []string
}

func (a *desktopNamingAgent) Name() string { return "desktop-naming" }

func (a *desktopNamingAgent) StartSession(ctx context.Context, sessionID string) (AgentSession, error) {
	a.startCalls = append(a.startCalls, sessionID)
	if a.startSessionFn != nil {
		return a.startSessionFn(ctx, sessionID)
	}
	return nil, errors.New("unexpected StartSession")
}

func (a *trackingDesktopSyncAgent) SetExternalConversationRoutes(sessionIDs []string) {
	a.routeSets = append(a.routeSets, append([]string(nil), sessionIDs...))
}

func (a *trackingDesktopSyncAgent) PollExternalConversation(ctx context.Context, sessionID string) ([]ExternalConversationEvent, error) {
	a.pollCalls = append(a.pollCalls, sessionID)
	return a.desktopSyncAgent.PollExternalConversation(ctx, sessionID)
}

func (p *desktopSyncFailingPlatform) Send(context.Context, any, string) error {
	return errors.New("send failed")
}

func TestDesktopLiveSyncLogsSuccessfulRelayWithoutMessageContent(t *testing.T) {
	const secretBody = "audit-secret-body"
	agent := &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
		"thread-audit": {{SessionID: "thread-audit", Role: "user", Content: secretBody}},
	}}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession("feishu:chat-audit:user", "audit")
	session.SetAgentSessionID("thread-audit", "codex")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	previous := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	engine.pollDesktopLiveSync(context.Background(), agent)

	output := logs.String()
	if !strings.Contains(output, `msg="desktop live sync sent"`) {
		t.Fatalf("success audit missing: %s", output)
	}
	if !strings.Contains(output, "role=user") || !strings.Contains(output, "content_len=17") {
		t.Fatalf("audit metadata missing: %s", output)
	}
	if strings.Contains(output, secretBody) {
		t.Fatalf("audit leaked message content: %s", output)
	}
}

func TestDesktopLiveSyncDoesNotLogSuccessWhenSendFails(t *testing.T) {
	agent := &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
		"thread-failure": {{SessionID: "thread-failure", Role: "assistant", Content: "not-delivered"}},
	}}
	platform := &desktopSyncFailingPlatform{
		desktopSyncPlatform: desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}},
	}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession("feishu:chat-failure:user", "failure")
	session.SetAgentSessionID("thread-failure", "codex")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	previous := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	engine.pollDesktopLiveSync(context.Background(), agent)

	output := logs.String()
	if strings.Contains(output, `msg="desktop live sync sent"`) {
		t.Fatalf("false success audit: %s", output)
	}
	if !strings.Contains(output, `msg="desktop live sync send failed"`) {
		t.Fatalf("failure audit missing: %s", output)
	}
}

func TestDesktopLiveSyncMediaLogsDoNotLeakIdentifiersOrAttachmentData(t *testing.T) {
	const (
		secretSession = "thread-secret-media"
		secretName    = "secret-local-path-name.png"
		secretData    = "secret-attachment-bytes"
	)
	agent := &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
		secretSession: {{
			SessionID: secretSession,
			Role:      "user",
			Images: []ImageAttachment{{
				FileName: secretName,
				Data:     []byte(secretData),
			}},
		}},
	}}
	platform := &desktopMediaPlatform{desktopSyncPlatform: desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.sessions.NewSession("feishu:secret-chat:secret-user", "audit").SetAgentSessionID(secretSession, "codex")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	previous := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	engine.pollDesktopLiveSync(context.Background(), agent)

	output := logs.String()
	for _, secret := range []string{secretSession, "secret-chat", "secret-user", secretName, secretData} {
		if strings.Contains(output, secret) {
			t.Fatalf("desktop media audit leaked secret metadata")
		}
	}
}

func TestDesktopLiveSyncRetriesUndeliveredEventsWithoutRepolling(t *testing.T) {
	agent := &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
		"thread-retry": {
			{SessionID: "thread-retry", Role: "user", Content: "first"},
			{SessionID: "thread-retry", Role: "assistant", Content: "second"},
		},
	}}}
	platform := &desktopSyncFlakyPlatform{
		desktopSyncPlatform: desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}},
		failures:            1,
	}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.sessions.NewSession("feishu:chat-retry:user", "retry").SetAgentSessionID("thread-retry", "codex")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.pollDesktopLiveSync(context.Background(), agent)

	if got, want := platform.attempts, []string{"Codex App · 你\nfirst", "Codex App · 你\nfirst", "Codex · 回复\nsecond"}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("send attempts = %#v, want %#v", got, want)
	}
	platform.mu.Lock()
	delivered := append([]string(nil), platform.sent...)
	platform.mu.Unlock()
	if got, want := delivered, []string{"Codex App · 你\nfirst", "Codex · 回复\nsecond"}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("delivered events = %#v, want %#v", got, want)
	}
	if got := agent.pollCalls; len(got) != 1 || got[0] != "thread-retry" {
		t.Fatalf("poll calls = %#v, want one poll while retry is pending", got)
	}

	engine.pollDesktopLiveSync(context.Background(), agent)
	platform.mu.Lock()
	deliveredAfterAck := append([]string(nil), platform.sent...)
	platform.mu.Unlock()
	if got, want := deliveredAfterAck, []string{"Codex App · 你\nfirst", "Codex · 回复\nsecond"}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("delivery after acknowledged retry = %#v, want no duplicates %#v", got, want)
	}
}

func TestDesktopLiveSyncReassertsCustomNameOnceAfterForwardingWholeTurnOnLiveSession(t *testing.T) {
	const (
		threadID   = "thread-name-live"
		sessionKey = "wecom:g:name-live"
		customName = "[企业微信-Codex] 实际企微群名"
	)
	agent := &desktopNamingAgent{trackingDesktopSyncAgent: &trackingDesktopSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			threadID: {{SessionID: threadID, Role: "user", Content: "桌面提问"}},
		}},
	}}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	logical := engine.sessions.NewSession(sessionKey, customName)
	logical.SetAgentSessionID(threadID, agent.Name())
	engine.sessions.SetSessionName(threadID, customName)
	live := &namingAgentSession{controllableAgentSession: newControllableSession(threadID)}
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
	}
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	if len(live.names) != 0 {
		t.Fatalf("live name syncs after user event = %#v, want none before assistant completes the turn", live.names)
	}
	agent.mu.Lock()
	agent.events[threadID] = []ExternalConversationEvent{
		{SessionID: threadID, Role: "assistant", Content: "桌面回答"},
	}
	agent.mu.Unlock()
	engine.pollDesktopLiveSync(context.Background(), agent)

	platform.mu.Lock()
	delivered := append([]string(nil), platform.sent...)
	platform.mu.Unlock()
	wantDelivered := []string{"Codex App · 你\n桌面提问", "Codex · 回复\n桌面回答"}
	if !equalDesktopSyncStrings(delivered, wantDelivered) {
		t.Fatalf("delivered events = %#v, want %#v", delivered, wantDelivered)
	}
	if got := live.names; len(got) != 1 || got[0] != customName {
		t.Fatalf("live name syncs = %#v, want one reassertion of %q", got, customName)
	}
	if len(agent.startCalls) != 0 {
		t.Fatalf("StartSession calls = %#v, want live session reuse", agent.startCalls)
	}
	if len(engine.desktopSyncPending) != 0 {
		t.Fatalf("desktop pending queue = %#v, want empty after full turn", engine.desktopSyncPending)
	}
	if got := logical.GetAgentSessionID(); got != threadID {
		t.Fatalf("bound thread = %q, want unchanged %q", got, threadID)
	}
}

func TestDesktopLiveSyncRestoresOnlyBoundThreadToReassertCustomName(t *testing.T) {
	const (
		threadID   = "thread-name-restore"
		sessionKey = "wecom:g:name-restore"
		customName = "[企业微信-Codex] 恢复群名"
	)
	restored := &namingAgentSession{controllableAgentSession: newControllableSession(threadID)}
	agent := &desktopNamingAgent{
		trackingDesktopSyncAgent: &trackingDesktopSyncAgent{
			desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
				threadID: {
					{SessionID: threadID, Role: "user", Content: "恢复提问"},
					{SessionID: threadID, Role: "assistant", Content: "恢复回答"},
				},
			}},
		},
		startSessionFn: func(_ context.Context, sessionID string) (AgentSession, error) {
			if sessionID != threadID {
				t.Fatalf("StartSession target = %q, want bound thread %q", sessionID, threadID)
			}
			return restored, nil
		},
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	logical := engine.sessions.NewSession(sessionKey, customName)
	logical.SetAgentSessionID(threadID, agent.Name())
	engine.sessions.SetSessionName(threadID, customName)
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)

	if got := agent.startCalls; len(got) != 1 || got[0] != threadID {
		t.Fatalf("StartSession calls = %#v, want only bound thread %q", got, threadID)
	}
	if got := restored.names; len(got) != 1 || got[0] != customName {
		t.Fatalf("restored name syncs = %#v, want %#v", got, []string{customName})
	}
	if restored.closeCalls != 1 {
		t.Fatalf("restored Close calls = %d, want 1", restored.closeCalls)
	}
	if got := logical.GetAgentSessionID(); got != threadID {
		t.Fatalf("bound thread = %q, want unchanged %q", got, threadID)
	}
	if len(engine.interactiveStates) != 0 {
		t.Fatalf("interactive states = %#v, want no new live route", engine.interactiveStates)
	}
	if len(engine.desktopSyncPending) != 0 {
		t.Fatalf("desktop pending queue = %#v, want empty after full turn", engine.desktopSyncPending)
	}
}

func TestDesktopLiveSyncNameFailureDoesNotRollBackRelayAndRetriesNextTurn(t *testing.T) {
	const (
		threadID   = "thread-name-retry"
		sessionKey = "wecom:g:name-retry"
		customName = "[企业微信-Codex] 重试群名"
	)
	agent := &desktopNamingAgent{trackingDesktopSyncAgent: &trackingDesktopSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			threadID: {
				{SessionID: threadID, Role: "user", Content: "第一轮提问"},
				{SessionID: threadID, Role: "assistant", Content: "第一轮回答"},
			},
		}},
	}}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	logical := engine.sessions.NewSession(sessionKey, customName)
	logical.SetAgentSessionID(threadID, agent.Name())
	engine.sessions.SetSessionName(threadID, customName)
	live := &namingAgentSession{
		controllableAgentSession: newControllableSession(threadID),
		err:                      errors.New("temporary name sync failure"),
	}
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
	}
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	previous := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	engine.pollDesktopLiveSync(context.Background(), agent)
	if len(engine.desktopSyncPending) != 0 {
		t.Fatalf("desktop pending queue after name failure = %#v, want relay acknowledged", engine.desktopSyncPending)
	}

	live.err = nil
	agent.mu.Lock()
	agent.events[threadID] = []ExternalConversationEvent{
		{SessionID: threadID, Role: "user", Content: "第二轮提问"},
		{SessionID: threadID, Role: "assistant", Content: "第二轮回答"},
	}
	agent.mu.Unlock()
	engine.pollDesktopLiveSync(context.Background(), agent)

	platform.mu.Lock()
	delivered := append([]string(nil), platform.sent...)
	platform.mu.Unlock()
	wantDelivered := []string{
		"Codex App · 你\n第一轮提问", "Codex · 回复\n第一轮回答",
		"Codex App · 你\n第二轮提问", "Codex · 回复\n第二轮回答",
	}
	if !equalDesktopSyncStrings(delivered, wantDelivered) {
		t.Fatalf("delivered events = %#v, want %#v", delivered, wantDelivered)
	}
	if got := live.names; len(got) != 2 || got[0] != customName || got[1] != customName {
		t.Fatalf("name attempts = %#v, want one failed then one successful retry", got)
	}
	if !strings.Contains(logs.String(), `msg="desktop live sync name reassertion failed"`) {
		t.Fatalf("name failure audit missing: %s", logs.String())
	}
	if len(engine.desktopSyncPending) != 0 {
		t.Fatalf("desktop pending queue after retry = %#v, want empty", engine.desktopSyncPending)
	}
	if got := logical.GetAgentSessionID(); got != threadID {
		t.Fatalf("bound thread = %q, want unchanged %q", got, threadID)
	}
}

func TestDesktopLiveSyncKeepsRouteWhilePlatformTemporarilyUnavailable(t *testing.T) {
	agent := &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)}}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.sessions.NewSession("feishu:chat-reconnect:user", "reconnect").SetAgentSessionID("thread-reconnect", "codex")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	if !engine.markPlatformUnavailable(platform) {
		t.Fatal("failed to mark platform unavailable")
	}
	agent.events["thread-reconnect"] = []ExternalConversationEvent{{SessionID: "thread-reconnect", Role: "user", Content: "offline message"}}
	pollsBeforeOffline := len(agent.pollCalls)
	engine.pollDesktopLiveSync(context.Background(), agent)

	lastRoutes := agent.routeSets[len(agent.routeSets)-1]
	if len(lastRoutes) != 1 || lastRoutes[0] != "thread-reconnect" {
		t.Fatalf("routes while unavailable = %#v, want retained route", lastRoutes)
	}
	if len(agent.pollCalls) != pollsBeforeOffline {
		t.Fatalf("poll calls while unavailable = %#v, want no cursor advance", agent.pollCalls)
	}

	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready again")
	}
	engine.pollDesktopLiveSync(context.Background(), agent)
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if got, want := platform.sent, []string{"Codex App · 你\noffline message"}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("delivered after reconnect = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncRestoresPersistedWorkspaceRoutesOnColdStart(t *testing.T) {
	baseDir := t.TempDir()
	workspaceDir := filepath.Join(baseDir, "workspace-cold")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	sessionStore := filepath.Join(dataDir, "test.json")
	bindingStore := filepath.Join(dataDir, "bindings.json")
	threadID := "thread-cold-start"
	sessionKey := "feishu:chat-cold:user-1"

	h := sha256.Sum256([]byte(normalizeWorkspacePath(workspaceDir)))
	workspaceSessionStore := filepath.Join(filepath.Dir(sessionStore), "test_ws_"+hex.EncodeToString(h[:4])+".json")
	persisted := NewSessionManager(workspaceSessionStore)
	persisted.NewSession(sessionKey, "cold").SetAgentSessionID(threadID, "desktop-sync-cold-agent")
	persisted.Save()

	bindings := NewWorkspaceBindingManager(bindingStore)
	bindings.Bind("project:test", "feishu:chat-cold", "cold group", workspaceDir)

	agentName := "desktop-sync-cold-agent"
	var workspaceAgent *trackingDesktopSyncAgent
	RegisterAgent(agentName, func(opts map[string]any) (Agent, error) {
		workspaceAgent = &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{
			stubAgent: stubAgent{},
			events: map[string][]ExternalConversationEvent{
				threadID: {{SessionID: threadID, Role: "user", Content: "cold-start message"}},
			},
		}}
		return &namedDesktopSyncAgent{name: agentName, trackingDesktopSyncAgent: workspaceAgent}, nil
	})
	rootAgent := &namedDesktopSyncAgent{name: agentName, trackingDesktopSyncAgent: &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)}}}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	engine := NewEngine("test", rootAgent, []Platform{platform}, sessionStore, LangChinese)
	engine.SetMultiWorkspace(baseDir, bindingStore)
	if len(engine.workspacePool.All()) != 0 {
		t.Fatal("workspace pool unexpectedly warm before cold-start poll")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	// Workspace restoration must not depend on an inbound platform message or
	// even on the asynchronously connecting platform already being ready.
	engine.pollDesktopLiveSync(context.Background(), rootAgent)
	if workspaceAgent == nil {
		t.Fatal("persisted workspace was not restored")
	}
	if got := workspaceAgent.pollCalls; len(got) != 0 {
		t.Fatalf("workspace polled while platform unavailable: %#v", got)
	}
	if len(workspaceAgent.routeSets) != 1 || len(workspaceAgent.routeSets[0]) != 1 || workspaceAgent.routeSets[0][0] != threadID {
		t.Fatalf("restored route set = %#v, want %q", workspaceAgent.routeSets, threadID)
	}

	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	engine.pollDesktopLiveSync(context.Background(), rootAgent)
	if got := workspaceAgent.pollCalls; len(got) != 1 || got[0] != threadID {
		t.Fatalf("workspace poll calls = %#v, want restored thread", got)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if got, want := platform.sent, []string{"Codex App · 你\ncold-start message"}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("cold-start delivery = %#v, want %#v", got, want)
	}
}

type namedDesktopSyncAgent struct {
	name string
	*trackingDesktopSyncAgent
}

func (a *namedDesktopSyncAgent) Name() string { return a.name }

func equalDesktopSyncStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDesktopLiveSyncRelaysMultiWorkspaceNewTargetAppEventOnce(t *testing.T) {
	baseDir := t.TempDir()
	workspaceDir := filepath.Join(baseDir, "workspace-a")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootAgent := &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)}
	workspaceAgent := &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)}
	platform := &desktopSyncSpawningPlatform{
		desktopSyncPlatform: desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}},
		spawned: SpawnedConversation{
			SessionKey: "feishu:chat-b:user-1",
			ReplyCtx:   "ctx-b",
		},
	}
	engine := NewEngine("test", rootAgent, []Platform{platform}, "", LangChinese)
	engine.SetMultiWorkspace(baseDir, filepath.Join(t.TempDir(), "bindings.json"))
	engine.workspaceBindings.Bind("project:test", "feishu:chat-a", "A群", workspaceDir)
	workspace := engine.workspacePool.GetOrCreate(workspaceDir)
	workspace.agent = workspaceAgent
	workspace.sessions = NewSessionManager("")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.cmdNew(platform, &Message{
		Platform:   "feishu",
		SessionKey: "feishu:chat-a:user-1",
		UserID:     "user-1",
		MessageID:  "message-workspace-new",
		ChatName:   "A群",
		ReplyCtx:   "ctx-a",
	}, []string{"新项目"})

	target := workspace.sessions.GetOrCreateActive("feishu:chat-b:user-1")
	threadID := target.GetAgentSessionID()
	if threadID == "" {
		t.Fatal("/new did not bind a Codex session to the workspace target")
	}
	workspaceAgent.events[threadID] = []ExternalConversationEvent{{SessionID: threadID, Role: "user", Content: "B 群 App 消息"}}
	platform.mu.Lock()
	platform.sent = nil
	platform.sentCtx = nil
	platform.mu.Unlock()

	engine.pollDesktopLiveSync(context.Background(), rootAgent)

	platform.mu.Lock()
	defer platform.mu.Unlock()
	if got, want := platform.sent, []string{"Codex App · 你\nB 群 App 消息"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("desktop relay = %#v, want %#v", got, want)
	}
	if got, want := platform.sentCtx, []any{"reconstructed:feishu:chat-b:user-1"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("desktop relay contexts = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncTrackerOnlyReceivesFeishuRoutes(t *testing.T) {
	t.Run("pure non-feishu routes", func(t *testing.T) {
		agent := &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)}}
		engine := NewEngine("test", agent, nil, "", LangChinese)
		engine.sessions.NewSession("telegram:chat-1:user-1", "telegram").SetAgentSessionID("thread-telegram", "codex")

		engine.pollDesktopLiveSync(context.Background(), agent)

		if len(agent.routeSets) != 1 || len(agent.routeSets[0]) != 0 {
			t.Fatalf("tracked routes = %#v, want one empty Feishu route set", agent.routeSets)
		}
		if len(agent.pollCalls) != 0 {
			t.Fatalf("poll calls = %#v, want none", agent.pollCalls)
		}
	})

	t.Run("mixed routes", func(t *testing.T) {
		agent := &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)}}
		platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "lark"}}
		engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
		engine.sessions.NewSession("lark:chat-1:user-1", "lark").SetAgentSessionID("thread-feishu", "codex")
		engine.sessions.NewSession("telegram:chat-1:user-1", "telegram").SetAgentSessionID("thread-telegram", "codex")
		if !engine.markPlatformReady(platform) {
			t.Fatal("failed to mark platform ready")
		}

		engine.pollDesktopLiveSync(context.Background(), agent)

		if len(agent.routeSets) != 1 || len(agent.routeSets[0]) != 1 || agent.routeSets[0][0] != "thread-feishu" {
			t.Fatalf("tracked routes = %#v, want only thread-feishu", agent.routeSets)
		}
		if len(agent.pollCalls) != 1 || agent.pollCalls[0] != "thread-feishu" {
			t.Fatalf("poll calls = %#v, want only thread-feishu", agent.pollCalls)
		}
	})
}
