package core

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type startupConfigSnapshot struct {
	sessionKey string
	provider   string
}

// concurrentStartupConfigAgent models agents whose next StartSession consumes
// mutable configuration previously installed through SetSessionEnv and
// SetActiveProvider.
type concurrentStartupConfigAgent struct {
	mu sync.Mutex

	envSessionKey string
	active        string
	providers     []ProviderConfig
	startCalls    int

	firstStartEntered chan struct{}
	releaseFirst      chan struct{}
	secondConfigured  chan struct{}
	secondOnce        sync.Once
	snapshots         chan startupConfigSnapshot
}

func newConcurrentStartupConfigAgent() *concurrentStartupConfigAgent {
	return &concurrentStartupConfigAgent{
		providers:         []ProviderConfig{{Name: "provider-a"}, {Name: "provider-b"}},
		firstStartEntered: make(chan struct{}),
		releaseFirst:      make(chan struct{}),
		secondConfigured:  make(chan struct{}),
		snapshots:         make(chan startupConfigSnapshot, 2),
	}
}

func (a *concurrentStartupConfigAgent) Name() string { return "config-snapshot" }

func (a *concurrentStartupConfigAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	a.startCalls++
	call := a.startCalls
	a.mu.Unlock()
	if call == 1 {
		close(a.firstStartEntered)
		<-a.releaseFirst
	}

	a.mu.Lock()
	snapshot := startupConfigSnapshot{sessionKey: a.envSessionKey, provider: a.active}
	a.mu.Unlock()
	a.snapshots <- snapshot
	return newControllableSession("thread-" + snapshot.sessionKey), nil
}

func (a *concurrentStartupConfigAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}

func (a *concurrentStartupConfigAgent) Stop() error { return nil }

func (a *concurrentStartupConfigAgent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, entry := range env {
		if strings.HasPrefix(entry, "CC_SESSION_KEY=") {
			a.envSessionKey = strings.TrimPrefix(entry, "CC_SESSION_KEY=")
			if a.envSessionKey == "route-b" {
				a.secondOnce.Do(func() { close(a.secondConfigured) })
			}
			return
		}
	}
}

func (a *concurrentStartupConfigAgent) ListProviders() []ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ProviderConfig(nil), a.providers...)
}

func (a *concurrentStartupConfigAgent) SetProviders(providers []ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers = append([]ProviderConfig(nil), providers...)
}

func (a *concurrentStartupConfigAgent) SetActiveProvider(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, provider := range a.providers {
		if provider.Name == name {
			a.active = name
			return true
		}
	}
	return false
}

func (a *concurrentStartupConfigAgent) GetActiveProvider() *ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, provider := range a.providers {
		if provider.Name == a.active {
			copy := provider
			return &copy
		}
	}
	return nil
}

func TestInteractiveStartKeepsMutableAgentConfigWithItsRoute(t *testing.T) {
	agent := newConcurrentStartupConfigAgent()
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	sessionA := e.sessions.GetOrCreateActive("route-a")
	sessionA.SetActiveProvider("provider-a")
	sessionB := e.sessions.GetOrCreateActive("route-b")
	sessionB.SetActiveProvider("provider-b")

	states := make(chan *interactiveState, 2)
	go func() {
		states <- e.getOrCreateInteractiveStateWith("route-a", p, "ctx-a", sessionA, e.sessions, nil, "route-a")
	}()
	select {
	case <-agent.firstStartEntered:
	case <-time.After(time.Second):
		t.Fatal("first route did not enter StartSession")
	}

	go func() {
		states <- e.getOrCreateInteractiveStateWith("route-b", p, "ctx-b", sessionB, e.sessions, nil, "route-b")
	}()

	// Before the fix route B can overwrite route A's mutable startup config.
	// With serialized per-agent startup it cannot configure until A has started.
	select {
	case <-agent.secondConfigured:
	case <-time.After(100 * time.Millisecond):
	}
	close(agent.releaseFirst)

	for range 2 {
		select {
		case state := <-states:
			state.mu.Lock()
			started := state.agentSession != nil
			state.mu.Unlock()
			if !started {
				t.Fatal("route returned without an agent session")
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent route startup did not finish")
		}
	}

	got := make(map[string]string)
	for range 2 {
		select {
		case snapshot := <-agent.snapshots:
			got[snapshot.sessionKey] = snapshot.provider
		case <-time.After(time.Second):
			t.Fatal("missing agent startup snapshot")
		}
	}
	want := map[string]string{"route-a": "provider-a", "route-b": "provider-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startup config snapshots = %#v, want %#v", got, want)
	}
}

func TestFirstRouteMessageWaitsForRecoveryStarterAndIsDelivered(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	agentSession := newControllableSession("thread-shared")
	agentSession.sendPrompts = make(chan string, 1)
	agent := &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		close(started)
		<-release
		return agentSession, nil
	}}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("route-shared")

	firstDone := make(chan *interactiveState, 1)
	go func() {
		firstDone <- e.getOrCreateInteractiveStateWith("route-shared", p, "ctx-a", session, e.sessions, nil, "route-shared")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("route starter did not begin")
	}

	if !session.TryLock() {
		t.Fatal("could not lock route for first message")
	}
	messageDone := make(chan struct{})
	go func() {
		e.processInteractiveMessageWith(p, &Message{
			Platform:   "test",
			SessionKey: "route-shared",
			UserID:     "user-1",
			MessageID:  "first-message",
			Content:    "first message during recovery",
			ReplyCtx:   "ctx-b",
		}, session, agent, e.sessions, "route-shared", "", "route-shared")
		close(messageDone)
	}()

	returnedEarly := false
	select {
	case <-messageDone:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	var first *interactiveState
	select {
	case first = <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("route starter did not finish")
	}
	if returnedEarly {
		t.Fatal("first route message returned while agentSession was still nil")
	}

	select {
	case prompt := <-agentSession.sendPrompts:
		if !strings.Contains(prompt, "first message during recovery") {
			t.Fatalf("delivered prompt = %q", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("first route message was not delivered after recovery startup")
	}
	agentSession.events <- Event{Type: EventResult, Content: "done", SessionID: "thread-shared", Done: true}
	select {
	case <-messageDone:
	case <-time.After(time.Second):
		t.Fatal("first route message did not finish")
	}

	if first == nil {
		t.Fatal("route starter returned nil state")
	}
	first.mu.Lock()
	got := first.agentSession
	first.mu.Unlock()
	if got != agentSession {
		t.Fatalf("started agent session = %p, want %p", got, agentSession)
	}
	for _, sent := range p.getSent() {
		if strings.Contains(sent, e.i18n.T(MsgFailedToStartAgentSession)) {
			t.Fatalf("first route message was falsely rejected: %q", sent)
		}
	}
}
