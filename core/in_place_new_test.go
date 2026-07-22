package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type inPlaceNewPlatform struct {
	*stubPlatformEngine
}

func (p *inPlaceNewPlatform) SupportsInPlaceConversationFork() bool { return true }

func (p *inPlaceNewPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "reconstructed:" + sessionKey, nil
}

type labeledInPlaceNewPlatform struct {
	*inPlaceNewPlatform
	sessionKey string
}

type blockingCapabilityInPlacePlatform struct {
	*inPlaceNewPlatform
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

type secondCapabilityCallBlockingPlatform struct {
	*inPlaceNewPlatform
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

type externalFirstNewPlatform struct {
	*spawningPlatform
}

func (p *externalFirstNewPlatform) SupportsInPlaceConversationFork() bool { return true }

func (p *blockingCapabilityInPlacePlatform) SupportsInPlaceConversationFork() bool {
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return true
}

func (p *secondCapabilityCallBlockingPlatform) SupportsInPlaceConversationFork() bool {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 2 {
		close(p.entered)
		<-p.release
	}
	return true
}

func (p *labeledInPlaceNewPlatform) ReplySessionKey(any) string { return p.sessionKey }

func (p *labeledInPlaceNewPlatform) FormatSessionReply(sessionName, content string) string {
	label := strings.TrimSpace(strings.TrimPrefix(sessionName, "[Codex]"))
	if label == "" {
		label = "微信"
	}
	prefix := "[" + label + "] "
	if strings.HasPrefix(content, prefix) {
		return content
	}
	return prefix + content
}

type failingActivatedReplyPlatform struct {
	*inPlaceNewPlatform
	mu        sync.Mutex
	replyErr  error
	replyCall int
}

func (p *failingActivatedReplyPlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	p.mu.Lock()
	p.replyCall++
	call := p.replyCall
	err := p.replyErr
	p.mu.Unlock()
	if call == 1 {
		return err
	}
	return p.inPlaceNewPlatform.Reply(ctx, replyCtx, content)
}

type inPlaceNewAgent struct {
	*controllableAgent
	mu           sync.Mutex
	recoveryKeys []string
	formatted    []string
}

// persistentInPlaceRegistry models the durable Codex recovery-key index. A
// process restart creates a new live AgentSession wrapper, while the same
// recovery key still resolves to exactly one durable Codex thread.
type persistentInPlaceRegistry struct {
	mu          sync.Mutex
	threads     map[string]string
	starts      []string
	nameStarted chan struct{}
	nameRelease <-chan struct{}
}

func newPersistentInPlaceRegistry() *persistentInPlaceRegistry {
	return &persistentInPlaceRegistry{threads: make(map[string]string)}
}

func (r *persistentInPlaceRegistry) remember(recoveryKey, threadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.threads[recoveryKey] = threadID
}

func (r *persistentInPlaceRegistry) start(recoveryKey, requestedID string) AgentSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.threads[recoveryKey]
	if id == "" {
		id = requestedID
		if id == "" {
			id = fmt.Sprintf("thread-%d", len(r.threads)+1)
		}
		r.threads[recoveryKey] = id
	}
	r.starts = append(r.starts, recoveryKey)
	base := &primingAgentSession{namingAgentSession: &namingAgentSession{controllableAgentSession: newControllableSession(id)}}
	if r.nameStarted != nil {
		return &persistentBlockingNameSession{primingAgentSession: base, started: r.nameStarted, release: r.nameRelease}
	}
	return base
}

func (r *persistentInPlaceRegistry) counts() (threads, starts int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.threads), len(r.starts)
}

type persistentBlockingNameSession struct {
	*primingAgentSession
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *persistentBlockingNameSession) SetSessionName(name string) error {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return s.primingAgentSession.SetSessionName(name)
}

type persistentInPlaceAgent struct {
	name     string
	registry *persistentInPlaceRegistry
}

func (a *persistentInPlaceAgent) Name() string { return a.name }
func (a *persistentInPlaceAgent) StartSession(_ context.Context, sessionID string) (AgentSession, error) {
	return a.registry.start("fresh:"+sessionID, sessionID), nil
}
func (a *persistentInPlaceAgent) StartRecoverableSession(_ context.Context, sessionID, recoveryKey string) (AgentSession, error) {
	return a.registry.start(recoveryKey, sessionID), nil
}
func (a *persistentInPlaceAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *persistentInPlaceAgent) Stop() error { return nil }
func (a *persistentInPlaceAgent) FormatConversationName(name string) string {
	return "[Codex] " + strings.TrimSpace(name)
}

type persistentInPlaceHarness struct {
	t           *testing.T
	dir         string
	sessionPath string
	opPath      string
	sessionKey  string
	opID        string
	recoveryKey string
	platform    *labeledInPlaceNewPlatform
	registry    *persistentInPlaceRegistry
	agentName   string
}

func newPersistentInPlaceHarness(t *testing.T) *persistentInPlaceHarness {
	t.Helper()
	dir := t.TempDir()
	key := "weixin:dm:peer"
	opID := NewOperationID("test", key, "m-restart")
	return &persistentInPlaceHarness{
		t:           t,
		dir:         dir,
		sessionPath: filepath.Join(dir, "sessions.json"),
		opPath:      filepath.Join(dir, "operations.json"),
		sessionKey:  key,
		opID:        opID,
		recoveryKey: "cc-connect/new/" + opID,
		platform: &labeledInPlaceNewPlatform{
			inPlaceNewPlatform: &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}},
			sessionKey:         key,
		},
		registry:  newPersistentInPlaceRegistry(),
		agentName: "persistent-in-place",
	}
}

func (h *persistentInPlaceHarness) seed(step NewOperationStep, laterActiveName string) (sourceID, candidateID, laterID string) {
	h.t.Helper()
	sessions := NewSessionManager(h.sessionPath)
	source := sessions.NewSession(h.sessionKey, "[Codex] A")
	source.SetAgentSessionID("thread-a", h.agentName)
	candidate := sessions.NewSideSession(h.sessionKey, "[Codex] B")
	candidate.SetAgentSessionID("thread-b", h.agentName)
	if step == NewOperationActivated {
		if err := sessions.ActivatePreparedSession(h.sessionKey, candidate.ID, source.ID); err != nil {
			h.t.Fatal(err)
		}
	}
	if laterActiveName != "" {
		later := sessions.NewSideSession(h.sessionKey, laterActiveName)
		later.SetAgentSessionID("thread-c", h.agentName)
		expected := source.ID
		if step == NewOperationActivated {
			expected = candidate.ID
		}
		if err := sessions.ActivatePreparedSession(h.sessionKey, later.ID, expected); err != nil {
			h.t.Fatal(err)
		}
		laterID = later.ID
	}
	if err := sessions.SaveErr(); err != nil {
		h.t.Fatal(err)
	}
	h.registry.remember(h.recoveryKey, "thread-b")
	store, err := NewNewOperationStore(h.opPath)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:                   h.opID,
		Project:              "test",
		Platform:             "weixin",
		Mode:                 NewOperationModeInPlaceSession,
		SourceSessionKey:     h.sessionKey,
		SourceLocalSessionID: source.ID,
		SourceMessageID:      "m-restart",
		UserID:               "peer",
		Name:                 "[Codex] B",
		WorkspaceDir:         "",
		Step:                 step,
		Status:               NewOperationRunning,
		TargetSessionKey:     h.sessionKey,
		LocalSessionID:       candidate.ID,
		AgentSessionID:       "thread-b",
		AgentRecoveryKey:     h.recoveryKey,
	}); err != nil {
		h.t.Fatal(err)
	}
	return source.ID, candidate.ID, laterID
}

func (h *persistentInPlaceHarness) restart() (*Engine, *NewOperationStore) {
	h.t.Helper()
	agent := &persistentInPlaceAgent{name: h.agentName, registry: h.registry}
	e := NewEngine("test", agent, []Platform{h.platform}, h.sessionPath, LangChinese)
	e.newOperationRetryDelay = time.Hour
	store, err := NewNewOperationStore(h.opPath)
	if err != nil {
		h.t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	e.OnPlatformReady(h.platform)
	h.t.Cleanup(func() { _ = e.Stop() })
	return e, store
}

func (h *persistentInPlaceHarness) waitStatus(store *NewOperationStore, status NewOperationStatus) NewOperation {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, ok := store.Get(h.opID)
		if ok && op.Status == status {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := store.Get(h.opID)
	h.t.Fatalf("operation did not reach %q: %+v", status, op)
	return NewOperation{}
}

func (h *persistentInPlaceHarness) waitNoticeContains(parts ...string) string {
	h.t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sent := strings.Join(h.platform.getSent(), "\n")
		matched := true
		for _, part := range parts {
			if !strings.Contains(sent, part) {
				matched = false
				break
			}
		}
		if matched {
			return sent
		}
		time.Sleep(time.Millisecond)
	}
	return strings.Join(h.platform.getSent(), "\n")
}

type permissionInPlaceSession struct {
	*namingAgentSession
	responded chan PermissionResult
	cancelled chan struct{}
}

type blockingPermissionInPlaceSession struct {
	*permissionInPlaceSession
	respondEntered chan struct{}
	respondRelease chan struct{}
}

type panicPermissionSession struct {
	*controllableAgentSession
}

type panicCancelSession struct {
	*controllableAgentSession
}

func (s *panicCancelSession) CancelTurn() error {
	panic("cancel turn panic")
}

func (s *panicPermissionSession) RespondPermission(string, PermissionResult) error {
	panic("permission response panic")
}

func (s *blockingPermissionInPlaceSession) RespondPermission(id string, result PermissionResult) error {
	close(s.respondEntered)
	<-s.respondRelease
	return s.permissionInPlaceSession.RespondPermission(id, result)
}

func (s *permissionInPlaceSession) RespondPermission(_ string, result PermissionResult) error {
	s.responded <- result
	return nil
}

func (s *permissionInPlaceSession) CancelTurn() error {
	select {
	case s.cancelled <- struct{}{}:
	default:
	}
	s.events <- Event{Type: EventResult, SessionID: s.sessionID, Done: true}
	return nil
}

func (a *inPlaceNewAgent) StartRecoverableSession(ctx context.Context, sessionID, recoveryKey string) (AgentSession, error) {
	a.mu.Lock()
	a.recoveryKeys = append(a.recoveryKeys, recoveryKey)
	a.mu.Unlock()
	return a.StartSession(ctx, sessionID)
}

func (a *inPlaceNewAgent) FormatConversationName(name string) string {
	a.mu.Lock()
	a.formatted = append(a.formatted, name)
	a.mu.Unlock()
	return "[Codex] " + strings.TrimSpace(name)
}

func (a *inPlaceNewAgent) keys() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.recoveryKeys...)
}

func newInPlaceNewTestEngine(t *testing.T, session AgentSession) (*Engine, *inPlaceNewPlatform, *inPlaceNewAgent, *NewOperationStore) {
	t.Helper()
	p := &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}}
	agent := &inPlaceNewAgent{controllableAgent: &controllableAgent{nextSession: session}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	return e, p, agent, store
}

func TestCmdNewInPlaceCreatesAndActivatesOneCodexSession(t *testing.T) {
	newAgentSession := &primingAgentSession{
		namingAgentSession: &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")},
	}
	e, p, agent, store := newInPlaceNewTestEngine(t, newAgentSession)
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())
	e.sessions.Save()
	oldLive := newControllableSession("thread-a")
	e.interactiveStates[key] = &interactiveState{agentSession: oldLive, platform: p, replyCtx: "ctx-a", agent: agent}

	msg := &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m1", ReplyCtx: "ctx-a"}
	e.cmdNew(p, msg, []string{"B"})

	if got := len(e.sessions.ListSessions(key)); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	active := e.sessions.GetOrCreateActive(key)
	if active.ID == a.ID || active.GetAgentSessionID() != "thread-b" || active.GetName() != "[Codex] B" {
		t.Fatalf("unexpected active session: %#v", active)
	}
	if !reflect.DeepEqual(newAgentSession.primedNames, []string{"[Codex] B"}) {
		t.Fatalf("primed names = %#v", newAgentSession.primedNames)
	}
	if !reflect.DeepEqual(newAgentSession.names, []string{"[Codex] B"}) {
		t.Fatalf("synced names = %#v", newAgentSession.names)
	}
	op, ok := store.Get(NewOperationID("test", key, "m1"))
	if !ok || op.EffectiveMode() != NewOperationModeInPlaceSession || op.Step != NewOperationCompleted || op.Status != NewOperationStatusCompleted {
		t.Fatalf("operation = %+v, ok = %v", op, ok)
	}
	if op.SourceLocalSessionID != a.ID || op.LocalSessionID != active.ID || op.TargetSessionKey != key {
		t.Fatalf("operation routing = %+v", op)
	}
	if got := agent.keys(); len(got) != 1 || got[0] != "cc-connect/new/"+op.ID {
		t.Fatalf("recovery keys = %#v", got)
	}
	e.interactiveMu.Lock()
	normalState := e.interactiveStates[key]
	_, tempExists := e.interactiveStates[inPlaceCandidateInteractiveKey(op)]
	e.interactiveMu.Unlock()
	if normalState == nil || normalState.agentSession != newAgentSession || tempExists {
		t.Fatalf("promoted state = %#v, tempExists = %v", normalState, tempExists)
	}
	select {
	case <-oldLive.closed:
	case <-time.After(time.Second):
		t.Fatal("old live process was not stopped after activation")
	}
}

func TestCmdNewInPlaceRepeatedMessageIsIdempotent(t *testing.T) {
	session := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	e, p, agent, store := newInPlaceNewTestEngine(t, session)
	const key = "weixin:dm:peer"
	e.sessions.NewSession(key, "[Codex] A")
	msg := &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "same", ReplyCtx: "ctx"}

	e.cmdNew(p, msg, []string{"B"})
	e.cmdNew(p, msg, []string{"B"})

	if got := len(e.sessions.ListSessions(key)); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	if got := agent.keys(); len(got) != 1 {
		t.Fatalf("agent starts = %d, want 1 (%#v)", len(got), got)
	}
	op, _ := store.Get(NewOperationID("test", key, "same"))
	if op.Step != NewOperationCompleted {
		t.Fatalf("operation = %+v", op)
	}
}

func TestCmdNewInPlaceNameFailureKeepsSourceActive(t *testing.T) {
	nameErr := errors.New("name RPC failed")
	session := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b"), err: nameErr}
	e, p, agent, store := newInPlaceNewTestEngine(t, session)
	e.newOperationRetryDelay = time.Hour
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())
	e.sessions.Save()
	oldLive := newControllableSession("thread-a")
	e.interactiveStates[key] = &interactiveState{agentSession: oldLive, platform: p, replyCtx: "ctx-a", agent: agent}

	e.cmdNew(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m1", ReplyCtx: "ctx-a"}, []string{"B"})

	if got := e.sessions.ActiveSessionID(key); got != a.ID {
		t.Fatalf("active = %q, want %q", got, a.ID)
	}
	if !oldLive.Alive() {
		t.Fatal("source live process was stopped after candidate name failure")
	}
	op, _ := store.Get(NewOperationID("test", key, "m1"))
	if op.Status != NewOperationRetryWait || op.Step != NewOperationAgentSessionStarted {
		t.Fatalf("operation = %+v", op)
	}
	e.interactiveMu.Lock()
	_, tempExists := e.interactiveStates[inPlaceCandidateInteractiveKey(op)]
	current := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if tempExists || current == nil || current.agentSession != oldLive {
		t.Fatalf("candidate cleanup failed: current=%#v tempExists=%v", current, tempExists)
	}
}

func TestCmdNewInPlaceDefaultNameUsesInjectedClock(t *testing.T) {
	session := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	e, p, agent, _ := newInPlaceNewTestEngine(t, session)
	e.conversationNow = func() time.Time {
		return time.Date(2026, 7, 22, 9, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	}
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())

	e.cmdNew(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-default", ReplyCtx: "ctx"}, nil)

	if got := e.sessions.GetOrCreateActive(key).GetName(); got != "[Codex] 微信新会话-0722-0930" {
		t.Fatalf("name = %q", got)
	}
}

func TestCmdNewInPlaceDefaultNameConvertsInjectedClockToBeijing(t *testing.T) {
	session := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	e, p, agent, _ := newInPlaceNewTestEngine(t, session)
	e.conversationNow = func() time.Time {
		return time.Date(2026, 7, 22, 1, 30, 0, 0, time.UTC)
	}
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())

	e.cmdNew(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-default-utc", ReplyCtx: "ctx"}, nil)

	if got := e.sessions.GetOrCreateActive(key).GetName(); got != "[Codex] 微信新会话-0722-0930" {
		t.Fatalf("name = %q, want Beijing clock", got)
	}
}

func TestInPlaceNewRestartAfterAgentStartReusesStableThread(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	sourceID, candidateID, _ := h.seed(NewOperationAgentSessionStarted, "")
	e, store := h.restart()
	op := h.waitStatus(store, NewOperationStatusCompleted)

	threads, starts := h.registry.counts()
	if threads != 1 || starts != 1 {
		t.Fatalf("durable threads = %d, recovery starts = %d, want 1 and 1", threads, starts)
	}
	if got := len(e.sessions.ListSessions(h.sessionKey)); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	if got := e.sessions.ActiveSessionID(h.sessionKey); got != candidateID {
		t.Fatalf("active = %q, want candidate %q", got, candidateID)
	}
	if op.Step != NewOperationCompleted || op.AgentRecoveryKey != h.recoveryKey || op.AgentSessionID != "thread-b" {
		t.Fatalf("completed operation = %+v", op)
	}
	back, err := e.sessions.BackSession(h.sessionKey)
	if err != nil || back.ID != sourceID {
		t.Fatalf("back = %#v, err = %v, want source %q", back, err, sourceID)
	}
	e.inPlaceTransitionMu.Lock()
	gateCount := len(e.inPlaceTransitions)
	e.inPlaceTransitionMu.Unlock()
	if gateCount != 0 {
		t.Fatalf("transition gates = %d, want 0 after recovery", gateCount)
	}
}

func TestInPlaceNewRestartAfterCandidatePersistedBeforeOperationUpdateReusesCandidate(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	sessions := NewSessionManager(h.sessionPath)
	source := sessions.NewSession(h.sessionKey, "[Codex] A")
	source.SetAgentSessionID("thread-a", h.agentName)
	candidate, err := sessions.EnsurePreparedSideSession(h.sessionKey, "[Codex] B", h.opID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewNewOperationStore(h.opPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:                   h.opID,
		Project:              "test",
		Platform:             "weixin",
		Mode:                 NewOperationModeInPlaceSession,
		SourceSessionKey:     h.sessionKey,
		SourceLocalSessionID: source.ID,
		SourceMessageID:      "m-restart",
		UserID:               "peer",
		Name:                 "[Codex] B",
		Step:                 NewOperationAccepted,
		Status:               NewOperationRunning,
		TargetSessionKey:     h.sessionKey,
		AgentRecoveryKey:     h.recoveryKey,
	}); err != nil {
		t.Fatal(err)
	}

	e, reloadedStore := h.restart()
	op := h.waitStatus(reloadedStore, NewOperationStatusCompleted)
	if got := len(e.sessions.ListSessions(h.sessionKey)); got != 2 {
		t.Fatalf("sessions = %d, want only A plus original B", got)
	}
	if op.LocalSessionID != candidate.ID || e.sessions.ActiveSessionID(h.sessionKey) != candidate.ID {
		t.Fatalf("operation local=%q active=%q, want original candidate %q", op.LocalSessionID, e.sessions.ActiveSessionID(h.sessionKey), candidate.ID)
	}
	threads, _ := h.registry.counts()
	if threads != 1 {
		t.Fatalf("durable threads = %d, want 1", threads)
	}
	back, err := e.sessions.BackSession(h.sessionKey)
	if err != nil || back.ID != source.ID {
		t.Fatalf("back = %#v, err = %v, want source %q", back, err, source.ID)
	}
}

func TestInPlaceNewOperationUpdateFailureRestartReusesPersistedCandidate(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	sessions := NewSessionManager(h.sessionPath)
	source := sessions.NewSession(h.sessionKey, "[Codex] A")
	source.SetAgentSessionID("thread-a", h.agentName)
	store, err := NewNewOperationStore(h.opPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:                   h.opID,
		Project:              "test",
		Platform:             "weixin",
		Mode:                 NewOperationModeInPlaceSession,
		SourceSessionKey:     h.sessionKey,
		SourceLocalSessionID: source.ID,
		SourceMessageID:      "m-restart",
		UserID:               "peer",
		Name:                 "[Codex] B",
		Step:                 NewOperationAccepted,
		Status:               NewOperationRunning,
		TargetSessionKey:     h.sessionKey,
		AgentRecoveryKey:     h.recoveryKey,
	}); err != nil {
		t.Fatal(err)
	}
	backupPath := h.opPath + ".before-update"
	if err := os.Rename(h.opPath, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.opPath, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := &persistentInPlaceAgent{name: h.agentName, registry: h.registry}
	e := NewEngine("test", agent, []Platform{h.platform}, h.sessionPath, LangChinese)
	e.newOperationRetryDelay = time.Hour
	e.SetNewOperationStore(store)
	e.advanceNewOperationByMode(h.platform, h.opID, "ctx", agent, e.sessions)

	persistedAfterFailure := NewSessionManager(h.sessionPath)
	list := persistedAfterFailure.ListSessions(h.sessionKey)
	if len(list) != 2 {
		t.Fatalf("sessions after failed operation update = %d, want A+B", len(list))
	}
	var originalCandidateID string
	for _, session := range list {
		if session.ID != source.ID {
			originalCandidateID = session.ID
		}
	}
	if originalCandidateID == "" {
		t.Fatal("persisted candidate B not found")
	}
	if err := os.Remove(h.opPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupPath, h.opPath); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}

	restarted, reloadedStore := h.restart()
	op := h.waitStatus(reloadedStore, NewOperationStatusCompleted)
	if got := len(restarted.sessions.ListSessions(h.sessionKey)); got != 2 {
		t.Fatalf("sessions after restart = %d, want only A plus original B", got)
	}
	if op.LocalSessionID != originalCandidateID || restarted.sessions.ActiveSessionID(h.sessionKey) != originalCandidateID {
		t.Fatalf("operation local=%q active=%q, want original candidate %q", op.LocalSessionID, restarted.sessions.ActiveSessionID(h.sessionKey), originalCandidateID)
	}
	threads, _ := h.registry.counts()
	if threads != 1 {
		t.Fatalf("durable threads = %d, want 1", threads)
	}
	back, err := restarted.sessions.BackSession(h.sessionKey)
	if err != nil || back.ID != source.ID {
		t.Fatalf("back = %#v, err = %v, want source %q", back, err, source.ID)
	}
}

func TestInPlaceNewRestartAfterActivationDoesNotPushBackTwice(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	sourceID, candidateID, _ := h.seed(NewOperationActivated, "")
	e, store := h.restart()
	op := h.waitStatus(store, NewOperationStatusCompleted)

	threads, starts := h.registry.counts()
	if threads != 1 || starts != 0 {
		t.Fatalf("durable threads = %d, recovery starts = %d, want 1 and 0", threads, starts)
	}
	if got := len(e.sessions.ListSessions(h.sessionKey)); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	if got := e.sessions.ActiveSessionID(h.sessionKey); got != candidateID {
		t.Fatalf("active = %q, want candidate %q", got, candidateID)
	}
	if !op.SourceNotified || op.Step != NewOperationCompleted {
		t.Fatalf("operation = %+v, want notified completed", op)
	}
	back, err := e.sessions.BackSession(h.sessionKey)
	if err != nil || back.ID != sourceID {
		t.Fatalf("first back = %#v, err = %v, want source %q", back, err, sourceID)
	}
	if _, err := e.sessions.BackSession(h.sessionKey); !errors.Is(err, ErrNoPreviousSession) {
		t.Fatalf("second back error = %v, want ErrNoPreviousSession", err)
	}
	if got := len(h.platform.getSent()); got != 1 {
		t.Fatalf("completion notices = %d, want 1", got)
	}
}

func TestInPlaceNewRestartAtNameSyncedRestoresCandidateBeforeActivation(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	sourceID, candidateID, _ := h.seed(NewOperationNameSynced, "")
	e, store := h.restart()
	op := h.waitStatus(store, NewOperationStatusCompleted)

	threads, starts := h.registry.counts()
	if threads != 1 || starts != 1 {
		t.Fatalf("durable threads = %d, recovery starts = %d, want 1 and 1", threads, starts)
	}
	if got := e.sessions.ActiveSessionID(h.sessionKey); got != candidateID {
		t.Fatalf("active = %q, want candidate %q", got, candidateID)
	}
	if op.Step != NewOperationCompleted {
		t.Fatalf("operation = %+v", op)
	}
	back, err := e.sessions.BackSession(h.sessionKey)
	if err != nil || back.ID != sourceID {
		t.Fatalf("back = %#v, err = %v, want source %q", back, err, sourceID)
	}
}

func TestInPlaceNewRestartAfterDurableActivationBeforeStepUpdateDoesNotPushBackTwice(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	sourceID, candidateID, _ := h.seed(NewOperationNameSynced, "")
	preCrash := NewSessionManager(h.sessionPath)
	if err := preCrash.ActivatePreparedSession(h.sessionKey, candidateID, sourceID); err != nil {
		t.Fatal(err)
	}
	e, store := h.restart()
	op := h.waitStatus(store, NewOperationStatusCompleted)

	if got := e.sessions.ActiveSessionID(h.sessionKey); got != candidateID {
		t.Fatalf("active = %q, want already durable candidate %q", got, candidateID)
	}
	if op.Step != NewOperationCompleted {
		t.Fatalf("operation = %+v", op)
	}
	back, err := e.sessions.BackSession(h.sessionKey)
	if err != nil || back.ID != sourceID {
		t.Fatalf("first back = %#v, err = %v, want source %q", back, err, sourceID)
	}
	if _, err := e.sessions.BackSession(h.sessionKey); !errors.Is(err, ErrNoPreviousSession) {
		t.Fatalf("second back error = %v, want ErrNoPreviousSession", err)
	}
}

func TestInPlaceNewActivatedRecoveryDoesNotOverrideLaterUserSwitch(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	_, candidateID, laterID := h.seed(NewOperationActivated, "[Codex] C")
	e, store := h.restart()
	op := h.waitStatus(store, NewOperationManualAction)

	if got := e.sessions.ActiveSessionID(h.sessionKey); got != laterID {
		t.Fatalf("active = %q, want later user choice %q", got, laterID)
	}
	if e.sessions.FindByID(candidateID) == nil {
		t.Fatal("activated B session was not retained")
	}
	if op.Step != NewOperationActivated || op.Status != NewOperationManualAction {
		t.Fatalf("operation = %+v, want activated manual_action_required", op)
	}
	if sent := h.waitNoticeContains("/switch"); !strings.HasPrefix(sent, "[C] ") {
		t.Fatalf("manual recovery notice = %q, want labeled explicit /switch guidance", sent)
	}
}

func TestInPlaceNewRecoveryDoesNotOverrideLaterUserSwitch(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	sourceID, candidateID, laterID := h.seed(NewOperationAgentSessionStarted, "[Codex] C")
	e, store := h.restart()
	op := h.waitStatus(store, NewOperationManualAction)

	threads, starts := h.registry.counts()
	if threads != 1 || starts != 1 {
		t.Fatalf("durable threads = %d, recovery starts = %d, want 1 and 1", threads, starts)
	}
	if got := len(e.sessions.ListSessions(h.sessionKey)); got != 3 {
		t.Fatalf("sessions = %d, want A/B/C", got)
	}
	if got := e.sessions.ActiveSessionID(h.sessionKey); got != laterID {
		t.Fatalf("active = %q, want later user choice %q", got, laterID)
	}
	if e.sessions.FindByID(candidateID) == nil {
		t.Fatal("prepared B candidate was deleted instead of retained")
	}
	if op.Step != NewOperationNameSynced || op.Status != NewOperationManualAction {
		t.Fatalf("operation = %+v, want name_synced manual_action_required", op)
	}
	if sent := h.waitNoticeContains("/switch"); !strings.HasPrefix(sent, "[C] ") {
		t.Fatalf("manual recovery notice = %q, want labeled explicit /switch guidance", sent)
	}
	back, err := e.sessions.BackSession(h.sessionKey)
	if err != nil || back.ID != sourceID {
		t.Fatalf("back after protected recovery = %#v, err = %v, want source %q", back, err, sourceID)
	}
	e.inPlaceTransitionMu.Lock()
	gateCount := len(e.inPlaceTransitions)
	e.inPlaceTransitionMu.Unlock()
	if gateCount != 0 {
		t.Fatalf("transition gates = %d, want 0 after manual recovery", gateCount)
	}
	beforeNotices := len(h.platform.getSent())
	e.recoverNewOperationsForPlatform(h.platform)
	again, _ := store.Get(h.opID)
	if again.Status != NewOperationManualAction || again.StepAttempts != op.StepAttempts {
		t.Fatalf("manual operation was automatically reactivated: before=%+v after=%+v", op, again)
	}
	if got := len(h.platform.getSent()); got != beforeNotices {
		t.Fatalf("manual recovery notice repeated on startup scan: got %d, want %d", got, beforeNotices)
	}
}

func TestInPlaceNewRecoveryRebuildsAndReleasesTransitionGate(t *testing.T) {
	h := newPersistentInPlaceHarness(t)
	h.seed(NewOperationAgentSessionStarted, "")
	started := make(chan struct{})
	release := make(chan struct{})
	h.registry.mu.Lock()
	h.registry.nameStarted = started
	h.registry.nameRelease = release
	h.registry.mu.Unlock()
	e, store := h.restart()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("recovery did not reach name synchronization")
	}
	e.inPlaceTransitionMu.Lock()
	gate := e.inPlaceTransitions[h.sessionKey]
	e.inPlaceTransitionMu.Unlock()
	if gate == nil {
		close(release)
		t.Fatal("background recovery did not rebuild the in-place transition gate")
	}
	close(release)
	h.waitStatus(store, NewOperationStatusCompleted)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.inPlaceTransitionMu.Lock()
		gateCount := len(e.inPlaceTransitions)
		e.inPlaceTransitionMu.Unlock()
		if gateCount == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background recovery left transition admission closed")
}

func TestInPlaceNewRecoveryRestoresOriginalWorkspaceContext(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace-a")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := newPersistentInPlaceRegistry()
	agentName := fmt.Sprintf("persistent-in-place-workspace-%d", time.Now().UnixNano())
	RegisterAgent(agentName, func(map[string]any) (Agent, error) {
		return &persistentInPlaceAgent{name: agentName, registry: registry}, nil
	})
	platform := &labeledInPlaceNewPlatform{
		inPlaceNewPlatform: &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}},
		sessionKey:         "weixin:dm:peer",
	}
	sessionPath := filepath.Join(dir, "sessions.json")
	opPath := filepath.Join(dir, "operations.json")
	bindingPath := filepath.Join(dir, "bindings.json")
	first := NewEngine("test", &persistentInPlaceAgent{name: agentName, registry: registry}, []Platform{platform}, sessionPath, LangChinese)
	first.SetMultiWorkspace(dir, bindingPath)
	_, workspaceSessions, err := first.getOrCreateWorkspaceAgent(workspace)
	if err != nil {
		t.Fatal(err)
	}
	const key = "weixin:dm:peer"
	source := workspaceSessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agentName)
	candidate := workspaceSessions.NewSideSession(key, "[Codex] B")
	candidate.SetAgentSessionID("thread-b", agentName)
	if err := workspaceSessions.SaveErr(); err != nil {
		t.Fatal(err)
	}
	opID := NewOperationID("test", key, "m-workspace-restart")
	recoveryKey := "cc-connect/new/" + opID
	registry.remember(recoveryKey, "thread-b")
	store, err := NewNewOperationStore(opPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:                   opID,
		Project:              "test",
		Platform:             "weixin",
		Mode:                 NewOperationModeInPlaceSession,
		SourceSessionKey:     key,
		SourceLocalSessionID: source.ID,
		SourceMessageID:      "m-workspace-restart",
		UserID:               "peer",
		Name:                 "[Codex] B",
		WorkspaceDir:         workspace,
		Step:                 NewOperationAgentSessionStarted,
		Status:               NewOperationRunning,
		TargetSessionKey:     key,
		LocalSessionID:       candidate.ID,
		AgentSessionID:       "thread-b",
		AgentRecoveryKey:     recoveryKey,
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Stop(); err != nil {
		t.Fatal(err)
	}

	restarted := NewEngine("test", &persistentInPlaceAgent{name: agentName, registry: registry}, []Platform{platform}, sessionPath, LangChinese)
	restarted.SetMultiWorkspace(dir, bindingPath)
	reloadedStore, err := NewNewOperationStore(opPath)
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetNewOperationStore(reloadedStore)
	restarted.OnPlatformReady(platform)
	t.Cleanup(func() { _ = restarted.Stop() })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, _ := reloadedStore.Get(opID)
		if op.Status == NewOperationStatusCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := reloadedStore.Get(opID)
	if op.Status != NewOperationStatusCompleted || op.WorkspaceDir != workspace {
		t.Fatalf("operation = %+v, want completed in original workspace", op)
	}
	if got := len(restarted.sessions.ListSessions(key)); got != 0 {
		t.Fatalf("global sessions = %d, want 0", got)
	}
	_, restoredSessions, err := restarted.getOrCreateWorkspaceAgent(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(restoredSessions.ListSessions(key)); got != 2 {
		t.Fatalf("workspace sessions = %d, want 2", got)
	}
	if got := restoredSessions.ActiveSessionID(key); got != candidate.ID {
		t.Fatalf("workspace active = %q, want candidate %q", got, candidate.ID)
	}
	threads, starts := registry.counts()
	if threads != 1 || starts != 1 {
		t.Fatalf("durable threads = %d, recovery starts = %d, want 1 and 1", threads, starts)
	}
	back, err := restoredSessions.BackSession(key)
	if err != nil || back.ID != source.ID {
		t.Fatalf("workspace back = %#v, err = %v, want source %q", back, err, source.ID)
	}
}

func TestCmdNewInPlaceDoesNotUseExternalConversationSpawner(t *testing.T) {
	// The existing Feishu regression tests exercise ConversationSpawner. This
	// assertion locks the capability split: an in-place platform never needs a
	// synthetic external target to create its new Codex session.
	session := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	e, p, agent, _ := newInPlaceNewTestEngine(t, session)
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())

	e.cmdNew(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-cap", ReplyCtx: "ctx"}, []string{"B"})

	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "thread-b" {
		t.Fatalf("active agent session = %q, want thread-b", got)
	}
}

func TestNewOperationRecoveryDispatchesInPlaceWithoutConversationSpawner(t *testing.T) {
	session := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	e, p, agent, store := newInPlaceNewTestEngine(t, session)
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())
	e.sessions.Save()
	opID := NewOperationID("test", key, "m-recovery")
	if _, _, err := store.CreateOrGet(NewOperation{
		ID:                   opID,
		Project:              "test",
		Platform:             "weixin",
		Mode:                 NewOperationModeInPlaceSession,
		SourceSessionKey:     key,
		SourceLocalSessionID: a.ID,
		SourceMessageID:      "m-recovery",
		UserID:               "peer",
		Name:                 "[Codex] 恢复",
		Step:                 NewOperationAccepted,
		Status:               NewOperationRunning,
		TargetSessionKey:     key,
		AgentRecoveryKey:     "cc-connect/new/" + opID,
	}); err != nil {
		t.Fatal(err)
	}

	e.advanceNewOperationByMode(p, opID, "ctx", agent, e.sessions)

	op, _ := store.Get(opID)
	if op.Step != NewOperationCompleted || op.Status != NewOperationStatusCompleted {
		t.Fatalf("operation = %+v", op)
	}
	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "thread-b" {
		t.Fatalf("active agent session = %q, want thread-b", got)
	}
}

func TestCmdNewInPlaceWaitsForSourceTurnBeforeRetiringLiveState(t *testing.T) {
	newAgentSession := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	e, p, agent, store := newInPlaceNewTestEngine(t, newAgentSession)
	e.newOperationRetryDelay = time.Hour
	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	e.sessions.Save()

	oldLive := newControllableSession("thread-a")
	oldLive.sendPrompts = make(chan string, 1)
	e.interactiveStates[key] = &interactiveState{
		agentSession: oldLive,
		platform:     p,
		replyCtx:     "ctx-a",
		agent:        agent,
	}
	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: "weixin", UserID: "peer", UserName: "用户",
		MessageID: "turn-a", Content: "A 正在处理", ReplyCtx: "ctx-a",
	})
	select {
	case <-oldLive.sendPrompts:
	case <-time.After(time.Second):
		t.Fatal("source turn did not start")
	}

	e.cmdNew(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "new-during-a", ReplyCtx: "ctx-new"}, []string{"B"})

	if got := e.sessions.ActiveSessionID(key); got != source.ID {
		t.Fatalf("active changed while source turn was running: %q", got)
	}
	e.interactiveMu.Lock()
	current := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if current == nil || current.agentSession != oldLive || !oldLive.Alive() {
		t.Fatalf("source live state retired during its turn: %#v", current)
	}
	opID := NewOperationID("test", key, "new-during-a")
	op, _ := store.Get(opID)
	if op.Step != NewOperationNameSynced || op.Status != NewOperationRetryWait {
		t.Fatalf("operation = %+v, want name_synced retry wait", op)
	}

	oldLive.events <- Event{Type: EventText, Content: "A 完成回复"}
	oldLive.events <- Event{Type: EventResult, SessionID: "thread-a", Done: true}
	deadline := time.Now().Add(time.Second)
	for source.Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if source.Busy() {
		t.Fatal("source turn did not unlock")
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "A 完成回复") {
		t.Fatalf("source reply was lost before switch: %q", got)
	}

	e.advanceNewOperationByMode(p, opID, "ctx-new", agent, e.sessions)
	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "thread-b" {
		t.Fatalf("active agent session = %q, want thread-b", got)
	}
	select {
	case <-oldLive.closed:
	case <-time.After(time.Second):
		t.Fatal("source live state was not retired after its turn completed")
	}
}

func TestCmdNewInPlaceActivatedReplyFailureRetriesNotificationWithoutRollback(t *testing.T) {
	base := &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}}
	p := &failingActivatedReplyPlatform{inPlaceNewPlatform: base, replyErr: errors.New("network unavailable")}
	session := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	agent := &inPlaceNewAgent{controllableAgent: &controllableAgent{nextSession: session}}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.newOperationRetryDelay = time.Hour
	store, err := NewNewOperationStore(filepath.Join(t.TempDir(), "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())
	e.sessions.Save()

	e.cmdNew(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "reply-fails", ReplyCtx: "ctx"}, []string{"B"})

	active := e.sessions.GetOrCreateActive(key)
	if active.ID == a.ID || active.GetAgentSessionID() != "thread-b" {
		t.Fatalf("activation rolled back after notification failure: %#v", active)
	}
	opID := NewOperationID("test", key, "reply-fails")
	op, _ := store.Get(opID)
	if op.Step != NewOperationActivated || op.Status != NewOperationRetryWait || op.SourceNotified {
		t.Fatalf("operation = %+v, want activated notification retry", op)
	}
	if got := strings.Join(p.getSent(), "\n"); strings.Contains(got, "当前会话保持不变") || strings.Contains(got, "需要检查权限或配置") {
		t.Fatalf("misleading creation failure was sent after activation: %q", got)
	}

	e.advanceNewOperationByMode(p, opID, "ctx", agent, e.sessions)
	op, _ = store.Get(opID)
	if op.Step != NewOperationCompleted || op.Status != NewOperationStatusCompleted || !op.SourceNotified {
		t.Fatalf("notification retry did not complete operation: %+v", op)
	}
	if got := e.sessions.GetOrCreateActive(key).ID; got != active.ID {
		t.Fatalf("active changed during notification retry: %q, want %q", got, active.ID)
	}
}

func waitInPlaceTestSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitInPlaceTestPrompt(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if !strings.Contains(got, want) {
			t.Fatalf("prompt = %q, want content %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for prompt %q", want)
	}
}

func waitInPlaceTestSessionIdle(t *testing.T, session *Session) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			if session.TryLock() {
				session.UnlockWithoutUpdate()
				close(done)
				return
			}
			runtime.Gosched()
		}
	}()
	waitInPlaceTestSignal(t, done, "session idle")
}

func waitInPlaceTestPendingPermission(t *testing.T, e *Engine, key string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		_, pending := e.lookupPending(key)
		if pending != nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for pending permission")
		default:
			runtime.Gosched()
		}
	}
}

func TestInPlaceNewQueuesFirstMessageForNewSession(t *testing.T) {
	nameStarted := make(chan struct{}, 1)
	nameRelease := make(chan struct{})
	candidateLive := &namingAgentSession{
		controllableAgentSession: newControllableSession("thread-b"),
		nameStarted:              nameStarted,
		nameRelease:              nameRelease,
	}
	candidateLive.sendPrompts = make(chan string, 3)
	e, p, agent, _ := newInPlaceNewTestEngine(t, candidateLive)
	e.newOperationRetryDelay = time.Hour
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	if err := e.sessions.SaveErr(); err != nil {
		t.Fatal(err)
	}
	e.sessions.storePath = ""
	sourceLive := newControllableSession("thread-a")
	sourceLive.sendPrompts = make(chan string, 3)
	e.interactiveStates[key] = &interactiveState{agentSession: sourceLive, platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, nameStarted, "candidate name sync")

	for i, content := range []string{"first-B", "second-B", "third-B"} {
		e.ReceiveMessage(p, &Message{
			SessionKey: key, Platform: "weixin", UserID: "peer",
			MessageID: fmt.Sprintf("m-%d", i), Content: content, ReplyCtx: fmt.Sprintf("ctx-%d", i),
		})
	}
	close(nameRelease)

	for _, content := range []string{"first-B", "second-B", "third-B"} {
		waitInPlaceTestPrompt(t, candidateLive.sendPrompts, content)
		candidateLive.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	}
	waitInPlaceTestSignal(t, newDone, "/new completion")
	waitInPlaceTestSessionIdle(t, e.sessions.GetOrCreateActive(key))
	select {
	case got := <-sourceLive.sendPrompts:
		t.Fatalf("source thread received queued message %q", got)
	default:
	}
	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "thread-b" {
		t.Fatalf("active agent session = %q, want thread-b", got)
	}
}

func TestInPlaceTransitionAllowsActiveTurnPermissionResponse(t *testing.T) {
	nameStarted := make(chan struct{}, 1)
	nameRelease := make(chan struct{})
	candidateLive := &permissionInPlaceSession{
		namingAgentSession: &namingAgentSession{
			controllableAgentSession: newControllableSession("thread-b"),
			nameStarted:              nameStarted,
			nameRelease:              nameRelease,
		},
		responded: make(chan PermissionResult, 1),
		cancelled: make(chan struct{}, 1),
	}
	candidateLive.sendPrompts = make(chan string, 1)
	e, p, agent, _ := newInPlaceNewTestEngine(t, candidateLive)
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	e.interactiveStates[key] = &interactiveState{agentSession: newControllableSession("thread-a"), platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-permission", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, nameStarted, "candidate name sync")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-needs-permission", Content: "needs-permission", ReplyCtx: "ctx-turn"})
	close(nameRelease)
	waitInPlaceTestPrompt(t, candidateLive.sendPrompts, "needs-permission")
	candidateLive.events <- Event{
		Type:         EventPermissionRequest,
		RequestID:    "req-b",
		ToolName:     "Bash",
		ToolInputRaw: map[string]any{"command": "pwd"},
	}
	waitInPlaceTestPendingPermission(t, e, key)

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-allow-b", Content: "允许", ReplyCtx: "ctx-allow"})
	select {
	case result := <-candidateLive.responded:
		if result.Behavior != "allow" {
			t.Fatalf("permission behavior = %q, want allow", result.Behavior)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("permission response was queued behind the active turn")
	}
	candidateLive.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	waitInPlaceTestSignal(t, newDone, "/new completion after permission")
}

func TestInPlaceTransitionAllowsOnlyOnePermissionBypassAtATime(t *testing.T) {
	nameStarted := make(chan struct{}, 1)
	nameRelease := make(chan struct{})
	basePermission := &permissionInPlaceSession{
		namingAgentSession: &namingAgentSession{
			controllableAgentSession: newControllableSession("thread-b"),
			nameStarted:              nameStarted,
			nameRelease:              nameRelease,
		},
		responded: make(chan PermissionResult, 1),
		cancelled: make(chan struct{}, 1),
	}
	candidateLive := &blockingPermissionInPlaceSession{
		permissionInPlaceSession: basePermission,
		respondEntered:           make(chan struct{}),
		respondRelease:           make(chan struct{}),
	}
	candidateLive.sendPrompts = make(chan string, 2)
	e, p, agent, _ := newInPlaceNewTestEngine(t, candidateLive)
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	e.interactiveStates[key] = &interactiveState{agentSession: newControllableSession("thread-a"), platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-single-permission", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, nameStarted, "candidate name sync")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-permission-turn", Content: "permission-turn", ReplyCtx: "ctx-turn"})
	close(nameRelease)
	waitInPlaceTestPrompt(t, candidateLive.sendPrompts, "permission-turn")
	candidateLive.events <- Event{
		Type:         EventPermissionRequest,
		RequestID:    "req-single",
		ToolName:     "Bash",
		ToolInputRaw: map[string]any{"command": "pwd"},
	}
	waitInPlaceTestPendingPermission(t, e, key)

	allowDone := make(chan struct{})
	allowAdmission := make(chan struct{})
	go func() {
		defer close(allowDone)
		e.ReceiveMessage(p, &Message{
			SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-allow-single",
			Content: "允许", ReplyCtx: "ctx-allow", DispatchAdmission: allowAdmission,
		})
	}()
	waitInPlaceTestSignal(t, candidateLive.respondEntered, "blocked permission response")
	select {
	case <-allowAdmission:
		t.Fatal("permission admission was released before handlePendingPermission completed")
	default:
	}

	ordinaryDone := make(chan struct{})
	go func() {
		defer close(ordinaryDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-after-allow", Content: "ordinary-after-allow", ReplyCtx: "ctx-ordinary"})
	}()
	waitInPlaceTestSignal(t, ordinaryDone, "ordinary message admission")
	select {
	case got := <-candidateLive.sendPrompts:
		t.Fatalf("ordinary message bypassed active permission control: %q", got)
	default:
	}

	close(candidateLive.respondRelease)
	waitInPlaceTestSignal(t, allowDone, "permission response completion")
	waitInPlaceTestSignal(t, allowAdmission, "permission core admission")
	select {
	case result := <-candidateLive.responded:
		if result.Behavior != "allow" {
			t.Fatalf("permission behavior = %q, want allow", result.Behavior)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("permission response did not complete")
	}
	candidateLive.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	waitInPlaceTestPrompt(t, candidateLive.sendPrompts, "ordinary-after-allow")
	candidateLive.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	waitInPlaceTestSignal(t, newDone, "/new completion after serialized permission")
}

func TestInPlacePermissionControlAlwaysReleasesAdmission(t *testing.T) {
	for _, tt := range []struct {
		name      string
		content   string
		configure func(*Engine, *interactiveState)
		wantPanic bool
	}{
		{
			name:    "early banned-word return",
			content: "forbidden",
			configure: func(e *Engine, _ *interactiveState) {
				e.SetBannedWords([]string{"forbidden"})
			},
		},
		{
			name:    "permission handler panic",
			content: "允许",
			configure: func(_ *Engine, state *interactiveState) {
				state.agentSession = &panicPermissionSession{controllableAgentSession: newControllableSession("thread-b")}
			},
			wantPanic: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}}
			e := NewEngine("test", &controllableAgent{}, []Platform{p}, "", LangChinese)
			const key = "weixin:dm:peer"
			transition := &inPlaceTransition{activeMessageID: "active-turn"}
			e.inPlaceTransitions[key] = transition
			state := &interactiveState{
				agentSession:     newControllableSession("thread-b"),
				currentMessageID: "active-turn",
				pending: &pendingPermission{
					RequestID: "req-release",
					ToolName:  "Bash",
					ToolInput: map[string]any{"command": "pwd"},
					Resolved:  make(chan struct{}),
				},
			}
			e.interactiveStates[key] = state
			tt.configure(e, state)

			admitted := make(chan struct{})
			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				e.ReceiveMessage(p, &Message{
					SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-control-release",
					Content: tt.content, ReplyCtx: "ctx", DispatchAdmission: admitted,
				})
			}()

			select {
			case recovered := <-done:
				if tt.wantPanic && recovered == nil {
					t.Fatal("expected permission handler panic")
				}
				if !tt.wantPanic && recovered != nil {
					t.Fatalf("unexpected panic: %v", recovered)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("permission control path did not return")
			}
			waitInPlaceTestSignal(t, admitted, "permission admission release")
			transition.mu.Lock()
			occupied := transition.permissionControlActive
			transition.mu.Unlock()
			if occupied {
				t.Fatal("permission control occupancy remained set")
			}
		})
	}
}

func TestInPlaceTransitionAllowsStopForActiveTurn(t *testing.T) {
	nameStarted := make(chan struct{}, 1)
	nameRelease := make(chan struct{})
	candidateLive := &permissionInPlaceSession{
		namingAgentSession: &namingAgentSession{
			controllableAgentSession: newControllableSession("thread-b"),
			nameStarted:              nameStarted,
			nameRelease:              nameRelease,
		},
		responded: make(chan PermissionResult, 1),
		cancelled: make(chan struct{}, 1),
	}
	candidateLive.sendPrompts = make(chan string, 1)
	e, p, agent, _ := newInPlaceNewTestEngine(t, candidateLive)
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	e.interactiveStates[key] = &interactiveState{agentSession: newControllableSession("thread-a"), platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-stop", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, nameStarted, "candidate name sync")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-long-b", Content: "long-running", ReplyCtx: "ctx-turn"})
	close(nameRelease)
	waitInPlaceTestPrompt(t, candidateLive.sendPrompts, "long-running")

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-stop-b", Content: "/stop", ReplyCtx: "ctx-stop"})
	select {
	case <-candidateLive.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("/stop was queued behind the active turn")
	}
	waitInPlaceTestSignal(t, newDone, "/new completion after /stop")
}

func TestInPlaceTransitionHoldsStopControlUntilCommandReturns(t *testing.T) {
	nameStarted := make(chan struct{}, 1)
	nameRelease := make(chan struct{})
	candidateLive := &permissionInPlaceSession{
		namingAgentSession: &namingAgentSession{
			controllableAgentSession: newControllableSession("thread-b"),
			nameStarted:              nameStarted,
			nameRelease:              nameRelease,
		},
		responded: make(chan PermissionResult, 1),
		cancelled: make(chan struct{}, 1),
	}
	candidateLive.sendPrompts = make(chan string, 2)
	e, p, agent, _ := newInPlaceNewTestEngine(t, candidateLive)
	t.Cleanup(func() { _ = e.Stop() })

	stopEntered := make(chan struct{}, 1)
	stopRelease := make(chan struct{})
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event HookEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode hook event: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if event.Content == "/stop" {
			stopEntered <- struct{}{}
			<-stopRelease
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()
	async := false
	e.SetHooks(NewHookManager("test", []HookConfig{{
		Event: string(HookEventMessageReceived), Type: string(HookHandlerHTTP),
		URL: hookServer.URL, Async: &async,
	}}, "", "", ""))

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	e.interactiveStates[key] = &interactiveState{agentSession: newControllableSession("thread-a"), platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-stop-control", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, nameStarted, "candidate name sync")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-stop-control-turn", Content: "stop-control-turn", ReplyCtx: "ctx-turn"})
	close(nameRelease)
	waitInPlaceTestPrompt(t, candidateLive.sendPrompts, "stop-control-turn")
	candidateLive.events <- Event{
		Type:         EventPermissionRequest,
		RequestID:    "req-stop-control",
		ToolName:     "Bash",
		ToolInputRaw: map[string]any{"command": "pwd"},
	}
	waitInPlaceTestPendingPermission(t, e, key)

	stopAdmission := make(chan struct{})
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		e.ReceiveMessage(p, &Message{
			SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-stop-control",
			Content: "/stop", ReplyCtx: "ctx-stop", DispatchAdmission: stopAdmission,
		})
	}()
	waitInPlaceTestSignal(t, stopEntered, "blocked /stop command")
	select {
	case <-stopAdmission:
		close(stopRelease)
		t.Fatal("/stop admission released before cmdStop returned")
	default:
	}

	ordinaryDone := make(chan struct{})
	go func() {
		defer close(ordinaryDone)
		e.ReceiveMessage(p, &Message{
			SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-after-stop-control",
			Content: "ordinary-after-stop", ReplyCtx: "ctx-ordinary",
		})
	}()
	waitInPlaceTestSignal(t, ordinaryDone, "ordinary message queued behind /stop")
	close(stopRelease)
	waitInPlaceTestSignal(t, stopDone, "/stop command completion")
	waitInPlaceTestSignal(t, stopAdmission, "/stop core admission")
	waitInPlaceTestPrompt(t, candidateLive.sendPrompts, "ordinary-after-stop")
	candidateLive.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	waitInPlaceTestSignal(t, newDone, "/new completion after /stop control")
}

func TestInPlaceStopControlRemainsInterruptibleAndExclusive(t *testing.T) {
	e := newTestEngine()
	p := &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}}
	const key = "weixin:dm:peer"
	transition := &inPlaceTransition{activeMessageID: "active-turn"}
	e.inPlaceTransitions[key] = transition
	e.interactiveStates[key] = &interactiveState{
		agentSession:     newControllableSession("thread-b"),
		currentMessageID: "active-turn",
		pending: &pendingPermission{
			RequestID: "req-control",
			ToolName:  "Bash",
			Resolved:  make(chan struct{}),
		},
	}

	permission := &Message{SessionKey: key, MessageID: "m-permission", Content: "允许"}
	if owned, queued := e.prepareInPlaceDispatch(p, permission, nil); owned != nil || queued || permission.inPlaceControlKind != inPlaceControlPermission {
		t.Fatalf("permission control admission: owned=%v queued=%v kind=%v", owned != nil, queued, permission.inPlaceControlKind)
	}
	stop := &Message{SessionKey: key, MessageID: "m-stop", Content: "/stop"}
	if owned, queued := e.prepareInPlaceDispatch(p, stop, nil); owned != nil || queued || stop.inPlaceControlKind != inPlaceControlStop {
		t.Fatalf("stop control admission: owned=%v queued=%v kind=%v", owned != nil, queued, stop.inPlaceControlKind)
	}
	ordinary := &Message{SessionKey: key, MessageID: "m-ordinary", Content: "ordinary"}
	if owned, queued := e.prepareInPlaceDispatch(p, ordinary, nil); owned != nil || !queued {
		t.Fatalf("ordinary admission while controls active: owned=%v queued=%v", owned != nil, queued)
	}

	e.finishInPlaceControl(permission)
	e.finishInPlaceControl(stop)
	transition.mu.Lock()
	permissionActive := transition.permissionControlActive
	stopActive := transition.stopControlActive
	transition.mu.Unlock()
	if permissionActive || stopActive {
		t.Fatalf("control slots after release: permission=%v stop=%v", permissionActive, stopActive)
	}
}

func TestInPlaceStopControlAlwaysReleasesAdmission(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*Engine, *interactiveState)
		wantPanic bool
	}{
		{
			name: "disabled command early return",
			configure: func(e *Engine, _ *interactiveState) {
				e.SetDisabledCommands([]string{"stop"})
			},
		},
		{
			name: "cancel panic",
			configure: func(_ *Engine, state *interactiveState) {
				state.agentSession = &panicCancelSession{controllableAgentSession: newControllableSession("thread-b")}
			},
			wantPanic: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}}
			e := NewEngine("test", &controllableAgent{}, []Platform{p}, "", LangChinese)
			const key = "weixin:dm:peer"
			transition := &inPlaceTransition{activeMessageID: "active-turn"}
			e.inPlaceTransitions[key] = transition
			state := &interactiveState{agentSession: newControllableSession("thread-b"), currentMessageID: "active-turn"}
			e.interactiveStates[key] = state
			tt.configure(e, state)

			admitted := make(chan struct{})
			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				e.ReceiveMessage(p, &Message{
					SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-stop-release",
					Content: "/stop", ReplyCtx: "ctx", DispatchAdmission: admitted,
				})
			}()

			select {
			case recovered := <-done:
				if tt.wantPanic && recovered == nil {
					t.Fatal("expected /stop panic")
				}
				if !tt.wantPanic && recovered != nil {
					t.Fatalf("unexpected panic: %v", recovered)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("/stop control path did not return")
			}
			waitInPlaceTestSignal(t, admitted, "/stop admission release")
			transition.mu.Lock()
			occupied := transition.stopControlActive
			transition.mu.Unlock()
			if occupied {
				t.Fatal("stop control occupancy remained set")
			}
		})
	}
}

func TestInPlaceNewRegistersGateAtomicallyWithFirstDispatch(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })
	base := &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}}
	p := &blockingCapabilityInPlacePlatform{
		inPlaceNewPlatform: base,
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	candidateLive := &namingAgentSession{controllableAgentSession: newControllableSession("thread-b")}
	candidateLive.sendPrompts = make(chan string, 1)
	agent := &inPlaceNewAgent{controllableAgent: &controllableAgent{nextSession: candidateLive}}
	e := NewEngine("test", agent, []Platform{p}, "", LangChinese)
	store, err := NewNewOperationStore("")
	if err != nil {
		t.Fatal(err)
	}
	e.SetNewOperationStore(store)
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	sourceLive := newControllableSession("thread-a")
	sourceLive.sendPrompts = make(chan string, 1)
	e.interactiveStates[key] = &interactiveState{agentSession: sourceLive, platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-atomic", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, p.entered, "in-place capability dispatch")
	messageDone := make(chan struct{})
	go func() {
		defer close(messageDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-first-atomic", Content: "first-B", ReplyCtx: "ctx-first"})
	}()
	// With one P, yielding here forces the concurrent message to either finish
	// the old ungated dispatch path or block behind the atomic transition lock.
	runtime.Gosched()
	close(p.release)
	waitInPlaceTestSignal(t, messageDone, "concurrent message dispatch")
	waitInPlaceTestPrompt(t, candidateLive.sendPrompts, "first-B")
	candidateLive.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	waitInPlaceTestSignal(t, newDone, "atomic /new completion")
	waitInPlaceTestSessionIdle(t, e.sessions.GetOrCreateActive(key))
	select {
	case got := <-sourceLive.sendPrompts:
		t.Fatalf("source thread received concurrent first message %q", got)
	default:
	}
}

func TestInPlaceBackRegistersGateBeforeAdmission(t *testing.T) {
	p := &secondCapabilityCallBlockingPlatform{
		inPlaceNewPlatform: &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}},
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	aLive := newControllableSession("thread-a")
	aLive.sendPrompts = make(chan string, 1)
	bLive := newControllableSession("thread-b")
	bLive.sendPrompts = make(chan string, 1)
	agent := &inPlaceNewAgent{controllableAgent: &controllableAgent{}}
	agent.startSessionFn = func(_ context.Context, sessionID string) (AgentSession, error) {
		if sessionID != "thread-a" {
			return nil, fmt.Errorf("resumed session = %q, want thread-a", sessionID)
		}
		return aLive, nil
	}
	e := NewEngine("test", agent, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())
	b := e.sessions.NewSideSession(key, "[Codex] B")
	b.SetAgentSessionID("thread-b", agent.Name())
	if err := e.sessions.ActivatePreparedSession(key, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	e.interactiveStates[key] = &interactiveState{agentSession: bLive, platform: p, replyCtx: "ctx-b", agent: agent}

	admitted := make(chan struct{})
	backDone := make(chan struct{})
	go func() {
		defer close(backDone)
		e.ReceiveMessage(p, &Message{
			SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-back-atomic",
			Content: "/back", ReplyCtx: "ctx-back", DispatchAdmission: admitted,
		})
	}()
	waitInPlaceTestSignal(t, admitted, "/back core admission")
	waitInPlaceTestSignal(t, p.entered, "/back command capability check")

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-after-back", Content: "after-back", ReplyCtx: "ctx-message"})
	select {
	case got := <-bLive.sendPrompts:
		close(p.release)
		t.Fatalf("message entered old B before /back gate: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	close(p.release)
	waitInPlaceTestPrompt(t, aLive.sendPrompts, "after-back")
	aLive.events <- Event{Type: EventResult, SessionID: "thread-a", Done: true}
	waitInPlaceTestSignal(t, backDone, "/back completion")
}

func TestInPlaceSwitchRegistersGateBeforeAdmission(t *testing.T) {
	p := &secondCapabilityCallBlockingPlatform{
		inPlaceNewPlatform: &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}},
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	aLive := newControllableSession("thread-a")
	aLive.sendPrompts = make(chan string, 1)
	bLive := newControllableSession("thread-b")
	bLive.sendPrompts = make(chan string, 1)
	agent := &inPlaceNewAgent{controllableAgent: &controllableAgent{}}
	agent.listFn = func() ([]AgentSessionInfo, error) {
		return []AgentSessionInfo{{ID: "thread-a", Summary: "A"}, {ID: "thread-b", Summary: "B"}}, nil
	}
	agent.startSessionFn = func(_ context.Context, sessionID string) (AgentSession, error) {
		if sessionID != "thread-b" {
			return nil, fmt.Errorf("resumed session = %q, want thread-b", sessionID)
		}
		return bLive, nil
	}
	e := NewEngine("test", agent, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())
	b := e.sessions.NewSideSession(key, "[Codex] B")
	b.SetAgentSessionID("thread-b", agent.Name())
	e.interactiveStates[key] = &interactiveState{agentSession: aLive, platform: p, replyCtx: "ctx-a", agent: agent}

	admitted := make(chan struct{})
	switchDone := make(chan struct{})
	go func() {
		defer close(switchDone)
		e.ReceiveMessage(p, &Message{
			SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-switch-atomic",
			Content: "/switch 2", ReplyCtx: "ctx-switch", DispatchAdmission: admitted,
		})
	}()
	waitInPlaceTestSignal(t, admitted, "/switch core admission")
	waitInPlaceTestSignal(t, p.entered, "/switch command capability check")

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-after-switch", Content: "after-switch", ReplyCtx: "ctx-message"})
	select {
	case got := <-aLive.sendPrompts:
		close(p.release)
		t.Fatalf("message entered old A before /switch gate: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	close(p.release)
	waitInPlaceTestPrompt(t, bLive.sendPrompts, "after-switch")
	bLive.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}
	waitInPlaceTestSignal(t, switchDone, "/switch completion")
}

func TestExternalConversationNewDoesNotRegisterInPlaceTransition(t *testing.T) {
	e := newTestEngine()
	p := &externalFirstNewPlatform{spawningPlatform: &spawningPlatform{stubPlatformEngine: &stubPlatformEngine{n: "feishu"}}}
	msg := &Message{SessionKey: "feishu:chat-a:user", Platform: "feishu", MessageID: "m-new", Content: "/new B"}
	owned, queued := e.prepareInPlaceDispatch(p, msg, nil)
	if owned != nil || queued {
		t.Fatalf("external /new entered in-place transition: owned=%v queued=%v", owned != nil, queued)
	}
	e.inPlaceTransitionMu.Lock()
	defer e.inPlaceTransitionMu.Unlock()
	if len(e.inPlaceTransitions) != 0 {
		t.Fatalf("external /new registered in-place transitions: %#v", e.inPlaceTransitions)
	}
}

func TestInPlaceTransitionAdmissionIgnoresOtherPlatformsAndCommands(t *testing.T) {
	e := newTestEngine()
	plain := &stubPlatformEngine{n: "feishu"}
	inPlace := &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}}
	tests := []struct {
		name     string
		platform Platform
		content  string
	}{
		{name: "plain back", platform: plain, content: "/back"},
		{name: "plain switch", platform: plain, content: "/switch 2"},
		{name: "in-place current", platform: inPlace, content: "/current"},
		{name: "in-place stop without transition", platform: inPlace, content: "/stop"},
		{name: "ordinary message", platform: inPlace, content: "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &Message{SessionKey: "test:" + strings.ReplaceAll(tt.name, " ", "-"), MessageID: tt.name, Content: tt.content}
			owned, queued := e.prepareInPlaceDispatch(tt.platform, msg, nil)
			if owned != nil || queued || msg.inPlaceTransition != nil {
				t.Fatalf("unexpected transition admission: owned=%v queued=%v marker=%v", owned != nil, queued, msg.inPlaceTransition != nil)
			}
		})
	}
}

func TestInPlaceNavigationAliasClaimsTransitionAtAdmission(t *testing.T) {
	e := newTestEngine()
	e.AddAlias("/return", "/back")
	p := &inPlaceNewPlatform{stubPlatformEngine: &stubPlatformEngine{n: "weixin"}}
	msg := &Message{SessionKey: "weixin:dm:alias", MessageID: "m-alias", Content: "/return"}
	owned, queued := e.prepareInPlaceDispatch(p, msg, nil)
	if owned == nil || queued || msg.inPlaceTransition != owned {
		t.Fatalf("alias admission: owned=%v queued=%v marker_matches=%v", owned != nil, queued, msg.inPlaceTransition == owned)
	}
}

func TestInPlaceTransitionDrainsMixedQueueWithoutOvertaking(t *testing.T) {
	nameStarted := make(chan struct{}, 1)
	nameRelease := make(chan struct{})
	bLive := &namingAgentSession{
		controllableAgentSession: newControllableSession("thread-b"),
		nameStarted:              nameStarted,
		nameRelease:              nameRelease,
	}
	bLive.sendPrompts = make(chan string, 1)
	cLive := &namingAgentSession{controllableAgentSession: newControllableSession("thread-c")}
	cLive.sendPrompts = make(chan string, 1)
	e, p, agent, _ := newInPlaceNewTestEngine(t, nil)
	var startMu sync.Mutex
	starts := []AgentSession{bLive, cLive}
	agent.startSessionFn = func(context.Context, string) (AgentSession, error) {
		startMu.Lock()
		defer startMu.Unlock()
		if len(starts) == 0 {
			return nil, errors.New("unexpected extra session start")
		}
		next := starts[0]
		starts = starts[1:]
		return next, nil
	}
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	e.interactiveStates[key] = &interactiveState{agentSession: newControllableSession("thread-a"), platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-b", Content: "/new B", ReplyCtx: "ctx-new-b"})
	}()
	waitInPlaceTestSignal(t, nameStarted, "B name sync")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-first-b", Content: "first-B", ReplyCtx: "ctx-first-b"})
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-back-mixed", Content: "/back", ReplyCtx: "ctx-back"})
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-c", Content: "/new C", ReplyCtx: "ctx-new-c"})
	close(nameRelease)

	waitInPlaceTestPrompt(t, bLive.sendPrompts, "first-B")
	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "thread-b" {
		t.Fatalf("active changed before first B turn completed: %q", got)
	}
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-late", Content: "late-after-finish", ReplyCtx: "ctx-late"})
	bLive.events <- Event{Type: EventResult, SessionID: "thread-b", Done: true}

	waitInPlaceTestPrompt(t, cLive.sendPrompts, "late-after-finish")
	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "thread-c" {
		t.Fatalf("late message routed before nested /new completed: active=%q", got)
	}
	cLive.events <- Event{Type: EventResult, SessionID: "thread-c", Done: true}
	waitInPlaceTestSignal(t, newDone, "mixed transition drain")
	waitInPlaceTestSessionIdle(t, e.sessions.GetOrCreateActive(key))
	if got := len(e.sessions.ListSessions(key)); got != 3 {
		t.Fatalf("sessions = %d, want A/B/C", got)
	}
}

func TestInPlaceNewRetryWaitReleasesQueuedMessageToSource(t *testing.T) {
	startEntered := make(chan struct{})
	startRelease := make(chan struct{})
	e, p, agent, store := newInPlaceNewTestEngine(t, nil)
	e.newOperationRetryDelay = time.Hour
	agent.startSessionFn = func(context.Context, string) (AgentSession, error) {
		close(startEntered)
		<-startRelease
		return nil, errors.New("temporary startup failure")
	}
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	if err := e.sessions.SaveErr(); err != nil {
		t.Fatal(err)
	}
	e.sessions.storePath = ""
	sourceLive := newControllableSession("thread-a")
	sourceLive.sendPrompts = make(chan string, 2)
	e.interactiveStates[key] = &interactiveState{agentSession: sourceLive, platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-retry", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, startEntered, "candidate agent start")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-still-a-1", Content: "still-A-1", ReplyCtx: "ctx-a-1"})
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-still-a-2", Content: "still-A-2", ReplyCtx: "ctx-a-2"})
	close(startRelease)

	for _, content := range []string{"still-A-1", "still-A-2"} {
		waitInPlaceTestPrompt(t, sourceLive.sendPrompts, content)
		sourceLive.events <- Event{Type: EventResult, SessionID: "thread-a", Done: true}
	}
	waitInPlaceTestSignal(t, newDone, "failed /new completion")
	waitInPlaceTestSessionIdle(t, source)
	if got := e.sessions.ActiveSessionID(key); got != source.ID {
		t.Fatalf("active = %q, want source %q", got, source.ID)
	}
	op, ok := store.Get(NewOperationID("test", key, "m-new-retry"))
	if !ok || op.Status != NewOperationRetryWait {
		t.Fatalf("operation = %+v, ok=%v, want retry_wait", op, ok)
	}
}

func TestInPlaceNewManualActionReleasesQueuedMessageToSource(t *testing.T) {
	startEntered := make(chan struct{})
	startRelease := make(chan struct{})
	e, p, agent, store := newInPlaceNewTestEngine(t, nil)
	agent.startSessionFn = func(context.Context, string) (AgentSession, error) {
		close(startEntered)
		<-startRelease
		return nil, NewPermanentOperationError(errors.New("permission denied"))
	}
	t.Cleanup(func() { _ = e.Stop() })

	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	if err := e.sessions.SaveErr(); err != nil {
		t.Fatal(err)
	}
	e.sessions.storePath = ""
	sourceLive := newControllableSession("thread-a")
	sourceLive.sendPrompts = make(chan string, 1)
	e.interactiveStates[key] = &interactiveState{agentSession: sourceLive, platform: p, replyCtx: "ctx-a", agent: agent}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-manual", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, startEntered, "candidate agent start")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-still-a", Content: "still-A", ReplyCtx: "ctx-a-message"})
	close(startRelease)

	waitInPlaceTestPrompt(t, sourceLive.sendPrompts, "still-A")
	sourceLive.events <- Event{Type: EventResult, SessionID: "thread-a", Done: true}
	waitInPlaceTestSignal(t, newDone, "manual-action /new completion")
	waitInPlaceTestSessionIdle(t, source)
	if got := e.sessions.ActiveSessionID(key); got != source.ID {
		t.Fatalf("active = %q, want source %q", got, source.ID)
	}
	op, ok := store.Get(NewOperationID("test", key, "m-new-manual"))
	if !ok || op.Status != NewOperationManualAction {
		t.Fatalf("operation = %+v, ok=%v, want manual_action_required", op, ok)
	}
}

func TestInPlaceNavigationCommandsQueueBehindNew(t *testing.T) {
	nameStarted := make(chan struct{}, 1)
	nameRelease := make(chan struct{})
	candidateLive := &namingAgentSession{
		controllableAgentSession: newControllableSession("thread-b"),
		nameStarted:              nameStarted,
		nameRelease:              nameRelease,
	}
	e, p, agent, _ := newInPlaceNewTestEngine(t, candidateLive)
	e.newOperationRetryDelay = time.Hour
	const key = "weixin:dm:peer"
	source := e.sessions.NewSession(key, "[Codex] A")
	source.SetAgentSessionID("thread-a", agent.Name())
	if err := e.sessions.SaveErr(); err != nil {
		t.Fatal(err)
	}
	agent.listFn = func() ([]AgentSessionInfo, error) {
		return []AgentSessionInfo{{ID: "thread-a", Summary: "A"}, {ID: "thread-b", Summary: "B"}}, nil
	}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-new-nav", Content: "/new B", ReplyCtx: "ctx-new"})
	}()
	waitInPlaceTestSignal(t, nameStarted, "candidate name sync")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-back-queued", Content: "/back", ReplyCtx: "ctx-back"})
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-switch-queued", Content: "/switch 2", ReplyCtx: "ctx-switch"})
	close(nameRelease)
	waitInPlaceTestSignal(t, newDone, "serialized navigation completion")

	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "thread-b" {
		t.Fatalf("active after queued back/switch = %q, want thread-b", got)
	}
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-back-after-switch", Content: "/back", ReplyCtx: "ctx-back-final"})
	if got := e.sessions.ActiveSessionID(key); got != source.ID {
		t.Fatalf("active after final back = %q, want source %q", got, source.ID)
	}
}

func TestCmdBackReturnsToPreviousWeixinSession(t *testing.T) {
	e, base, agent, _ := newInPlaceNewTestEngine(t, nil)
	p := &labeledInPlaceNewPlatform{inPlaceNewPlatform: base, sessionKey: "weixin:dm:peer"}
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())
	b := e.sessions.NewSideSession(key, "[Codex] B")
	b.SetAgentSessionID("thread-b", agent.Name())
	if err := e.sessions.ActivatePreparedSession(key, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-back", Content: "/back", ReplyCtx: "ctx"})

	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "thread-a" {
		t.Fatalf("active agent session = %q, want thread-a", got)
	}
	sent := p.getSent()
	if len(sent) == 0 || !strings.HasPrefix(sent[len(sent)-1], "[A] ") {
		t.Fatalf("last reply = %q, want [A] prefix", strings.Join(sent, "\n"))
	}
}

func TestCmdSwitchOnWeixinRecordsBackPoint(t *testing.T) {
	e, base, agent, _ := newInPlaceNewTestEngine(t, nil)
	p := &labeledInPlaceNewPlatform{inPlaceNewPlatform: base, sessionKey: "weixin:dm:peer"}
	const key = "weixin:dm:peer"
	a := e.sessions.NewSession(key, "[Codex] A")
	a.SetAgentSessionID("thread-a", agent.Name())
	b := e.sessions.NewSideSession(key, "[Codex] B")
	b.SetAgentSessionID("thread-b", agent.Name())
	agent.listFn = func() ([]AgentSessionInfo, error) {
		return []AgentSessionInfo{{ID: "thread-a", Summary: "A"}, {ID: "thread-b", Summary: "B"}}, nil
	}

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-switch", Content: "/switch 2", ReplyCtx: "ctx-switch"})
	if got := e.sessions.ActiveSessionID(key); got != b.ID {
		t.Fatalf("active after switch = %q, want %q", got, b.ID)
	}
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "weixin", UserID: "peer", MessageID: "m-back", Content: "/back", ReplyCtx: "ctx-back"})
	if got := e.sessions.ActiveSessionID(key); got != a.ID {
		t.Fatalf("active after back = %q, want %q", got, a.ID)
	}
}
