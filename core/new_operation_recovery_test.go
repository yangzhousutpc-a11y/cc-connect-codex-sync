package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recoverableControllableAgent struct {
	*controllableAgent
	mu           sync.Mutex
	recoveryKeys []string
	sessionIDs   []string
}

type blockingRecoverableAgent struct {
	*controllableAgent
	mu       sync.Mutex
	calls    int
	started  chan struct{}
	release  <-chan struct{}
	recovery []string
}

func (a *blockingRecoverableAgent) StartRecoverableSession(ctx context.Context, sessionID, recoveryKey string) (AgentSession, error) {
	a.mu.Lock()
	a.calls++
	a.recovery = append(a.recovery, recoveryKey)
	call := a.calls
	a.mu.Unlock()
	if call == 1 {
		close(a.started)
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return a.StartSession(ctx, sessionID)
}

func (a *blockingRecoverableAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *blockingRecoverableAgent) recoveryKeys() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.recovery...)
}

type blockingNamingSession struct {
	id      string
	events  chan Event
	started chan struct{}
	release <-chan struct{}
}

func (s *blockingNamingSession) Send(string, []ImageAttachment, []FileAttachment) error { return nil }
func (s *blockingNamingSession) RespondPermission(string, PermissionResult) error       { return nil }
func (s *blockingNamingSession) Events() <-chan Event                                   { return s.events }
func (s *blockingNamingSession) CurrentSessionID() string                               { return s.id }
func (s *blockingNamingSession) Alive() bool                                            { return true }
func (s *blockingNamingSession) Close() error                                           { return nil }
func (s *blockingNamingSession) SetSessionName(string) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}

func (a *recoverableControllableAgent) StartRecoverableSession(ctx context.Context, sessionID, recoveryKey string) (AgentSession, error) {
	a.mu.Lock()
	a.recoveryKeys = append(a.recoveryKeys, recoveryKey)
	a.sessionIDs = append(a.sessionIDs, sessionID)
	a.mu.Unlock()
	return a.StartSession(ctx, sessionID)
}

type reconstructingSpawningPlatform struct {
	*spawningPlatform
}

func (p *reconstructingSpawningPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "reconstructed:" + sessionKey, nil
}

type lockedRetrySpawningPlatform struct {
	*stubPlatformEngine
	mu       sync.Mutex
	spawned  SpawnedConversation
	spawnErr error
	requests []ConversationSpawnRequest
}

type cancellationAwareSpawningPlatform struct {
	*stubPlatformEngine
	mu      sync.Mutex
	calls   int
	started chan struct{}
	spawned SpawnedConversation
}

type blockingRecoverySpawningPlatform struct {
	*stubPlatformEngine
	started chan struct{}
	release chan struct{}
	spawned SpawnedConversation
}

func (p *blockingRecoverySpawningPlatform) SpawnConversation(context.Context, ConversationSpawnRequest) (SpawnedConversation, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-p.release
	return p.spawned, nil
}

func (p *blockingRecoverySpawningPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "reconstructed:" + sessionKey, nil
}

type cancellationAwareRecoverableAgent struct {
	*controllableAgent
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (a *cancellationAwareRecoverableAgent) StartRecoverableSession(ctx context.Context, _, _ string) (AgentSession, error) {
	a.mu.Lock()
	a.calls++
	call := a.calls
	a.mu.Unlock()
	if call == 1 {
		close(a.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.StartSession(ctx, "")
}

func (a *cancellationAwareRecoverableAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type permanentlyFailingRecoverableAgent struct {
	*controllableAgent
}

func (a *permanentlyFailingRecoverableAgent) StartRecoverableSession(context.Context, string, string) (AgentSession, error) {
	return nil, NewPermanentOperationError(errors.New("recoverable sessions require app_server"))
}

type cancellationAwareNotificationPlatform struct {
	*stubPlatformEngine
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (p *cancellationAwareNotificationPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "reconstructed:" + sessionKey, nil
}

func (p *cancellationAwareNotificationPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.started)
		<-ctx.Done()
		return ctx.Err()
	}
	return p.stubPlatformEngine.Send(ctx, replyCtx, content)
}

func (p *cancellationAwareNotificationPlatform) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *cancellationAwareSpawningPlatform) SpawnConversation(ctx context.Context, _ ConversationSpawnRequest) (SpawnedConversation, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.started)
		<-ctx.Done()
		return SpawnedConversation{}, ctx.Err()
	}
	return p.spawned, nil
}

func (p *cancellationAwareSpawningPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "reconstructed:" + sessionKey, nil
}

func (p *cancellationAwareSpawningPlatform) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *lockedRetrySpawningPlatform) SpawnConversation(_ context.Context, req ConversationSpawnRequest) (SpawnedConversation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	return p.spawned, p.spawnErr
}

func (p *lockedRetrySpawningPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "reconstructed:" + sessionKey, nil
}

func (p *lockedRetrySpawningPlatform) setSpawnError(err error) {
	p.mu.Lock()
	p.spawnErr = err
	p.mu.Unlock()
}

func (p *lockedRetrySpawningPlatform) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (a *recoverableControllableAgent) keys() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.recoveryKeys...)
}

func TestNewOperationRetryDelay_ExponentialAndCapped(t *testing.T) {
	base := 5 * time.Second
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 5 * time.Second},
		{attempt: 2, want: 10 * time.Second},
		{attempt: 3, want: 20 * time.Second},
		{attempt: 20, want: 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := newOperationRetryDelayForAttempt(base, tc.attempt); got != tc.want {
			t.Fatalf("attempt %d delay = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestNewOperationErrorSummary_DoesNotPersistRawSecrets(t *testing.T) {
	err := fmt.Errorf("request failed: Authorization=Bearer secret-token-123")
	if got := newOperationErrorSummary(err); strings.Contains(got, "secret-token-123") {
		t.Fatalf("persisted error contains secret: %q", got)
	}
}

func TestNewOperationRecovery_PermanentFailureRequiresManualAction(t *testing.T) {
	p := &lockedRetrySpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawnErr:           NewPermanentOperationError(errors.New("permission denied")),
	}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-permission")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-permission", UserID: "user-1", Name: "[Codex] 权限失败",
		Step: NewOperationAccepted, Status: NewOperationRunning,
	}); err != nil {
		t.Fatal(err)
	}

	e.OnPlatformReady(p)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationManualAction {
			if p.requestCount() != 1 {
				t.Fatalf("spawn calls = %d, want 1", p.requestCount())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	t.Fatalf("operation did not require manual action: %+v", op)
}

func TestNewOperationRecovery_UnknownFailureStopsAtAttemptLimit(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	seed, _, err := store.CreateOrGet(NewOperation{
		ID: "op-attempt-limit", Platform: "feishu", Step: NewOperationAccepted,
		Status: NewOperationRunning, StepAttempts: newOperationMaxAttempts - 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	e.failNewOperation(seed, errors.New("unknown failure"))
	got, _ := store.Get(seed.ID)
	if got.Status != NewOperationManualAction || got.StepAttempts != newOperationMaxAttempts {
		t.Fatalf("operation at retry limit = %+v", got)
	}
}

func TestNewOperationRecovery_RestartRetriesManualOperationAfterRepair(t *testing.T) {
	p := &lockedRetrySpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned:            SpawnedConversation{SessionKey: "feishu:chat-b:user-1", ReplyCtx: "ctx-b"},
	}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-repaired")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-repaired", UserID: "user-1", Name: "[Codex] 已修复",
		Step: NewOperationAccepted, Status: NewOperationManualAction, StepAttempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	e.OnPlatformReady(p)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	t.Fatalf("repaired operation was not resumed after restart: %+v", op)
}

func TestCmdNew_DurableAgentStartFailureTellsSourceItWillRetry(t *testing.T) {
	p := &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned:            SpawnedConversation{SessionKey: "feishu:chat-b:user-1", ReplyCtx: "ctx-b"},
	}
	agent := &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		return nil, context.DeadlineExceeded
	}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	e.cmdNew(p, &Message{
		Platform: "feishu", SessionKey: "feishu:chat-a:user-1", MessageID: "message-start-fail",
		UserID: "user-1", ChatName: "A群", ReplyCtx: "ctx-a",
	}, []string{"网络恢复"})

	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "自动重试") {
		t.Fatalf("source notification = %q, want automatic retry notice", got)
	}
}

func TestCmdNew_DurablePermanentFailureTellsSourceToCheckConfiguration(t *testing.T) {
	p := &lockedRetrySpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawnErr:           NewPermanentOperationError(errors.New("permission denied")),
	}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	e.cmdNew(p, &Message{
		Platform: "feishu", SessionKey: "feishu:chat-a:user-1", MessageID: "message-permanent",
		UserID: "user-1", ChatName: "A群", ReplyCtx: "ctx-a",
	}, []string{"权限修复"})

	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "权限或配置") {
		t.Fatalf("source notification = %q, want configuration notice", got)
	}
}

func TestAdvanceNewOperation_ResumesAfterConversationSpawned(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned:            SpawnedConversation{SessionKey: "unexpected", ReplyCtx: "unexpected"},
	}}
	agentSession := &primingAgentSession{namingAgentSession: &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	const sourceKey = "feishu:chat-a:user-1"
	source := e.sessions.GetOrCreateActive(sourceKey)
	source.SetAgentSessionID("thread-a", "controllable")
	source.AddHistory("user", "A 群历史")
	sourceHistory := source.GetHistory(0)
	opID := NewOperationID("test", sourceKey, "message-1")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               opID,
		Project:          "test",
		Platform:         "feishu",
		SourceSessionKey: sourceKey,
		SourceMessageID:  "message-1",
		UserID:           "user-1",
		Name:             "[Codex] 恢复测试",
		Step:             NewOperationConversationSpawned,
		Status:           NewOperationRunning,
		TargetSessionKey: "feishu:chat-b:user-1",
		AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, nil, agent, e.sessions)

	if len(p.requests) != 0 {
		t.Fatalf("SpawnConversation calls = %d, want 0", len(p.requests))
	}
	if got := len(e.sessions.ListSessions("feishu:chat-b:user-1")); got != 1 {
		t.Fatalf("target sessions = %d, want 1", got)
	}
	if keys := agent.keys(); !reflect.DeepEqual(keys, []string{"cc-connect/new/" + opID}) {
		t.Fatalf("recovery keys = %#v", keys)
	}
	if !reflect.DeepEqual(agentSession.primedNames, []string{"[Codex] 恢复测试"}) {
		t.Fatalf("visibility primes = %#v, want [[Codex] 恢复测试]", agentSession.primedNames)
	}
	op, _ := store.Get(opID)
	if op.Step != NewOperationCompleted || op.AgentSessionID != "thread-b" {
		t.Fatalf("operation = %+v", op)
	}
	if source.GetAgentSessionID() != "thread-a" || !reflect.DeepEqual(source.GetHistory(0), sourceHistory) {
		t.Fatalf("source session changed: %+v", source)
	}
}

func TestAdvanceNewOperation_ResumesExistingAgentSessionAfterSessionBound(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-existing")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	targetKey := "feishu:chat-b:user-1"
	target := e.sessions.GetOrCreateActive(targetKey)
	target.SetName("[Codex] 恢复测试")
	target.SetAgentSessionID("thread-existing", "codex")
	e.sessions.Save()
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-2")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               opID,
		Project:          "test",
		Platform:         "feishu",
		SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID:  "message-2",
		UserID:           "user-1",
		Name:             "[Codex] 恢复测试",
		Step:             NewOperationSessionBound,
		Status:           NewOperationRunning,
		TargetSessionKey: targetKey,
		LocalSessionID:   target.ID,
		AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, nil, agent, e.sessions)

	agent.mu.Lock()
	gotIDs := append([]string(nil), agent.sessionIDs...)
	agent.mu.Unlock()
	if !reflect.DeepEqual(gotIDs, []string{"thread-existing"}) {
		t.Fatalf("resume session IDs = %#v, want thread-existing", gotIDs)
	}
	if got := len(e.sessions.ListSessions(targetKey)); got != 1 {
		t.Fatalf("target sessions = %d, want 1", got)
	}
	op, _ := store.Get(opID)
	if op.Step != NewOperationCompleted || op.AgentSessionID != "thread-existing" {
		t.Fatalf("operation = %+v", op)
	}
}

func TestAdvanceNewOperation_RecoveryStartDoesNotBlockOtherRoute(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	releaseA := make(chan struct{})
	var releaseAOnce sync.Once
	t.Cleanup(func() { releaseAOnce.Do(func() { close(releaseA) }) })
	agent := &blockingRecoverableAgent{
		controllableAgent: &controllableAgent{startSessionFn: func(_ context.Context, sessionID string) (AgentSession, error) {
			if sessionID == "thread-a" {
				return &namingAgentSession{controllableAgentSession: newControllableSession("thread-a")}, nil
			}
			return newControllableSession("thread-b"), nil
		}},
		started: make(chan struct{}),
		release: releaseA,
	}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	targetKeyA := "feishu:chat-a:user-1"
	targetA := e.sessions.GetOrCreateActive(targetKeyA)
	targetA.SetName("[Codex] A")
	targetA.SetAgentSessionID("thread-a", agent.Name())
	opID := NewOperationID("test", "feishu:source:user-1", "message-a")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:source:user-1",
		SourceMessageID: "message-a", UserID: "user-1", Name: "[Codex] A",
		Step: NewOperationSessionBound, Status: NewOperationRunning, TargetSessionKey: targetKeyA,
		LocalSessionID: targetA.ID, AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	doneA := make(chan struct{})
	go func() {
		e.advanceNewOperation(p, opID, nil, agent, e.sessions)
		close(doneA)
	}()
	select {
	case <-agent.started:
	case <-time.After(time.Second):
		t.Fatal("recoverable starter for route A did not begin")
	}
	if !targetA.Busy() {
		t.Fatal("route A target session was released while recovery was still starting")
	}

	targetKeyB := "feishu:chat-b:user-1"
	targetB := e.sessions.GetOrCreateActive(targetKeyB)
	if !targetB.TryLock() {
		t.Fatal("could not lock route B")
	}
	doneB := make(chan *interactiveState, 1)
	go func() {
		doneB <- e.getOrCreateInteractiveStateWith(targetKeyB, p, "ctx-b", targetB, e.sessions, nil, "")
	}()
	select {
	case state := <-doneB:
		if state == nil {
			t.Fatal("route B did not get an interactive state")
		}
		state.mu.Lock()
		startedB := state.agentSession != nil
		state.mu.Unlock()
		if !startedB {
			t.Fatal("route B did not complete agent session startup")
		}
		targetB.UnlockWithoutUpdate()
	case <-time.After(time.Second):
		t.Fatal("route B start was blocked by route A recovery")
	}

	// A duplicate recovery drive must be rejected by the target-operation gate,
	// not start another recoverable session while the first is still blocked.
	e.advanceNewOperation(p, opID, nil, agent, e.sessions)
	if got := agent.callCount(); got != 1 {
		t.Fatalf("recoverable starts for route A = %d, want 1", got)
	}

	releaseAOnce.Do(func() { close(releaseA) })
	select {
	case <-doneA:
	case <-time.After(time.Second):
		t.Fatal("route A did not finish after its starter was released")
	}
	if got := agent.callCount(); got != 1 {
		t.Fatalf("recoverable starts after release = %d, want 1", got)
	}
	if got, want := agent.recoveryKeys(), []string{"cc-connect/new/" + opID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recovery keys = %#v, want %#v", got, want)
	}
}

func TestAdvanceNewOperation_NameSyncSnapshotAllowsConcurrentCleanup(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	targetKey := "feishu:chat-b:user-1"
	target := e.sessions.GetOrCreateActive(targetKey)
	target.SetAgentSessionID("thread-b", "stub")
	opID := NewOperationID("test", "feishu:source:user-1", "message-name")
	op := NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:source:user-1",
		SourceMessageID: "message-name", UserID: "user-1", Name: "[Codex] 名称同步",
		Step: NewOperationAgentSessionStarted, Status: NewOperationRunning, TargetSessionKey: targetKey,
		LocalSessionID: target.ID, AgentSessionID: "thread-b", AgentRecoveryKey: "cc-connect/new/" + opID,
	}
	if _, _, err := store.CreateOrGet(op); err != nil {
		t.Fatal(err)
	}
	nameRelease := make(chan struct{})
	session := &blockingNamingSession{
		id: "thread-b", events: make(chan Event), started: make(chan struct{}, 1), release: nameRelease,
	}
	state := &interactiveState{agentSession: session}
	e.interactiveMu.Lock()
	e.interactiveStates[targetKey] = state
	e.interactiveMu.Unlock()

	done := make(chan struct{})
	go func() {
		e.advanceNewOperation(p, opID, nil, e.agent, e.sessions)
		close(done)
	}()
	select {
	case <-session.started:
	case <-time.After(time.Second):
		t.Fatal("name sync did not begin")
	}

	cleanupDone := make(chan struct{})
	go func() {
		e.cleanupInteractiveState(targetKey, state)
		close(cleanupDone)
	}()
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup was blocked by name sync")
	}
	close(nameRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("name sync did not finish after cleanup")
	}
}

func TestAdvanceNewOperation_AgentSessionStartedDoesNotCompleteWhenResumeFails(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	startCalls := 0
	agent := &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		startCalls++
		return nil, errors.New("codex unavailable")
	}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	targetKey := "feishu:chat-b:user-1"
	target := e.sessions.GetOrCreateActive(targetKey)
	target.SetAgentSessionID("thread-existing", agent.Name())
	e.sessions.Save()
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-resume-fail")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-resume-fail", UserID: "user-1", Name: "[Codex] 恢复失败",
		Step: NewOperationAgentSessionStarted, Status: NewOperationRunning,
		TargetSessionKey: targetKey, LocalSessionID: target.ID, AgentSessionID: "thread-existing",
		AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, "ctx-a", agent, e.sessions)
	got, _ := store.Get(opID)
	if got.Step != NewOperationAgentSessionStarted || got.Status != NewOperationRetryWait {
		t.Fatalf("operation falsely advanced after failed resume: %+v", got)
	}
	if startCalls != 1 {
		t.Fatalf("agent start calls = %d, want one strict resume without fresh fallback", startCalls)
	}
	if sent := p.getSent(); len(sent) != 1 || !strings.Contains(sent[0], "自动重试") {
		t.Fatalf("notifications after failed resume = %#v, want retry notice only", sent)
	}
}

func TestAdvanceNewOperation_DoesNotAdvanceWhenSessionBindingCannotPersist(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.json")
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, sessionPath, LangChinese)
	store, err := NewNewOperationStore(filepath.Join(dir, "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	if err := os.Mkdir(sessionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-session-write")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-session-write", UserID: "user-1", Name: "[Codex] Session落盘",
		Step: NewOperationConversationSpawned, Status: NewOperationRunning,
		TargetSessionKey: "feishu:chat-b:user-1", AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, "ctx-a", agent, e.sessions)
	got, _ := store.Get(opID)
	if got.Step != NewOperationConversationSpawned || got.Status != NewOperationRetryWait {
		t.Fatalf("operation advanced without durable B session: %+v", got)
	}
}

func TestAdvanceNewOperation_DoesNotAdvanceWhenAgentSessionIDCannotPersist(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.json")
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, sessionPath, LangChinese)
	targetKey := "feishu:chat-b:user-1"
	target := e.sessions.GetOrCreateActive(targetKey)
	e.sessions.Save()
	if err := os.Rename(sessionPath, filepath.Join(dir, "sessions.backup.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sessionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := NewNewOperationStore(filepath.Join(dir, "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-agent-write")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-agent-write", UserID: "user-1", Name: "[Codex] Task落盘",
		Step: NewOperationSessionBound, Status: NewOperationRunning,
		TargetSessionKey: targetKey, LocalSessionID: target.ID, AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, "ctx-a", agent, e.sessions)
	got, _ := store.Get(opID)
	if got.Step != NewOperationSessionBound || got.Status != NewOperationRetryWait {
		t.Fatalf("operation advanced without durable agent session ID: %+v", got)
	}
}

func TestAdvanceNewOperation_BusyTargetDoesNotConsumeFailureBudget(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.newOperationRetryDelay = 20 * time.Millisecond
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	targetKey := "feishu:chat-b:user-1"
	target := e.sessions.GetOrCreateActive(targetKey)
	if !target.TryLock() {
		t.Fatal("failed to hold target session lock")
	}
	t.Cleanup(func() {
		_ = e.Stop()
		target.UnlockWithoutUpdate()
	})
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-busy")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-busy", UserID: "user-1", Name: "[Codex] Busy",
		Step: NewOperationSessionBound, Status: NewOperationRunning, StepAttempts: newOperationMaxAttempts - 1,
		TargetSessionKey: targetKey, LocalSessionID: target.ID, AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, "ctx-a", e.agent, e.sessions)
	got, _ := store.Get(opID)
	if got.Status != NewOperationRetryWait || got.StepAttempts != newOperationMaxAttempts-1 {
		t.Fatalf("busy target consumed failure budget: %+v", got)
	}
}

func TestAdvanceNewOperation_OperationWriteFailureRetriesInMemoryAfterRepair(t *testing.T) {
	dir := t.TempDir()
	operationPath := filepath.Join(dir, "operations.json")
	backupPath := filepath.Join(dir, "operations.backup.json")
	p := &lockedRetrySpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned:            SpawnedConversation{SessionKey: "feishu:chat-b:user-1", ReplyCtx: "ctx-b"},
	}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(dir, "sessions.json"), LangChinese)
	e.newOperationRetryDelay = 30 * time.Millisecond
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	if !e.markPlatformReady(p) {
		t.Fatal("failed to mark platform ready")
	}
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-write-retry")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-write-retry", UserID: "user-1", Name: "[Codex] 写盘恢复",
		Step: NewOperationAccepted, Status: NewOperationRunning, AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(operationPath, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(operationPath, 0o755); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, "ctx-a", agent, e.sessions)
	if p.requestCount() != 1 {
		t.Fatalf("initial spawn calls = %d, want 1", p.requestCount())
	}
	if err := os.Remove(operationPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupPath, operationPath); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			if p.requestCount() != 2 {
				t.Fatalf("spawn calls after recovery = %d, want idempotent replay once", p.requestCount())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	t.Fatalf("operation did not retry after persistence repair: %+v", op)
}

func TestAdvanceNewOperation_CompletedWriteFailureKeepsInMemoryRetry(t *testing.T) {
	dir := t.TempDir()
	operationPath := filepath.Join(dir, "operations.json")
	backupPath := filepath.Join(dir, "operations.backup.json")
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(dir, "sessions.json"), LangChinese)
	e.newOperationRetryDelay = 30 * time.Millisecond
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	if !e.markPlatformReady(p) {
		t.Fatal("failed to mark platform ready")
	}
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-complete-write-retry")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-complete-write-retry", UserID: "user-1", Name: "[Codex] 完成态写盘恢复",
		Step: NewOperationNameSynced, Status: NewOperationRunning, TargetSessionKey: "feishu:chat-b:user-1",
		TargetNotified: true, SourceNotified: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(operationPath, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(operationPath, 0o755); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, "ctx-a", e.agent, e.sessions)
	if err := os.Remove(operationPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupPath, operationPath); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	t.Fatalf("completed state did not retry after persistence repair: %+v", op)
}

func TestAdvanceNewOperation_PermanentAgentStartFailureRequiresManualActionImmediately(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	agent := &permanentlyFailingRecoverableAgent{controllableAgent: &controllableAgent{}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	targetKey := "feishu:chat-b:user-1"
	target := e.sessions.GetOrCreateActive(targetKey)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-permanent-agent")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-permanent-agent", UserID: "user-1", Name: "[Codex] 后端不支持",
		Step: NewOperationSessionBound, Status: NewOperationRunning, TargetSessionKey: targetKey,
		LocalSessionID: target.ID, AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperation(p, opID, "ctx-a", agent, e.sessions)
	op, _ := store.Get(opID)
	if op.Status != NewOperationManualAction || op.StepAttempts != 1 {
		t.Fatalf("permanent agent failure was not preserved: %+v", op)
	}
}

func TestAdvanceNewOperation_FirstTargetMessageDoesNotWaitForNameSync(t *testing.T) {
	p := &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned: SpawnedConversation{
			SessionKey: "feishu:chat-b:user-1",
			ReplyCtx:   "ctx-b",
		},
	}
	baseSession := newControllableSession("thread-b")
	baseSession.sendPrompts = make(chan string, 1)
	nameStarted := make(chan struct{}, 1)
	nameRelease := make(chan struct{})
	agentSession := &namingAgentSession{
		controllableAgentSession: baseSession,
		nameStarted:              nameStarted,
		nameRelease:              nameRelease,
	}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	newDone := make(chan struct{})
	go func() {
		e.cmdNew(p, &Message{
			Platform:   "feishu",
			SessionKey: "feishu:chat-a:user-1",
			UserID:     "user-1",
			MessageID:  "message-name-sync",
			ChatName:   "A群",
			ReplyCtx:   "ctx-a",
		}, []string{"新项目"})
		close(newDone)
	}()

	select {
	case <-nameStarted:
	case <-time.After(time.Second):
		t.Fatal("name sync did not start")
	}
	target := e.sessions.GetOrCreateActive("feishu:chat-b:user-1")
	if target.Busy() {
		t.Fatal("target session remained locked during name sync")
	}

	messageDone := make(chan struct{})
	go func() {
		e.ReceiveMessage(p, &Message{
			Platform:   "feishu",
			SessionKey: "feishu:chat-b:user-1",
			UserID:     "user-1",
			UserName:   "用户",
			MessageID:  "first-target-message",
			ChatName:   "[Codex] 新项目",
			Content:    "第一条真实消息",
			ReplyCtx:   "ctx-first",
		})
		close(messageDone)
	}()

	select {
	case prompt := <-baseSession.sendPrompts:
		if !strings.Contains(prompt, "第一条真实消息") {
			t.Fatalf("prompt = %q", prompt)
		}
	case <-time.After(time.Second):
		close(nameRelease)
		t.Fatal("first target message waited for name sync")
	}
	baseSession.events <- Event{Type: EventText, Content: "第一条回复"}
	baseSession.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	close(nameRelease)

	select {
	case <-newDone:
	case <-time.After(time.Second):
		t.Fatal("/new did not finish")
	}
	select {
	case <-messageDone:
	case <-time.After(time.Second):
		t.Fatal("first target message did not finish")
	}
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-name-sync")
	op, _ := store.Get(opID)
	if op.Step != NewOperationCompleted || target.GetAgentSessionID() != "thread-b" {
		t.Fatalf("operation = %+v targetAgent=%q", op, target.GetAgentSessionID())
	}
}

func TestNewOperationRecovery_FirstTargetMessageUsesStableRecoveryKey(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	baseSession := newControllableSession("thread-b")
	baseSession.sendPrompts = make(chan string, 1)
	agentSession := &namingAgentSession{controllableAgentSession: baseSession}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	targetKey := "feishu:chat-b:user-1"
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-race")
	recoveryKey := "cc-connect/new/" + opID
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               opID,
		Project:          "test",
		Platform:         "feishu",
		SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID:  "message-race",
		UserID:           "user-1",
		Name:             "[Codex] 并发恢复",
		Step:             NewOperationConversationSpawned,
		Status:           NewOperationRunning,
		TargetSessionKey: targetKey,
		AgentRecoveryKey: recoveryKey,
	}); err != nil {
		t.Fatal(err)
	}

	messageDone := make(chan struct{})
	go func() {
		e.ReceiveMessage(p, &Message{
			Platform:   "feishu",
			SessionKey: targetKey,
			UserID:     "user-1",
			UserName:   "用户",
			MessageID:  "first-target-message",
			ChatName:   "[Codex] 并发恢复",
			Content:    "抢先到达的第一条消息",
			ReplyCtx:   "ctx-first",
		})
		close(messageDone)
	}()
	select {
	case <-baseSession.sendPrompts:
	case <-time.After(time.Second):
		t.Fatal("first target message did not reach agent")
	}
	baseSession.events <- Event{Type: EventText, Content: "收到"}
	baseSession.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	select {
	case <-messageDone:
	case <-time.After(time.Second):
		t.Fatal("first target message did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if target := e.sessions.GetOrCreateActive(targetKey); !target.Busy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if target := e.sessions.GetOrCreateActive(targetKey); target.Busy() {
		t.Fatal("first target turn did not release the session")
	}

	e.advanceNewOperation(p, opID, nil, agent, e.sessions)

	if keys := agent.keys(); !reflect.DeepEqual(keys, []string{recoveryKey}) {
		t.Fatalf("recovery keys = %#v, want %q", keys, recoveryKey)
	}
	if got := len(e.sessions.ListSessions(targetKey)); got != 1 {
		t.Fatalf("target sessions = %d, want 1", got)
	}
	op, _ := store.Get(opID)
	if op.Step != NewOperationCompleted || op.AgentSessionID != "thread-b" {
		t.Fatalf("operation = %+v", op)
	}
}

func TestNewOperationRecovery_TargetWriteFailureKeepsRecoveryKeyForFirstMessageAndRestart(t *testing.T) {
	dir := t.TempDir()
	operationPath := filepath.Join(dir, "operations.json")
	operationBackup := filepath.Join(dir, "operations.backup.json")
	sessionPath := filepath.Join(dir, "sessions.json")
	const (
		sourceKey = "feishu:chat-a:user-1"
		targetKey = "feishu:chat-b:user-1"
	)

	p := &lockedRetrySpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned:            SpawnedConversation{SessionKey: targetKey, ReplyCtx: "ctx-b"},
	}
	firstSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	firstSession.sendPrompts = make(chan string, 1)
	firstAgent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: firstSession}}
	firstEngine := NewEngine("test", firstAgent, []Platform{p}, sessionPath, LangChinese)
	firstEngine.newOperationRetryDelay = time.Hour
	firstStore, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	firstEngine.SetNewOperationStore(firstStore)
	opID := NewOperationID("test", sourceKey, "message-target-write-failure")
	recoveryKey := "cc-connect/new/" + opID
	if _, _, err := firstStore.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: sourceKey,
		SourceMessageID: "message-target-write-failure", UserID: "user-1", Name: "[Codex] 写盘窗口",
		Step: NewOperationAccepted, Status: NewOperationRunning, AgentRecoveryKey: recoveryKey,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(operationPath, operationBackup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(operationPath, 0o755); err != nil {
		t.Fatal(err)
	}

	firstEngine.advanceNewOperation(p, opID, "ctx-a", firstAgent, firstEngine.sessions)
	if op, _ := firstStore.Get(opID); op.TargetSessionKey != "" || op.Step != NewOperationAccepted {
		t.Fatalf("failed target write changed durable operation: %+v", op)
	}

	messageDone := make(chan struct{})
	go func() {
		firstEngine.ReceiveMessage(p, &Message{
			Platform: "feishu", SessionKey: targetKey, UserID: "user-1", UserName: "用户",
			MessageID: "first-target-message-after-write-failure", ChatName: "[Codex] 写盘窗口",
			Content: "写盘失败后的第一条消息", ReplyCtx: "ctx-first",
		})
		close(messageDone)
	}()
	select {
	case <-firstSession.sendPrompts:
	case <-time.After(time.Second):
		t.Fatal("first target message did not reach agent")
	}
	if keys := firstAgent.keys(); !reflect.DeepEqual(keys, []string{recoveryKey}) {
		t.Fatalf("first target recovery keys = %#v, want %q", keys, recoveryKey)
	}
	firstSession.events <- Event{Type: EventText, Content: "收到"}
	firstSession.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	select {
	case <-messageDone:
	case <-time.After(time.Second):
		t.Fatal("first target message did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !firstEngine.sessions.GetOrCreateActive(targetKey).Busy() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if firstEngine.sessions.GetOrCreateActive(targetKey).Busy() {
		t.Fatal("first target turn did not release the session before restart")
	}

	if err := firstEngine.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(operationPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(operationBackup, operationPath); err != nil {
		t.Fatal(err)
	}

	restartedSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	restartedAgent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: restartedSession}}
	restartedPlatform := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned:            SpawnedConversation{SessionKey: targetKey, ReplyCtx: "ctx-b"},
	}}
	restartedEngine := NewEngine("test", restartedAgent, []Platform{restartedPlatform}, sessionPath, LangChinese)
	t.Cleanup(func() { _ = restartedEngine.Stop() })
	reloadedStore, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	restartedEngine.SetNewOperationStore(reloadedStore)
	restartedEngine.OnPlatformReady(restartedPlatform)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, _ := reloadedStore.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	op, _ := reloadedStore.Get(opID)
	if op.Status != NewOperationStatusCompleted || op.AgentSessionID != "thread-b" {
		t.Fatalf("restarted operation = %+v", op)
	}
	if keys := restartedAgent.keys(); !reflect.DeepEqual(keys, []string{recoveryKey}) {
		t.Fatalf("restarted recovery keys = %#v, want %q", keys, recoveryKey)
	}
	if got := restartedAgent.sessionIDs; !reflect.DeepEqual(got, []string{"thread-b"}) {
		t.Fatalf("restarted session IDs = %#v, want resume thread-b", got)
	}
}

func TestNewOperationRecovery_PlatformReadyRecoversUnknownTargetBeforeFirstMessage(t *testing.T) {
	const (
		sourceKey = "feishu:chat-a:user-1"
		targetKey = "feishu:chat-b:user-1"
	)
	p := &blockingRecoverySpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		started:            make(chan struct{}),
		release:            make(chan struct{}),
		spawned:            SpawnedConversation{SessionKey: targetKey, ReplyCtx: "ctx-b"},
	}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agentSession.sendPrompts = make(chan string, 1)
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", sourceKey, "message-ready-gate")
	recoveryKey := "cc-connect/new/" + opID
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: sourceKey,
		SourceMessageID: "message-ready-gate", UserID: "user-1", Name: "[Codex] Ready 门禁",
		Step: NewOperationAccepted, Status: NewOperationRunning, AgentRecoveryKey: recoveryKey,
	}); err != nil {
		t.Fatal(err)
	}

	e.OnPlatformReady(p)
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("platform recovery did not start")
	}
	messageDone := make(chan struct{})
	go func() {
		e.ReceiveMessage(p, &Message{
			Platform: "feishu", SessionKey: targetKey, UserID: "user-1", UserName: "用户",
			MessageID: "first-target-message-during-ready-recovery", ChatName: "[Codex] Ready 门禁",
			Content: "恢复尚未识别目标时到达", ReplyCtx: "ctx-first",
		})
		close(messageDone)
	}()

	escapedGate := false
	select {
	case <-agentSession.sendPrompts:
		escapedGate = true
	case <-time.After(100 * time.Millisecond):
	}
	close(p.release)
	if !escapedGate {
		select {
		case <-agentSession.sendPrompts:
		case <-time.After(time.Second):
			t.Fatal("first target message did not continue after recovery")
		}
	}
	agentSession.events <- Event{Type: EventText, Content: "收到"}
	agentSession.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	select {
	case <-messageDone:
	case <-time.After(time.Second):
		t.Fatal("first target message did not finish")
	}
	if escapedGate {
		t.Fatal("first target message started before platform recovery identified the target")
	}
	if keys := agent.keys(); !reflect.DeepEqual(keys, []string{recoveryKey}) {
		t.Fatalf("recovery keys = %#v, want %q", keys, recoveryKey)
	}
}

func TestNewOperationRecovery_WaitsForPlatformReady(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned: SpawnedConversation{
			SessionKey: "feishu:chat-b:user-1",
			ReplyCtx:   "ctx-b",
		},
	}}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-ready")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               opID,
		Project:          "test",
		Platform:         "feishu",
		SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID:  "message-ready",
		UserID:           "user-1",
		Name:             "[Codex] Ready恢复",
		Step:             NewOperationAccepted,
		Status:           NewOperationRunning,
		AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("spawn calls before Ready = %d", len(p.requests))
	}

	e.OnPlatformReady(p)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	if op.Status != NewOperationStatusCompleted || len(p.requests) != 1 {
		t.Fatalf("operation = %+v, spawn calls = %d", op, len(p.requests))
	}
}

func TestNewOperationRecovery_TransientFailureRetriesWhilePlatformRemainsReady(t *testing.T) {
	p := &lockedRetrySpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned: SpawnedConversation{
			SessionKey: "feishu:chat-b:user-1",
			ReplyCtx:   "ctx-b",
		},
		spawnErr: context.DeadlineExceeded,
	}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.newOperationRetryDelay = 20 * time.Millisecond
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-retry")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               opID,
		Project:          "test",
		Platform:         "feishu",
		SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID:  "message-retry",
		UserID:           "user-1",
		Name:             "[Codex] 自动重试",
		Step:             NewOperationAccepted,
		Status:           NewOperationRunning,
		AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.OnPlatformReady(p)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationRetryWait {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	if op.Status != NewOperationRetryWait {
		t.Fatalf("operation did not enter retry wait: %+v", op)
	}
	p.setSpawnError(nil)

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ = store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if op.Status != NewOperationStatusCompleted || p.requestCount() != 2 {
		t.Fatalf("operation = %+v, spawn calls = %d", op, p.requestCount())
	}
}

func TestNewOperationRecovery_RestoresFutureRetryTimerAfterRestart(t *testing.T) {
	p := &lockedRetrySpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned: SpawnedConversation{
			SessionKey: "feishu:chat-b:user-1",
			ReplyCtx:   "ctx-b",
		},
	}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-future")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:               opID,
		Project:          "test",
		Platform:         "feishu",
		SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID:  "message-future",
		UserID:           "user-1",
		Name:             "[Codex] 重启计时",
		Step:             NewOperationAccepted,
		Status:           NewOperationRetryWait,
		NextAttemptAt:    time.Now().Add(60 * time.Millisecond),
		AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.OnPlatformReady(p)
	time.Sleep(20 * time.Millisecond)
	if p.requestCount() != 0 {
		t.Fatalf("spawn calls before retry time = %d", p.requestCount())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	t.Fatalf("future retry was not restored: %+v", op)
}

func TestNewOperationRecovery_PlatformUnavailableCancelsCurrentRequestAndResumesNextReady(t *testing.T) {
	p := &cancellationAwareSpawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		started:            make(chan struct{}),
		spawned:            SpawnedConversation{SessionKey: "feishu:chat-b:user-1", ReplyCtx: "ctx-b"},
	}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.newOperationRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-disconnect")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-disconnect", UserID: "user-1", Name: "[Codex] 掉线恢复",
		Step: NewOperationAccepted, Status: NewOperationRunning, AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.OnPlatformReady(p)
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("spawn request did not start")
	}
	e.OnPlatformUnavailable(p, errors.New("connection lost"))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationRetryWait {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.callCount() != 1 {
		t.Fatalf("spawn calls while unavailable = %d, want 1", p.callCount())
	}
	time.Sleep(40 * time.Millisecond)
	if p.callCount() != 1 {
		t.Fatalf("retry ran while unavailable: calls=%d", p.callCount())
	}

	e.OnPlatformReady(p)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			if p.callCount() != 2 {
				t.Fatalf("spawn calls after reconnect = %d, want 2", p.callCount())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	t.Fatalf("operation did not resume after reconnect: %+v", op)
}

func TestNewOperationRecovery_PlatformUnavailableCancelsAgentStartAndResumesNextReady(t *testing.T) {
	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &cancellationAwareRecoverableAgent{
		controllableAgent: &controllableAgent{nextSession: agentSession},
		started:           make(chan struct{}),
	}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.newOperationRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	targetKey := "feishu:chat-b:user-1"
	target := e.sessions.GetOrCreateActive(targetKey)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-agent-disconnect")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-agent-disconnect", UserID: "user-1", Name: "[Codex] Agent掉线恢复",
		Step: NewOperationSessionBound, Status: NewOperationRunning, TargetSessionKey: targetKey,
		LocalSessionID: target.ID, AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.OnPlatformReady(p)
	select {
	case <-agent.started:
	case <-time.After(time.Second):
		t.Fatal("agent start did not begin")
	}
	e.OnPlatformUnavailable(p, errors.New("connection lost"))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationRetryWait {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if agent.callCount() != 1 {
		t.Fatalf("agent starts while unavailable = %d, want 1", agent.callCount())
	}

	e.OnPlatformReady(p)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			if agent.callCount() != 2 {
				t.Fatalf("agent starts after reconnect = %d, want 2", agent.callCount())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	t.Fatalf("agent start did not resume after reconnect: %+v", op)
}

func TestNewOperationRecovery_PlatformUnavailableCancelsNotificationAndResumesNextReady(t *testing.T) {
	p := &cancellationAwareNotificationPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		started:            make(chan struct{}),
	}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.newOperationRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	opID := NewOperationID("test", "feishu:chat-a:user-1", "message-notice-disconnect")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID: opID, Project: "test", Platform: "feishu", SourceSessionKey: "feishu:chat-a:user-1",
		SourceMessageID: "message-notice-disconnect", UserID: "user-1", Name: "[Codex] 通知掉线恢复",
		Step: NewOperationNameSynced, Status: NewOperationRunning, TargetSessionKey: "feishu:chat-b:user-1",
	}); err != nil {
		t.Fatal(err)
	}

	e.OnPlatformReady(p)
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("target notification did not begin")
	}
	e.OnPlatformUnavailable(p, errors.New("connection lost"))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationRetryWait {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.callCount() != 1 {
		t.Fatalf("notifications while unavailable = %d, want 1", p.callCount())
	}

	e.OnPlatformReady(p)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := store.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			if p.callCount() != 2 {
				t.Fatalf("notifications after reconnect = %d, want 2", p.callCount())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(opID)
	t.Fatalf("notification did not resume after reconnect: %+v", op)
}

func TestNewOperationRecovery_ReloadsSessionAndOperationAfterProcessRestart(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.json")
	operationPath := filepath.Join(dir, "operations.json")
	firstEngine := NewEngine("test", &stubAgent{}, nil, sessionPath, LangChinese)
	const sourceKey = "feishu:chat-a:user-1"
	const targetKey = "feishu:chat-b:user-1"
	source := firstEngine.sessions.GetOrCreateActive(sourceKey)
	source.SetAgentSessionID("thread-a", "controllable")
	source.AddHistory("user", "重启前的 A 群历史")
	target := firstEngine.sessions.GetOrCreateActive(targetKey)
	target.SetName("[Codex] 进程重启")
	firstEngine.sessions.Save()
	opID := NewOperationID("test", sourceKey, "message-restart")
	firstStore, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := firstStore.CreateOrGet(NewOperation{
		ID:               opID,
		Project:          "test",
		Platform:         "feishu",
		SourceSessionKey: sourceKey,
		SourceMessageID:  "message-restart",
		UserID:           "user-1",
		Name:             "[Codex] 进程重启",
		Step:             NewOperationSessionBound,
		Status:           NewOperationRunning,
		TargetSessionKey: targetKey,
		LocalSessionID:   target.ID,
		AgentRecoveryKey: "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	p := &reconstructingSpawningPlatform{spawningPlatform: &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
	}}
	agentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{controllableAgent: &controllableAgent{nextSession: agentSession}}
	restartedEngine := NewEngine("test", agent, []Platform{p}, sessionPath, LangChinese)
	reloadedStore, err := NewNewOperationStore(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	restartedEngine.SetNewOperationStore(reloadedStore)
	restartedEngine.OnPlatformReady(p)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, _ := reloadedStore.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	op, _ := reloadedStore.Get(opID)
	if op.Status != NewOperationStatusCompleted || op.AgentSessionID != "thread-b" {
		t.Fatalf("operation = %+v", op)
	}
	reloadedSource := restartedEngine.sessions.GetOrCreateActive(sourceKey)
	if reloadedSource.GetAgentSessionID() != "thread-a" {
		t.Fatalf("source agent session = %q", reloadedSource.GetAgentSessionID())
	}
	history := reloadedSource.GetHistory(0)
	if len(history) != 1 || history[0].Content != "重启前的 A 群历史" {
		t.Fatalf("source history = %+v", history)
	}
	if got := len(restartedEngine.sessions.ListSessions(targetKey)); got != 1 {
		t.Fatalf("target sessions = %d, want 1", got)
	}
}

func TestCmdNew_PersistedOperationDeduplicatesRepeatedDelivery(t *testing.T) {
	p := &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned: SpawnedConversation{
			SessionKey: "feishu:chat-b:user-1",
			ReplyCtx:   "ctx-b",
		},
	}
	newAgentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &recoverableControllableAgent{
		controllableAgent: &controllableAgent{nextSession: newAgentSession},
	}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	const sourceKey = "feishu:chat-a:user-1"
	source := e.sessions.GetOrCreateActive(sourceKey)
	source.SetAgentSessionID("thread-a", "codex")
	source.AddHistory("user", "A 群历史")
	e.sessions.Save()
	sourceID := source.ID
	sourceHistory := source.GetHistory(0)
	msg := &Message{
		Platform:   "feishu",
		SessionKey: sourceKey,
		UserID:     "user-1",
		MessageID:  "message-1",
		ChatName:   "A群",
		ReplyCtx:   "ctx-a",
	}

	e.cmdNew(p, msg, []string{"新项目"})
	e.cmdNew(p, msg, []string{"新项目"})

	if len(p.requests) != 1 {
		t.Fatalf("SpawnConversation calls = %d, want 1", len(p.requests))
	}
	keys := agent.keys()
	if len(keys) != 1 || keys[0] == "" {
		t.Fatalf("recovery keys = %#v, want one stable key", keys)
	}
	if got := len(e.sessions.ListSessions("feishu:chat-b:user-1")); got != 1 {
		t.Fatalf("target session count = %d, want 1", got)
	}
	if source.ID != sourceID || source.GetAgentSessionID() != "thread-a" || !reflect.DeepEqual(source.GetHistory(0), sourceHistory) {
		t.Fatalf("source session changed: %+v", source)
	}
	opID := NewOperationID("test", sourceKey, "message-1")
	op, ok := store.Get(opID)
	if !ok || op.Step != NewOperationCompleted || op.Status != NewOperationStatusCompleted {
		t.Fatalf("operation = %+v, ok = %v", op, ok)
	}
}

func TestCmdNew_PersistedOperationRejectsEmptyMessageIDBeforeSpawn(t *testing.T) {
	p := &spawningPlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "feishu"},
		spawned:            SpawnedConversation{SessionKey: "feishu:chat-b:user-1", ReplyCtx: "ctx-b"},
	}
	e := NewEngine("test", &controllableAgent{}, []Platform{p}, "", LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)

	e.cmdNew(p, &Message{
		Platform:   "feishu",
		SessionKey: "feishu:chat-a:user-1",
		UserID:     "user-1",
		MessageID:  "",
		ChatName:   "A群",
		ReplyCtx:   "ctx-a",
	}, []string{"新项目"})

	if len(p.requests) != 0 {
		t.Fatalf("SpawnConversation calls = %d, want 0", len(p.requests))
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "message ID") {
		t.Fatalf("reply = %q, want stable-id error", got)
	}
}
