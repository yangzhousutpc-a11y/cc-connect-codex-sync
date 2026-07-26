package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

type desktopNameSyncAgent struct {
	*desktopSyncAgent
	name      string
	startMu   sync.Mutex
	startIDs  []string
	startFunc func(context.Context, string) (AgentSession, error)
}

func (a *desktopNameSyncAgent) Name() string { return a.name }

func (a *desktopNameSyncAgent) StartSession(ctx context.Context, sessionID string) (AgentSession, error) {
	a.startMu.Lock()
	a.startIDs = append(a.startIDs, sessionID)
	a.startMu.Unlock()
	if a.startFunc == nil {
		return nil, errors.New("unexpected desktop name restore")
	}
	return a.startFunc(ctx, sessionID)
}

func (a *desktopNameSyncAgent) setEvents(sessionID string, events []ExternalConversationEvent) {
	a.mu.Lock()
	a.events[sessionID] = events
	a.mu.Unlock()
}

func (a *desktopNameSyncAgent) startedSessionIDs() []string {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	return append([]string(nil), a.startIDs...)
}

type desktopNameReassertSession struct {
	*controllableAgentSession
	mu              sync.Mutex
	reassertedNames []string
	failures        int
	onReassert      func()
	started         chan struct{}
	release         <-chan struct{}
}

type desktopNameTOCTOUSession struct {
	*controllableAgentSession
	mu          sync.Mutex
	oldName     string
	oldStarted  bool
	started     chan struct{}
	releaseOld  <-chan struct{}
	appliedName []string
}

func (s *desktopNameTOCTOUSession) ReassertSessionName(name string) error {
	s.mu.Lock()
	blockOld := name == s.oldName && !s.oldStarted
	if blockOld {
		s.oldStarted = true
	}
	s.mu.Unlock()
	if blockOld {
		select {
		case s.started <- struct{}{}:
		default:
		}
		<-s.releaseOld
	}
	s.mu.Lock()
	s.appliedName = append(s.appliedName, name)
	s.mu.Unlock()
	return nil
}

func (s *desktopNameTOCTOUSession) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.appliedName...)
}

func (s *desktopNameReassertSession) ReassertSessionName(name string) error {
	s.mu.Lock()
	s.reassertedNames = append(s.reassertedNames, name)
	fail := s.failures > 0
	if fail {
		s.failures--
	}
	s.mu.Unlock()
	if s.onReassert != nil {
		s.onReassert()
	}
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.release != nil {
		<-s.release
	}
	if fail {
		return errors.New("transient desktop name reassert failure")
	}
	return nil
}

func (s *desktopNameReassertSession) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reassertedNames...)
}

func (s *desktopNameReassertSession) setFailures(failures int) {
	s.mu.Lock()
	s.failures = failures
	s.mu.Unlock()
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

func TestDesktopLiveSyncNameWaitsForCompletionAndReassertsOnce(t *testing.T) {
	const (
		sessionID  = "thread-name-complete"
		sessionKey = "wecom:g:name-complete"
		customName = "[企业微信-Codex] 名称重申群"
	)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	live := &desktopNameReassertSession{controllableAgentSession: newControllableSession(sessionID)}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
		agent:        agent,
	}
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	agent.setEvents(sessionID, []ExternalConversationEvent{{
		SessionID: sessionID,
		Role:      "user",
		Content:   "App 用户消息",
	}})
	engine.pollDesktopLiveSync(context.Background(), agent)
	if got := live.names(); len(got) != 0 {
		t.Fatalf("user phase reassertions = %#v, want none before completion", got)
	}

	agent.setEvents(sessionID, []ExternalConversationEvent{{
		SessionID:     sessionID,
		Role:          "assistant",
		Content:       "App 助手回复",
		TurnCompleted: true,
	}})
	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()

	if got, want := live.names(), []string{customName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("completion reassertions = %#v, want %#v", got, want)
	}
	if got := agent.startedSessionIDs(); len(got) != 0 {
		t.Fatalf("live route unexpectedly restored sessions: %#v", got)
	}
	platform.mu.Lock()
	delivered := append([]string(nil), platform.sent...)
	platform.mu.Unlock()
	if got, want := delivered, []string{"Codex App · 你\nApp 用户消息", "Codex · 回复\nApp 助手回复"}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("delivered messages = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncNameReassertsForEmptyCompletionWithoutEmptyRelay(t *testing.T) {
	const (
		sessionID  = "thread-name-cancelled"
		sessionKey = "wecom:g:name-cancelled"
		customName = "[企业微信-Codex] 取消回合群"
	)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			sessionID: {{SessionID: sessionID, Role: "assistant", TurnCompleted: true}},
		}},
		name: "desktop-name-sync",
	}
	live := &desktopNameReassertSession{controllableAgentSession: newControllableSession(sessionID)}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
		agent:        agent,
	}
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()

	if got, want := live.names(), []string{customName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("empty completion reassertions = %#v, want %#v", got, want)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.sent) != 0 {
		t.Fatalf("empty completion relayed messages = %#v, want none", platform.sent)
	}
}

func TestDesktopLiveSyncNameCompletionSurvivesLaterMessageRetry(t *testing.T) {
	const (
		sessionID  = "thread-name-completion-retry"
		sessionKey = "wecom:g:name-completion-retry"
		customName = "[企业微信-Codex] 完成态持久群"
	)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			sessionID: {
				{SessionID: sessionID, Role: "assistant", TurnCompleted: true},
				{SessionID: sessionID, Role: "user", Content: "完成态之后的消息"},
			},
		}},
		name: "desktop-name-sync",
	}
	live := &desktopNameReassertSession{controllableAgentSession: newControllableSession(sessionID)}
	platform := &desktopSyncFlakyPlatform{
		desktopSyncPlatform: desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}},
		failures:            1,
	}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
		agent:        agent,
	}
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	if got := live.names(); len(got) != 0 {
		t.Fatalf("reassertions before the later message succeeds = %#v, want none", got)
	}

	agent.setEvents(sessionID, nil)
	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()

	if got, want := live.names(), []string{customName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("completion reassertions after retry = %#v, want %#v", got, want)
	}
	platform.mu.Lock()
	delivered := append([]string(nil), platform.sent...)
	platform.mu.Unlock()
	if got, want := delivered, []string{"Codex App · 你\n完成态之后的消息"}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("delivered messages = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncNameRestoresOriginalSessionIDWithoutLiveState(t *testing.T) {
	const (
		sessionID  = "thread-name-restore"
		sessionKey = "wecom:g:name-restore"
		customName = "[企业微信-Codex] 恢复原会话群"
	)
	restored := &desktopNameReassertSession{controllableAgentSession: newControllableSession(sessionID)}
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			sessionID: {{SessionID: sessionID, Role: "assistant", TurnCompleted: true}},
		}},
		name: "desktop-name-sync",
		startFunc: func(_ context.Context, targetID string) (AgentSession, error) {
			if targetID != sessionID {
				t.Fatalf("restore target = %q, want original %q", targetID, sessionID)
			}
			return restored, nil
		},
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()

	if got, want := agent.startedSessionIDs(), []string{sessionID}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("restored session IDs = %#v, want %#v", got, want)
	}
	if got, want := restored.names(), []string{customName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("restored reassertions = %#v, want %#v", got, want)
	}
	select {
	case <-restored.closed:
	default:
		t.Fatal("temporary restored session was not closed")
	}
	if got := session.GetAgentSessionID(); got != sessionID {
		t.Fatalf("route rebound to %q, want unchanged %q", got, sessionID)
	}
}

func TestDesktopLiveSyncNameMismatchedLiveStateRestoresOriginalSessionID(t *testing.T) {
	const (
		sessionID  = "thread-name-original"
		sessionKey = "wecom:g:name-mismatched-live"
		customName = "[企业微信-Codex] 原会话群"
	)
	mismatched := &desktopNameReassertSession{controllableAgentSession: newControllableSession("thread-other")}
	restored := &desktopNameReassertSession{controllableAgentSession: newControllableSession(sessionID)}
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
		startFunc: func(_ context.Context, targetID string) (AgentSession, error) {
			if targetID != sessionID {
				t.Fatalf("restore target = %q, want original %q", targetID, sessionID)
			}
			return restored, nil
		},
	}
	engine := NewEngine("test", agent, nil, "", LangChinese)
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: mismatched,
		agent:        agent,
	}
	t.Cleanup(func() { _ = engine.Stop() })

	if err := engine.reassertDesktopSessionName(agent, sessionID, sessionKey, customName); err != nil {
		t.Fatalf("reassert mismatched live state: %v", err)
	}

	if got := mismatched.names(); len(got) != 0 {
		t.Fatalf("mismatched live session was borrowed: %#v", got)
	}
	if got, want := agent.startedSessionIDs(), []string{sessionID}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("restored session IDs = %#v, want %#v", got, want)
	}
	if got, want := restored.names(), []string{customName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("restored reassertions = %#v, want %#v", got, want)
	}
	select {
	case <-restored.closed:
	default:
		t.Fatal("temporary restored session was not closed")
	}
}

func TestDesktopLiveSyncNameFailureRetriesWithoutNewEventsOrMessageReplay(t *testing.T) {
	const (
		sessionID  = "thread-name-retry"
		sessionKey = "wecom:g:name-retry"
		customName = "[企业微信-Codex] 重试群"
	)
	now := time.Date(2026, 7, 26, 15, 0, 0, 0, time.Local)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			sessionID: {
				{SessionID: sessionID, Role: "user", Content: "一次用户消息"},
				{SessionID: sessionID, Role: "assistant", Content: "一次助手回复", TurnCompleted: true},
			},
		}},
		name: "desktop-name-sync",
	}
	live := &desktopNameReassertSession{
		controllableAgentSession: newControllableSession(sessionID),
		failures:                 1,
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.desktopNameReassertNow = func() time.Time { return now }
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
		agent:        agent,
	}
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()

	if got, want := live.names(), []string{customName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("initial reassertions = %#v, want %#v", got, want)
	}
	engine.desktopSyncMu.Lock()
	if got := len(engine.desktopNameReassertPending); got != 1 {
		engine.desktopSyncMu.Unlock()
		t.Fatalf("name pending after failure = %d, want 1", got)
	}
	if got := len(engine.desktopSyncPending); got != 0 {
		engine.desktopSyncMu.Unlock()
		t.Fatalf("message pending after name failure = %d, want 0", got)
	}
	var retryAt time.Time
	for _, state := range engine.desktopNameReassertPending {
		if state.attempts != 1 {
			engine.desktopSyncMu.Unlock()
			t.Fatalf("name attempts after failure = %d, want 1", state.attempts)
		}
		retryAt = state.nextAttempt
	}
	engine.desktopSyncMu.Unlock()

	platform.mu.Lock()
	deliveredBeforeRetry := append([]string(nil), platform.sent...)
	platform.mu.Unlock()
	wantDelivered := []string{"Codex App · 你\n一次用户消息", "Codex · 回复\n一次助手回复"}
	if !equalDesktopSyncStrings(deliveredBeforeRetry, wantDelivered) {
		t.Fatalf("initial delivery = %#v, want %#v", deliveredBeforeRetry, wantDelivered)
	}

	live.setFailures(0)
	now = retryAt
	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()
	engine.pollDesktopLiveSync(context.Background(), agent)

	if got, want := live.names(), []string{customName, customName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("retry reassertions = %#v, want %#v", got, want)
	}
	engine.desktopSyncMu.Lock()
	pendingAfterSuccess := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()
	if pendingAfterSuccess != 0 {
		t.Fatalf("name pending after success = %d, want 0", pendingAfterSuccess)
	}
	platform.mu.Lock()
	deliveredAfterRetry := append([]string(nil), platform.sent...)
	platform.mu.Unlock()
	if !equalDesktopSyncStrings(deliveredAfterRetry, wantDelivered) {
		t.Fatalf("delivery after name retry = %#v, want no message replay %#v", deliveredAfterRetry, wantDelivered)
	}
}

func TestDesktopLiveSyncNameChangedGenerationSupersedesCollectedRequest(t *testing.T) {
	const (
		sessionID  = "thread-name-generation"
		sessionKey = "wecom:g:name-generation"
		oldName    = "[企业微信-Codex] 旧一代重申群"
		newName    = "[企业微信-Codex] 新一代重申群"
	)
	now := time.Date(2026, 7, 26, 15, 30, 0, 0, time.Local)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	live := &desktopNameReassertSession{controllableAgentSession: newControllableSession(sessionID)}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, oldName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, oldName)
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
		agent:        agent,
	}
	t.Cleanup(func() { _ = engine.Stop() })

	key := desktopNameReassertKey{sessions: engine.sessions, sessionID: sessionID, sessionKey: sessionKey}
	engine.desktopSyncMu.Lock()
	engine.scheduleDesktopNameReassertLocked(key, agent, sessionKey, oldName, now)
	first := engine.collectDueDesktopNameReassertsLocked(now)
	engine.sessions.SetSessionName(sessionID, newName)
	engine.scheduleDesktopNameReassertLocked(key, agent, sessionKey, newName, now)
	engine.desktopSyncMu.Unlock()
	if len(first) != 1 {
		t.Fatalf("first collected requests = %d, want 1", len(first))
	}

	staleResult := engine.executeDesktopNameReassert(first[0])
	if staleResult.attempted {
		t.Fatal("superseded generation executed a stale name RPC")
	}
	engine.desktopSyncMu.Lock()
	engine.commitDesktopNameReassertLocked(staleResult, now)
	second := engine.collectDueDesktopNameReassertsLocked(now)
	engine.desktopSyncMu.Unlock()
	if len(second) != 1 || second[0].generation == first[0].generation {
		t.Fatalf("latest collected requests = %#v, want one newer generation", second)
	}

	result := engine.executeDesktopNameReassert(second[0])
	engine.desktopSyncMu.Lock()
	engine.commitDesktopNameReassertLocked(result, now)
	pending := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()

	if got, want := live.names(), []string{newName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("generation reassertions = %#v, want latest only %#v", got, want)
	}
	if pending != 0 {
		t.Fatalf("name pending after latest success = %d, want 0", pending)
	}
}

func TestDesktopLiveSyncNameSerializesGenerationsForSameRoute(t *testing.T) {
	const (
		sessionID  = "thread-name-serialized"
		sessionKey = "wecom:g:name-serialized"
		oldName    = "[企业微信-Codex] 串行旧名称"
		newName    = "[企业微信-Codex] 串行新名称"
	)
	now := time.Date(2026, 7, 26, 15, 45, 0, 0, time.Local)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	live := &desktopNameReassertSession{
		controllableAgentSession: newControllableSession(sessionID),
		started:                  started,
		release:                  release,
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.desktopNameReassertNow = func() time.Time { return now }
	session := engine.sessions.NewSession(sessionKey, oldName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, oldName)
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		agent:        agent,
	}
	t.Cleanup(func() { _ = engine.Stop() })

	key := desktopNameReassertKey{sessions: engine.sessions, sessionID: sessionID, sessionKey: sessionKey}
	engine.desktopSyncMu.Lock()
	engine.scheduleDesktopNameReassertLocked(key, agent, sessionKey, oldName, now)
	engine.desktopSyncMu.Unlock()
	firstDone := make(chan struct{})
	go func() {
		engine.processDueDesktopNameReasserts()
		close(firstDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("timed out waiting for first name RPC")
	}

	engine.sessions.SetSessionName(sessionID, newName)
	engine.desktopSyncMu.Lock()
	engine.scheduleDesktopNameReassertLocked(key, agent, sessionKey, newName, now.Add(time.Second))
	engine.desktopSyncMu.Unlock()

	secondWorkerDone := make(chan struct{})
	go func() {
		engine.processDueDesktopNameReasserts()
		close(secondWorkerDone)
	}()
	select {
	case <-secondWorkerDone:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("second worker blocked despite the route already having an in-flight RPC")
	}
	select {
	case <-started:
		close(release)
		t.Fatal("a second same-route name RPC started before the first completed")
	default:
	}

	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first name RPC did not finish after release")
	}

	if got, want := live.names(), []string{oldName, newName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("serialized reassertions = %#v, want %#v", got, want)
	}
	engine.desktopSyncMu.Lock()
	pending := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending after latest generation success = %d, want 0", pending)
	}
}

func TestDesktopLiveSyncNameWorkerClaimsOnlyOneDueRouteWhileRPCBlocks(t *testing.T) {
	const (
		slowSessionID  = "thread-name-worker-slow"
		slowSessionKey = "wecom:g:name-worker-slow"
		slowName       = "[企业微信-Codex] 慢命名群"
		nextSessionID  = "thread-name-worker-next"
		nextSessionKey = "wecom:g:name-worker-next"
		nextName       = "[企业微信-Codex] 后续命名群"
	)
	now := time.Date(2026, 7, 26, 16, 0, 0, 0, time.Local)
	runAt := now.Add(2 * time.Second)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.desktopNameReassertNow = func() time.Time { return runAt }
	slowRoute := engine.sessions.NewSession(slowSessionKey, slowName)
	slowRoute.SetAgentSessionID(slowSessionID, agent.Name())
	engine.sessions.SetSessionName(slowSessionID, slowName)
	nextRoute := engine.sessions.NewSession(nextSessionKey, nextName)
	nextRoute.SetAgentSessionID(nextSessionID, agent.Name())
	engine.sessions.SetSessionName(nextSessionID, nextName)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	slowLive := &desktopNameReassertSession{
		controllableAgentSession: newControllableSession(slowSessionID),
		started:                  started,
		release:                  release,
	}
	nextLive := &desktopNameReassertSession{controllableAgentSession: newControllableSession(nextSessionID)}
	engine.interactiveStates[slowSessionKey] = &interactiveState{agentSession: slowLive, agent: agent}
	engine.interactiveStates[nextSessionKey] = &interactiveState{agentSession: nextLive, agent: agent}
	t.Cleanup(func() { _ = engine.Stop() })

	slowKey := desktopNameReassertKey{
		sessions:   engine.sessions,
		sessionID:  slowSessionID,
		sessionKey: slowSessionKey,
	}
	nextKey := desktopNameReassertKey{
		sessions:   engine.sessions,
		sessionID:  nextSessionID,
		sessionKey: nextSessionKey,
	}
	engine.desktopSyncMu.Lock()
	engine.scheduleDesktopNameReassertLocked(slowKey, agent, slowSessionKey, slowName, now)
	engine.scheduleDesktopNameReassertLocked(nextKey, agent, nextSessionKey, nextName, now.Add(time.Second))
	engine.desktopSyncMu.Unlock()

	workerDone := make(chan struct{})
	go func() {
		engine.processDueDesktopNameReasserts()
		close(workerDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("timed out waiting for deterministic first name RPC")
	}

	engine.desktopSyncMu.Lock()
	nextState, nextPending := engine.desktopNameReassertPending[nextKey]
	nextInFlight := nextPending && nextState.inFlight
	engine.desktopSyncMu.Unlock()
	nextRanEarly := len(nextLive.names()) > 0
	close(release)
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("name worker did not finish after releasing the first RPC")
	}

	if nextInFlight {
		t.Fatal("later due route was marked in flight before the worker could execute it")
	}
	if nextRanEarly {
		t.Fatal("later due route executed before the deterministically earlier slow route completed")
	}
	if got, want := nextLive.names(), []string{nextName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("later route reassertions after release = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncNameCompensatesWhenUserRenameWinsValidationRace(t *testing.T) {
	const (
		sessionID  = "thread-name-user-race"
		sessionKey = "wecom:g:name-user-race"
		oldName    = "[企业微信-Codex] 旧后台名称"
		newName    = "[企业微信-Codex] 用户新名称"
	)
	now := time.Date(2026, 7, 26, 16, 5, 0, 0, time.Local)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.desktopNameReassertNow = func() time.Time { return now }
	session := engine.sessions.NewSession(sessionKey, oldName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, oldName)
	started := make(chan struct{}, 1)
	releaseOld := make(chan struct{})
	live := &desktopNameTOCTOUSession{
		controllableAgentSession: newControllableSession(sessionID),
		oldName:                  oldName,
		started:                  started,
		releaseOld:               releaseOld,
	}
	engine.interactiveStates[sessionKey] = &interactiveState{agentSession: live, agent: agent}
	t.Cleanup(func() { _ = engine.Stop() })

	key := desktopNameReassertKey{
		sessions:   engine.sessions,
		sessionID:  sessionID,
		sessionKey: sessionKey,
	}
	engine.desktopSyncMu.Lock()
	engine.scheduleDesktopNameReassertLocked(key, agent, sessionKey, oldName, now)
	engine.desktopSyncMu.Unlock()
	workerDone := make(chan struct{})
	go func() {
		engine.processDueDesktopNameReasserts()
		close(workerDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(releaseOld)
		t.Fatal("timed out waiting for old background name RPC")
	}

	engine.sessions.SetSessionName(sessionID, newName)
	if err := forceSyncRequiredAgentSessionName(live, newName); err != nil {
		close(releaseOld)
		t.Fatalf("apply user rename: %v", err)
	}
	close(releaseOld)
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("name worker did not finish after releasing the old RPC")
	}

	if got, want := live.names(), []string{newName, oldName, newName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("remote name writes = %#v, want user write, stale overwrite, compensation %#v", got, want)
	}
	engine.desktopSyncMu.Lock()
	pending := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()
	if pending != 0 {
		t.Fatalf("name pending after compensation = %d, want 0", pending)
	}
}

func TestDesktopLiveSyncNameDuplicateCompletionPreservesRetryAgeAndBackoff(t *testing.T) {
	const (
		sessionID  = "thread-name-duplicate"
		sessionKey = "wecom:g:name-duplicate"
		customName = "[企业微信-Codex] 重复完成态群"
	)
	now := time.Date(2026, 7, 26, 15, 50, 0, 0, time.Local)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	clock := now
	engine.desktopNameReassertNow = func() time.Time { return clock }
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	key := desktopNameReassertKey{sessions: engine.sessions, sessionID: sessionID, sessionKey: sessionKey}
	engine.desktopSyncMu.Lock()
	engine.scheduleDesktopNameReassertLocked(key, agent, sessionKey, customName, now)
	requests := engine.collectDueDesktopNameReassertsLocked(now)
	if len(requests) != 1 {
		engine.desktopSyncMu.Unlock()
		t.Fatalf("initial requests = %d, want 1", len(requests))
	}
	engine.commitDesktopNameReassertLocked(
		desktopNameReassertResult{
			request:   requests[0],
			attempted: true,
			err:       errors.New("transient"),
		},
		now,
	)
	before := engine.desktopNameReassertPending[key]
	engine.desktopSyncMu.Unlock()

	clock = now.Add(100 * time.Millisecond)
	agent.setEvents(sessionID, []ExternalConversationEvent{{
		SessionID:     sessionID,
		Role:          "assistant",
		TurnCompleted: true,
	}})
	engine.pollDesktopLiveSync(context.Background(), agent)

	engine.desktopSyncMu.Lock()
	after := engine.desktopNameReassertPending[key]
	early := engine.collectDueDesktopNameReassertsLocked(clock)
	engine.desktopSyncMu.Unlock()

	if before.generation != after.generation ||
		before.attempts != after.attempts ||
		!before.createdAt.Equal(after.createdAt) ||
		!before.nextAttempt.Equal(after.nextAttempt) {
		t.Fatalf("duplicate completion reset retry state: before=%+v after=%+v", before, after)
	}
	if len(early) != 0 {
		t.Fatalf("duplicate completion bypassed backoff with %d early request(s)", len(early))
	}
}

func TestDesktopLiveSyncNameRetryStateHasCapacityAndTTLBounds(t *testing.T) {
	now := time.Date(2026, 7, 26, 16, 0, 0, 0, time.Local)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	engine := NewEngine("test", agent, nil, "", LangChinese)
	t.Cleanup(func() { _ = engine.Stop() })

	engine.desktopSyncMu.Lock()
	for i := 0; i < desktopNameReassertPendingMaximum+10; i++ {
		sessionID := fmt.Sprintf("thread-capacity-%d", i)
		sessionKey := fmt.Sprintf("wecom:g:capacity-%d", i)
		engine.scheduleDesktopNameReassertLocked(
			desktopNameReassertKey{
				sessions:   engine.sessions,
				sessionID:  sessionID,
				sessionKey: sessionKey,
			},
			agent,
			sessionKey,
			"[企业微信-Codex] 容量保护群",
			now.Add(time.Duration(i)*time.Nanosecond),
		)
	}
	if got := len(engine.desktopNameReassertPending); got != desktopNameReassertPendingMaximum {
		engine.desktopSyncMu.Unlock()
		t.Fatalf("name retry state size = %d, want bounded at %d", got, desktopNameReassertPendingMaximum)
	}
	engine.pruneExpiredDesktopNameReassertsLocked(
		now.Add(time.Second + desktopNameReassertPendingTTL),
	)
	remaining := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()

	if remaining != 0 {
		t.Fatalf("name retry state after TTL = %d, want 0", remaining)
	}
}

func TestDesktopLiveSyncCompletionStateHasCapacityAndTTLBounds(t *testing.T) {
	now := time.Date(2026, 7, 26, 16, 5, 0, 0, time.Local)
	engine := NewEngine("test", &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)}, nil, "", LangChinese)
	t.Cleanup(func() { _ = engine.Stop() })

	engine.desktopSyncMu.Lock()
	duplicateKey := desktopSyncPendingKey{
		sessions:   engine.sessions,
		sessionID:  "thread-completion-duplicate",
		sessionKey: "wecom:g:completion-duplicate",
	}
	engine.markDesktopSyncCompletionLocked(duplicateKey, now)
	engine.markDesktopSyncCompletionLocked(duplicateKey, now.Add(time.Minute))
	if got := engine.desktopSyncCompletionPending[duplicateKey].createdAt; !got.Equal(now) {
		engine.desktopSyncMu.Unlock()
		t.Fatalf("duplicate completion createdAt = %v, want original %v", got, now)
	}
	delete(engine.desktopSyncCompletionPending, duplicateKey)

	for i := 0; i < desktopSyncCompletionPendingMax+10; i++ {
		engine.markDesktopSyncCompletionLocked(
			desktopSyncPendingKey{
				sessions:   engine.sessions,
				sessionID:  fmt.Sprintf("thread-completion-capacity-%d", i),
				sessionKey: fmt.Sprintf("wecom:g:completion-capacity-%d", i),
			},
			now.Add(time.Duration(i)*time.Nanosecond),
		)
	}
	if got := len(engine.desktopSyncCompletionPending); got != desktopSyncCompletionPendingMax {
		engine.desktopSyncMu.Unlock()
		t.Fatalf("completion state size = %d, want bounded at %d", got, desktopSyncCompletionPendingMax)
	}
	engine.pruneExpiredDesktopSyncCompletionsLocked(now.Add(time.Second + desktopSyncCompletionPendingTTL))
	remaining := len(engine.desktopSyncCompletionPending)
	engine.desktopSyncMu.Unlock()

	if remaining != 0 {
		t.Fatalf("completion state after TTL = %d, want 0", remaining)
	}
}

func TestDesktopLiveSyncCompletionWaitsUntilNameQueueCanAcceptIt(t *testing.T) {
	const (
		sessionID  = "thread-completion-capacity-wait"
		sessionKey = "wecom:g:completion-capacity-wait"
		customName = "[企业微信-Codex] 等待命名队列群"
	)
	now := time.Date(2026, 7, 26, 16, 10, 0, 0, time.Local)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	engine := NewEngine("test", agent, nil, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	t.Cleanup(func() { _ = engine.Stop() })

	pendingKey := desktopSyncPendingKey{
		sessions:   engine.sessions,
		sessionID:  sessionID,
		sessionKey: sessionKey,
	}
	engine.desktopSyncMu.Lock()
	engine.desktopNameReassertPending = make(map[desktopNameReassertKey]desktopNameReassertState)
	var releasable desktopNameReassertKey
	for i := 0; i < desktopNameReassertPendingMaximum; i++ {
		key := desktopNameReassertKey{
			sessions:   engine.sessions,
			sessionID:  fmt.Sprintf("thread-in-flight-capacity-%d", i),
			sessionKey: fmt.Sprintf("wecom:g:in-flight-capacity-%d", i),
		}
		if i == 0 {
			releasable = key
		}
		engine.desktopNameReassertPending[key] = desktopNameReassertState{
			agent:       agent,
			name:        customName,
			generation:  uint64(i + 1),
			createdAt:   now,
			nextAttempt: now,
			inFlight:    true,
			inFlightGen: uint64(i + 1),
		}
	}
	engine.markDesktopSyncCompletionLocked(pendingKey, now)
	engine.scheduleCompletedDesktopNameReassertLocked(pendingKey, agent, nil)
	if _, waiting := engine.desktopSyncCompletionPending[pendingKey]; !waiting {
		engine.desktopSyncMu.Unlock()
		t.Fatal("completion was dropped while every bounded name slot was in flight")
	}

	state := engine.desktopNameReassertPending[releasable]
	state.inFlight = false
	state.inFlightGen = 0
	engine.desktopNameReassertPending[releasable] = state
	engine.scheduleCompletedDesktopNameReassertLocked(pendingKey, agent, nil)
	_, waiting := engine.desktopSyncCompletionPending[pendingKey]
	_, scheduled := engine.desktopNameReassertPending[desktopNameReassertKey{
		sessions:   engine.sessions,
		sessionID:  sessionID,
		sessionKey: sessionKey,
	}]
	engine.desktopSyncMu.Unlock()

	if waiting {
		t.Fatal("completion remained waiting after a name queue slot became available")
	}
	if !scheduled {
		t.Fatal("completion was not scheduled after a name queue slot became available")
	}
}

func TestDesktopLiveSyncUnsupportedNameIsPermanentAndClosesRestore(t *testing.T) {
	const (
		sessionID  = "thread-name-unsupported"
		sessionKey = "wecom:g:name-unsupported"
		customName = "[企业微信-Codex] 不支持命名群"
	)
	restored := newControllableSession(sessionID)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			sessionID: {{SessionID: sessionID, Role: "assistant", TurnCompleted: true}},
		}},
		name: "desktop-name-sync",
		startFunc: func(context.Context, string) (AgentSession, error) {
			return restored, nil
		},
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	if err := syncRequiredAgentSessionName(restored, customName); !errors.Is(err, ErrAgentSessionNameUnsupported) {
		t.Fatalf("unsupported error = %v, want ErrAgentSessionNameUnsupported", err)
	}
	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()
	engine.processDueDesktopNameReasserts()
	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()

	if got, want := agent.startedSessionIDs(), []string{sessionID}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("restore attempts = %#v, want one permanent attempt %#v", got, want)
	}
	select {
	case <-restored.closed:
	default:
		t.Fatal("unsupported temporary restore session was not closed")
	}
	engine.desktopSyncMu.Lock()
	pending := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()
	if pending != 0 {
		t.Fatalf("unsupported name pending = %d, want dropped", pending)
	}
}

func TestDesktopLiveSyncNameRetryBackoffIsExponentialAndBounded(t *testing.T) {
	previous := time.Duration(0)
	reachedMaximum := false
	for attempts := 1; attempts <= 100; attempts++ {
		delay := desktopNameReassertRetryDelay(attempts)
		if delay < previous {
			t.Fatalf("retry delay decreased at attempt %d: %v after %v", attempts, delay, previous)
		}
		if delay > 30*time.Second {
			t.Fatalf("retry delay exceeded 30s bound at attempt %d: %v", attempts, delay)
		}
		want := previous * 2
		if want > 30*time.Second {
			want = 30 * time.Second
		}
		if previous > 0 && previous < 30*time.Second && delay != want {
			t.Fatalf("retry delay at attempt %d = %v, want %v", attempts, delay, want)
		}
		if delay == 30*time.Second {
			reachedMaximum = true
		}
		previous = delay
	}
	if !reachedMaximum {
		t.Fatal("retry delay never reached its upper bound")
	}
}

func TestDesktopLiveSyncNameRouteChangeBeforeExecutionFailsClosed(t *testing.T) {
	const (
		sessionID    = "thread-name-stale"
		replacement  = "thread-name-replacement"
		sessionKey   = "wecom:g:name-stale"
		customName   = "[企业微信-Codex] 旧路由群"
		replacedName = "[企业微信-Codex] 新路由群"
	)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			sessionID: {{SessionID: sessionID, Role: "assistant", TurnCompleted: true}},
		}},
		name: "desktop-name-sync",
	}
	live := &desktopNameReassertSession{controllableAgentSession: newControllableSession(sessionID)}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
		agent:        agent,
	}
	engine.desktopNameReassertExecute = func(request desktopNameReassertRequest) desktopNameReassertResult {
		next := engine.sessions.NewSession(sessionKey, replacedName)
		next.SetAgentSessionID(replacement, agent.Name())
		engine.sessions.SetSessionName(replacement, replacedName)
		return engine.executeDesktopNameReassert(request)
	}
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()

	if got := live.names(); len(got) != 0 {
		t.Fatalf("stale route reassertions = %#v, want none", got)
	}
	if got := agent.startedSessionIDs(); len(got) != 0 {
		t.Fatalf("stale route restores = %#v, want none", got)
	}
	if got := engine.sessions.GetOrCreateActive(sessionKey).GetAgentSessionID(); got != replacement {
		t.Fatalf("active route = %q, want replacement %q", got, replacement)
	}
	engine.desktopSyncMu.Lock()
	pending := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()
	if pending != 0 {
		t.Fatalf("stale route name pending = %d, want 0", pending)
	}
}

func TestDesktopLiveSyncNameRouteChangeDuringExecutionDropsFailedRetry(t *testing.T) {
	const (
		sessionID   = "thread-name-commit-stale"
		replacement = "thread-name-commit-replacement"
		sessionKey  = "wecom:g:name-commit-stale"
		customName  = "[企业微信-Codex] 提交前换路由群"
	)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			sessionID: {{SessionID: sessionID, Role: "assistant", TurnCompleted: true}},
		}},
		name: "desktop-name-sync",
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	session := engine.sessions.NewSession(sessionKey, customName)
	session.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	live := &desktopNameReassertSession{
		controllableAgentSession: newControllableSession(sessionID),
		failures:                 1,
		onReassert: func() {
			next := engine.sessions.NewSession(sessionKey, "replacement")
			next.SetAgentSessionID(replacement, agent.Name())
		},
	}
	engine.interactiveStates[sessionKey] = &interactiveState{
		agentSession: live,
		platform:     platform,
		replyCtx:     "ctx",
		agent:        agent,
	}
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.processDueDesktopNameReasserts()

	if got, want := live.names(), []string{customName}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("in-flight reassertions = %#v, want %#v", got, want)
	}
	engine.desktopSyncMu.Lock()
	pending := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()
	if pending != 0 {
		t.Fatalf("route-changed failed reassert pending = %d, want dropped", pending)
	}
}

func TestDesktopLiveSyncNameRoutePruneClearsMessageAndNamePending(t *testing.T) {
	const (
		sessionID   = "thread-name-prune"
		replacement = "thread-name-prune-replacement"
		sessionKey  = "wecom:g:name-prune"
		customName  = "[企业微信-Codex] 路由裁剪群"
	)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: make(map[string][]ExternalConversationEvent)},
		name:             "desktop-name-sync",
	}
	platform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	old := engine.sessions.NewSession(sessionKey, customName)
	old.SetAgentSessionID(sessionID, agent.Name())
	engine.sessions.SetSessionName(sessionID, customName)
	key := desktopSyncPendingKey{
		sessions:   engine.sessions,
		sessionID:  sessionID,
		sessionKey: sessionKey,
	}
	engine.desktopSyncMu.Lock()
	engine.desktopSyncPending = map[desktopSyncPendingKey][]ExternalConversationEvent{
		key: {{SessionID: sessionID, Role: "assistant", Content: "stale"}},
	}
	engine.desktopSyncCompletionPending = map[desktopSyncPendingKey]desktopSyncCompletionState{
		key: {createdAt: time.Now()},
	}
	engine.scheduleDesktopNameReassertLocked(
		desktopNameReassertKey{
			sessions:   engine.sessions,
			sessionID:  sessionID,
			sessionKey: sessionKey,
		},
		agent,
		sessionKey,
		customName,
		time.Now(),
	)
	engine.desktopSyncMu.Unlock()
	next := engine.sessions.NewSession(sessionKey, "replacement")
	next.SetAgentSessionID(replacement, agent.Name())
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)

	engine.desktopSyncMu.Lock()
	messagePending := len(engine.desktopSyncPending)
	completionPending := len(engine.desktopSyncCompletionPending)
	namePending := len(engine.desktopNameReassertPending)
	engine.desktopSyncMu.Unlock()
	if messagePending != 0 || completionPending != 0 || namePending != 0 {
		t.Fatalf(
			"pending after route prune: message=%d completion=%d name=%d, want all 0",
			messagePending,
			completionPending,
			namePending,
		)
	}
}

func TestDesktopLiveSyncRunLoopAdvancesOtherPlatformWhileNameRPCBlocks(t *testing.T) {
	const (
		nameSessionID  = "thread-name-slow"
		nameSessionKey = "wecom:g:name-slow"
		messageID      = "thread-message-progress"
		messageKey     = "feishu:message-progress:user"
		customName     = "[企业微信-Codex] 慢重申群"
	)
	agent := &desktopNameSyncAgent{
		desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
			nameSessionID: {{
				SessionID:     nameSessionID,
				Role:          "assistant",
				TurnCompleted: true,
			}},
		}},
		name: "desktop-name-sync",
	}
	namePlatform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	messagePlatform := &desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	engine := NewEngine("test", agent, []Platform{namePlatform, messagePlatform}, "", LangChinese)
	nameSession := engine.sessions.NewSession(nameSessionKey, customName)
	nameSession.SetAgentSessionID(nameSessionID, agent.Name())
	engine.sessions.SetSessionName(nameSessionID, customName)
	messageSession := engine.sessions.NewSession(messageKey, "message route")
	messageSession.SetAgentSessionID(messageID, agent.Name())
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	live := &desktopNameReassertSession{
		controllableAgentSession: newControllableSession(nameSessionID),
		started:                  started,
		release:                  release,
	}
	engine.interactiveStates[nameSessionKey] = &interactiveState{
		agentSession: live,
		platform:     namePlatform,
		replyCtx:     "ctx-name",
		agent:        agent,
	}
	if err := engine.Start(); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		_ = engine.Stop()
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for slow name RPC")
	}

	agent.setEvents(messageID, []ExternalConversationEvent{{
		SessionID: messageID,
		Role:      "user",
		Content:   "另一平台继续推进",
	}})

	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		messagePlatform.mu.Lock()
		delivered := len(messagePlatform.sent)
		messagePlatform.mu.Unlock()
		if delivered > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	messagePlatform.mu.Lock()
	delivered := append([]string(nil), messagePlatform.sent...)
	messagePlatform.mu.Unlock()
	if got, want := delivered, []string{"Codex App · 你\n另一平台继续推进"}; !equalDesktopSyncStrings(got, want) {
		t.Fatalf("other platform delivery = %#v, want %#v", got, want)
	}
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
