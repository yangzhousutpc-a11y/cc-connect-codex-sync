package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testCodexThreadID = "00000000-0000-7000-8000-000000000001"

type threadRoutePlatform struct {
	stubPlatformEngine
	routeMu        sync.Mutex
	reconstructed  []string
	receivedImages []ImageAttachment
	receivedFiles  []FileAttachment
	reconstructErr error
}

func (p *threadRoutePlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	p.routeMu.Lock()
	p.reconstructed = append(p.reconstructed, sessionKey)
	p.routeMu.Unlock()
	if p.reconstructErr != nil {
		return nil, p.reconstructErr
	}
	return "reconstructed", nil
}

func (p *threadRoutePlatform) SendImage(_ context.Context, _ any, image ImageAttachment) error {
	p.routeMu.Lock()
	p.receivedImages = append(p.receivedImages, image)
	p.routeMu.Unlock()
	return nil
}

func (p *threadRoutePlatform) SendFile(_ context.Context, _ any, file FileAttachment) error {
	p.routeMu.Lock()
	p.receivedFiles = append(p.receivedFiles, file)
	p.routeMu.Unlock()
	return nil
}

func postThreadRouteSend(t *testing.T, api *APIServer, request SendRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleSend(rec, req)
	return rec
}

func bindActiveAgentThread(t *testing.T, sessions *SessionManager, sessionKey, threadID string) {
	t.Helper()
	session := sessions.GetOrCreateActive(sessionKey)
	session.SetAgentSessionID(threadID, "codex")
}

func TestHandleSend_AgentThreadRouteRequiresExactlyOneActiveMatch(t *testing.T) {
	t.Run("zero matches", func(t *testing.T) {
		platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
		engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
		api := &APIServer{engines: map[string]*Engine{"test": engine}}

		rec := postThreadRouteSend(t, api, SendRequest{
			Project:       "test",
			AgentThreadID: testCodexThreadID,
			Message:       "hello",
		})
		if rec.Code == http.StatusOK {
			t.Fatalf("status = %d, want fail closed", rec.Code)
		}
		if strings.Contains(rec.Body.String(), testCodexThreadID) {
			t.Fatalf("response leaked agent thread ID: %s", rec.Body.String())
		}
	})

	t.Run("one match", func(t *testing.T) {
		platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
		engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
		bindActiveAgentThread(t, engine.sessions, "wecom:room:user", testCodexThreadID)
		api := &APIServer{engines: map[string]*Engine{"test": engine}}

		rec := postThreadRouteSend(t, api, SendRequest{
			Project:       "test",
			AgentThreadID: testCodexThreadID,
			Message:       "hello",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		if got := platform.getSent(); len(got) != 1 || got[0] != "hello" {
			t.Fatalf("sent = %#v, want hello", got)
		}
	})

	t.Run("two matches", func(t *testing.T) {
		platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
		engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
		bindActiveAgentThread(t, engine.sessions, "wecom:room-a:user", testCodexThreadID)
		bindActiveAgentThread(t, engine.sessions, "wecom:room-b:user", testCodexThreadID)
		api := &APIServer{engines: map[string]*Engine{"test": engine}}

		rec := postThreadRouteSend(t, api, SendRequest{
			Project:       "test",
			AgentThreadID: testCodexThreadID,
			Message:       "hello",
		})
		if rec.Code == http.StatusOK {
			t.Fatalf("status = %d, want ambiguous route rejected", rec.Code)
		}
		if strings.Contains(rec.Body.String(), testCodexThreadID) ||
			strings.Contains(rec.Body.String(), "wecom:room-a:user") ||
			strings.Contains(rec.Body.String(), "wecom:room-b:user") {
			t.Fatalf("response leaked route context: %s", rec.Body.String())
		}
	})
}

func TestHandleSend_AgentThreadRouteIgnoresInactiveAndPastSessions(t *testing.T) {
	platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
	old := engine.sessions.SwitchToAgentSession("wecom:room:user", testCodexThreadID, "codex", "old")
	if old == nil {
		t.Fatal("create old session")
	}
	current := engine.sessions.NewSession("wecom:room:user", "current")
	current.SetAgentSessionID("00000000-0000-7000-8000-000000000002", "codex")
	current.PastAgentSessionIDs = append(current.PastAgentSessionIDs, testCodexThreadID)
	api := &APIServer{engines: map[string]*Engine{"test": engine}}

	rec := postThreadRouteSend(t, api, SendRequest{
		Project:       "test",
		AgentThreadID: testCodexThreadID,
		Message:       "hello",
	})
	if rec.Code == http.StatusOK {
		t.Fatal("inactive or past agent thread matched active route")
	}
	if got := platform.getSent(); len(got) != 0 {
		t.Fatalf("sent = %#v, want none", got)
	}
}

func TestHandleSend_AgentThreadRouteDispatchesTextImageAndFileAcrossPlatforms(t *testing.T) {
	for _, platformName := range []string{"feishu", "weixin", "wecom"} {
		t.Run(platformName, func(t *testing.T) {
			platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: platformName}}
			engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
			bindActiveAgentThread(t, engine.sessions, platformName+":room:user", testCodexThreadID)
			api := &APIServer{engines: map[string]*Engine{"test": engine}}

			rec := postThreadRouteSend(t, api, SendRequest{
				Project:       "test",
				AgentThreadID: testCodexThreadID,
				Message:       "hello",
				Images:        []ImageAttachment{{FileName: "image.png", MimeType: "image/png", Data: []byte("image")}},
				Files:         []FileAttachment{{FileName: "file.txt", MimeType: "text/plain", Data: []byte("file")}},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			if got := platform.getSent(); len(got) != 1 {
				t.Fatalf("text sends = %d, want 1", len(got))
			}
			platform.routeMu.Lock()
			defer platform.routeMu.Unlock()
			if len(platform.reconstructed) != 1 || platform.reconstructed[0] != platformName+":room:user" {
				t.Fatalf("reconstructed = %#v", platform.reconstructed)
			}
			if len(platform.receivedImages) != 1 || len(platform.receivedFiles) != 1 {
				t.Fatalf("images/files = %d/%d, want 1/1", len(platform.receivedImages), len(platform.receivedFiles))
			}
		})
	}
}

func TestHandleSend_AgentThreadRouteResolvesLoadedWorkspaceUniquely(t *testing.T) {
	platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
	engine.SetMultiWorkspace(t.TempDir(), filepath.Join(t.TempDir(), "bindings.json"))
	workspace := engine.workspacePool.GetOrCreate(t.TempDir())
	workspace.sessions = NewSessionManager("")
	bindActiveAgentThread(t, workspace.sessions, "feishu:room:user", testCodexThreadID)
	t.Cleanup(func() { _ = engine.Stop() })
	api := &APIServer{engines: map[string]*Engine{"test": engine}}

	rec := postThreadRouteSend(t, api, SendRequest{
		Project:       "test",
		AgentThreadID: testCodexThreadID,
		Message:       "hello",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSend_AgentThreadRouteRejectsCrossWorkspaceAmbiguity(t *testing.T) {
	platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
	bindActiveAgentThread(t, engine.sessions, "feishu:root:user", testCodexThreadID)
	engine.SetMultiWorkspace(t.TempDir(), filepath.Join(t.TempDir(), "bindings.json"))
	workspace := engine.workspacePool.GetOrCreate(t.TempDir())
	workspace.sessions = NewSessionManager("")
	bindActiveAgentThread(t, workspace.sessions, "feishu:workspace:user", testCodexThreadID)
	t.Cleanup(func() { _ = engine.Stop() })
	api := &APIServer{engines: map[string]*Engine{"test": engine}}

	rec := postThreadRouteSend(t, api, SendRequest{
		Project:       "test",
		AgentThreadID: testCodexThreadID,
		Message:       "hello",
	})
	if rec.Code == http.StatusOK {
		t.Fatal("cross-workspace ambiguous route was accepted")
	}
	if strings.Contains(rec.Body.String(), testCodexThreadID) ||
		strings.Contains(rec.Body.String(), "feishu:root:user") ||
		strings.Contains(rec.Body.String(), "feishu:workspace:user") {
		t.Fatalf("response leaked route context: %s", rec.Body.String())
	}
}

func TestHandleSend_AgentThreadRouteSelectsUniqueProjectWhenProjectOmitted(t *testing.T) {
	platformA := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	platformB := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engineA := NewEngine("a", &stubAgent{}, []Platform{platformA}, "", LangEnglish)
	engineB := NewEngine("b", &stubAgent{}, []Platform{platformB}, "", LangEnglish)
	bindActiveAgentThread(t, engineB.sessions, "wecom:room:user", testCodexThreadID)
	api := &APIServer{engines: map[string]*Engine{"a": engineA, "b": engineB}}

	rec := postThreadRouteSend(t, api, SendRequest{
		AgentThreadID: testCodexThreadID,
		Message:       "hello",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := platformA.getSent(); len(got) != 0 {
		t.Fatalf("project a sent = %#v, want none", got)
	}
	if got := platformB.getSent(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("project b sent = %#v, want hello", got)
	}
}

func TestHandleSend_AgentThreadRouteRejectsCrossProjectAmbiguity(t *testing.T) {
	platformA := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	platformB := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engineA := NewEngine("a", &stubAgent{}, []Platform{platformA}, "", LangEnglish)
	engineB := NewEngine("b", &stubAgent{}, []Platform{platformB}, "", LangEnglish)
	bindActiveAgentThread(t, engineA.sessions, "feishu:room:user", testCodexThreadID)
	bindActiveAgentThread(t, engineB.sessions, "wecom:room:user", testCodexThreadID)
	api := &APIServer{engines: map[string]*Engine{"a": engineA, "b": engineB}}

	rec := postThreadRouteSend(t, api, SendRequest{
		AgentThreadID: testCodexThreadID,
		Message:       "hello",
	})
	if rec.Code == http.StatusOK {
		t.Fatal("cross-project ambiguous route was accepted")
	}
	for _, secret := range []string{testCodexThreadID, "feishu:room:user", "wecom:room:user"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("response leaked route context: %s", rec.Body.String())
		}
	}
}

func TestHandleSend_AgentThreadRouteHidesDownstreamRouteErrors(t *testing.T) {
	route := "weixin:private-user"
	platform := &threadRoutePlatform{
		stubPlatformEngine: stubPlatformEngine{n: "weixin"},
		reconstructErr:     errors.New("context missing for " + route + " and " + testCodexThreadID),
	}
	engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
	bindActiveAgentThread(t, engine.sessions, route, testCodexThreadID)
	api := &APIServer{engines: map[string]*Engine{"test": engine}}

	rec := postThreadRouteSend(t, api, SendRequest{
		Project:       "test",
		AgentThreadID: testCodexThreadID,
		Message:       "hello",
	})
	if rec.Code == http.StatusOK {
		t.Fatal("downstream reconstruction error unexpectedly succeeded")
	}
	for _, secret := range []string{testCodexThreadID, route, "private-user"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("response leaked downstream route context: %s", rec.Body.String())
		}
	}
}

func TestHandleSend_ExplicitSessionIgnoresAgentThreadFallback(t *testing.T) {
	platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	engine := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
	api := &APIServer{engines: map[string]*Engine{"test": engine}}

	rec := postThreadRouteSend(t, api, SendRequest{
		Project:       "test",
		SessionKey:    "wecom:explicit:user",
		AgentThreadID: "invalid-and-untrusted",
		Message:       "hello",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	platform.routeMu.Lock()
	defer platform.routeMu.Unlock()
	if len(platform.reconstructed) != 1 || platform.reconstructed[0] != "wecom:explicit:user" {
		t.Fatalf("reconstructed = %#v, want explicit route", platform.reconstructed)
	}
}

func TestHandleSend_AgentThreadRouteRestoresPersistedWorkspaceAfterRestart(t *testing.T) {
	baseDir := t.TempDir()
	workspaceDir := filepath.Join(baseDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	sessionStore := filepath.Join(stateDir, "test.json")
	bindingStore := filepath.Join(stateDir, "bindings.json")
	agentName := "thread-route-restart-agent"
	RegisterAgent(agentName, func(map[string]any) (Agent, error) {
		return &namedTestAgent{name: agentName}, nil
	})

	first := NewEngine("test", &namedTestAgent{name: agentName}, nil, sessionStore, LangEnglish)
	first.SetMultiWorkspace(baseDir, bindingStore)
	first.workspaceBindings.Bind("project:test", "feishu:room", "room", workspaceDir)
	_, workspaceSessions, err := first.getOrCreateWorkspaceAgent(normalizeWorkspacePath(workspaceDir))
	if err != nil {
		t.Fatalf("create first workspace: %v", err)
	}
	if workspaceSessions.SwitchToAgentSession("feishu:room:user", testCodexThreadID, "codex", "test") == nil {
		t.Fatal("persist workspace route")
	}
	workspaceStore := workspaceSessions.StorePath()
	if matches := workspaceSessions.ActiveRoutesForAgentSessionID(testCodexThreadID); len(matches) != 1 {
		t.Fatalf("first workspace route matches = %d, want 1", len(matches))
	}
	if err := first.Stop(); err != nil {
		t.Fatalf("stop first engine: %v", err)
	}
	if matches := NewSessionManager(workspaceStore).ActiveRoutesForAgentSessionID(testCodexThreadID); len(matches) != 1 {
		t.Fatalf("persisted workspace route matches = %d, want 1", len(matches))
	}

	platform := &threadRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	restarted := NewEngine("test", &namedTestAgent{name: agentName}, []Platform{platform}, sessionStore, LangEnglish)
	restarted.SetMultiWorkspace(baseDir, bindingStore)
	t.Cleanup(func() { _ = restarted.Stop() })
	if len(restarted.workspacePool.All()) != 0 {
		t.Fatal("workspace pool unexpectedly warm before send")
	}
	if matches := restarted.activeRoutesForAgentThread(testCodexThreadID); len(matches) != 1 {
		t.Fatalf("restored route matches = %d, want 1", len(matches))
	}
	api := &APIServer{engines: map[string]*Engine{"test": restarted}}

	rec := postThreadRouteSend(t, api, SendRequest{
		Project:       "test",
		AgentThreadID: testCodexThreadID,
		Message:       "hello",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := platform.getSent(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("sent = %#v, want hello", got)
	}
}
