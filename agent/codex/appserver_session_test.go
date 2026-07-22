package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestAppServerSession_ApplyThreadRuntimeState(t *testing.T) {
	s := &appServerSession{}
	effort := "xhigh"

	s.applyThreadRuntimeState("/tmp/project", "gpt-5.4", &effort)

	if got := s.GetWorkDir(); got != "/tmp/project" {
		t.Fatalf("GetWorkDir() = %q, want /tmp/project", got)
	}
	if got := s.GetModel(); got != "gpt-5.4" {
		t.Fatalf("GetModel() = %q, want gpt-5.4", got)
	}
	if got := s.GetReasoningEffort(); got != "xhigh" {
		t.Fatalf("GetReasoningEffort() = %q, want xhigh", got)
	}
}

func TestAppServerSession_HandleRateLimitsUpdatedCachesUsage(t *testing.T) {
	s := &appServerSession{}
	raw, err := json.Marshal(appServerRateLimitsResponse{
		RateLimits: appServerRateLimitSnapshot{
			LimitID:   "codex",
			PlanType:  "pro",
			Primary:   &appServerRateLimitWindow{UsedPercent: 25, WindowDurationMins: 15, ResetsAt: 1730947200},
			Secondary: &appServerRateLimitWindow{UsedPercent: 42, WindowDurationMins: 60, ResetsAt: 1730950800},
			Credits:   &appServerCreditsSnapshot{HasCredits: true, Unlimited: false},
		},
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}

	s.handleNotification("account/rateLimits/updated", raw)

	report, err := s.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("GetUsage() returned error: %v", err)
	}
	if report.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", report.Provider)
	}
	if report.Plan != "pro" {
		t.Fatalf("plan = %q, want pro", report.Plan)
	}
	if len(report.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(report.Buckets))
	}
	if got := report.Buckets[0].Name; got != "codex" {
		t.Fatalf("bucket name = %q, want codex", got)
	}
	if got := report.Buckets[0].Windows[0].WindowSeconds; got != 15*60 {
		t.Fatalf("primary window seconds = %d, want %d", got, 15*60)
	}
	if got := report.Buckets[0].Windows[1].UsedPercent; got != 42 {
		t.Fatalf("secondary used percent = %d, want 42", got)
	}
	if report.Credits == nil || !report.Credits.HasCredits {
		t.Fatalf("credits = %#v, want has credits", report.Credits)
	}
}

func TestAppServerSession_HandleThreadTokenUsageUpdatedCachesContextUsage(t *testing.T) {
	s := &appServerSession{}
	raw, err := json.Marshal(appServerThreadTokenUsageNotification{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		TokenUsage: struct {
			Total              codexTokenUsage `json:"total"`
			Last               codexTokenUsage `json:"last"`
			ModelContextWindow int             `json:"modelContextWindow"`
		}{
			Total: codexTokenUsage{
				TotalTokens:           52011395,
				InputTokens:           51847383,
				CachedInputTokens:     48187904,
				OutputTokens:          164012,
				ReasoningOutputTokens: 78910,
			},
			Last: codexTokenUsage{
				TotalTokens:           41061,
				InputTokens:           40849,
				CachedInputTokens:     36864,
				OutputTokens:          212,
				ReasoningOutputTokens: 32,
			},
			ModelContextWindow: 258400,
		},
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}

	s.handleNotification("thread/tokenUsage/updated", raw)

	usage := s.GetContextUsage()
	if usage == nil {
		t.Fatal("GetContextUsage() = nil, want cached context usage")
	}
	if usage.UsedTokens != 41061 {
		t.Fatalf("used tokens = %d, want 41061", usage.UsedTokens)
	}
	if usage.BaselineTokens != codexContextBaselineTokens {
		t.Fatalf("baseline tokens = %d, want %d", usage.BaselineTokens, codexContextBaselineTokens)
	}
	if usage.TotalTokens != 41061 {
		t.Fatalf("total tokens = %d, want 41061", usage.TotalTokens)
	}
	if usage.ContextWindow != 258400 {
		t.Fatalf("context window = %d, want 258400", usage.ContextWindow)
	}
	if usage.CachedInputTokens != 36864 {
		t.Fatalf("cached input tokens = %d, want 36864", usage.CachedInputTokens)
	}
	if usage.InputTokens != 40849 {
		t.Fatalf("input tokens = %d, want 40849", usage.InputTokens)
	}
}

func TestMapAppServerRateLimits_PrefersMultiBucketView(t *testing.T) {
	report := mapAppServerRateLimits(appServerRateLimitsResponse{
		RateLimits: appServerRateLimitSnapshot{
			LimitID:  "legacy",
			PlanType: "team",
			Primary:  &appServerRateLimitWindow{UsedPercent: 99, WindowDurationMins: 15},
		},
		RateLimitsByLimitID: map[string]appServerRateLimitSnapshot{
			"codex": {
				LimitID:   "codex",
				LimitName: "Codex",
				PlanType:  "team",
				Primary:   &appServerRateLimitWindow{UsedPercent: 10, WindowDurationMins: 15},
			},
			"codex_other": {
				LimitID:  "codex_other",
				PlanType: "team",
				Primary:  &appServerRateLimitWindow{UsedPercent: 20, WindowDurationMins: 60},
			},
		},
	})

	if report.Plan != "team" {
		t.Fatalf("plan = %q, want team", report.Plan)
	}
	if len(report.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(report.Buckets))
	}
	if report.Buckets[0].Name != "Codex" {
		t.Fatalf("first bucket = %q, want Codex", report.Buckets[0].Name)
	}
	if report.Buckets[1].Name != "codex_other" {
		t.Fatalf("second bucket = %q, want codex_other", report.Buckets[1].Name)
	}
}

func TestAppServerSession_HandleRequestUserInputEmitsAskQuestion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		pendingApprovals: make(map[string]chan core.PermissionResult),
		stdin:            stdin,
	}

	s.handleServerRequest(serverRequestProbe(t, `"rui-1"`, "item/tool/requestUserInput", map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"itemId":   "call-1",
		"questions": []any{
			map[string]any{
				"id":       "database",
				"header":   "Database",
				"question": "Which database should we use?",
				"isOther":  true,
				"isSecret": false,
				"options": []any{
					map[string]any{"label": "Postgres", "description": "Use the existing relational database"},
					map[string]any{"label": "SQLite", "description": "Keep it embedded"},
				},
			},
		},
	}))

	var event core.Event
	select {
	case event = <-s.events:
	case <-time.After(time.Second):
		t.Fatal("expected AskUserQuestion event")
	}
	if event.Type != core.EventPermissionRequest {
		t.Fatalf("event type = %s, want %s", event.Type, core.EventPermissionRequest)
	}
	if event.ToolName != "AskUserQuestion" {
		t.Fatalf("tool name = %q, want AskUserQuestion", event.ToolName)
	}
	if event.RequestID != `"rui-1"` {
		t.Fatalf("request id = %q, want raw JSON id", event.RequestID)
	}
	if len(event.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(event.Questions))
	}
	q := event.Questions[0]
	if q.Question != "Which database should we use?" || q.Header != "Database" {
		t.Fatalf("question = %#v", q)
	}
	if len(q.Options) != 2 || q.Options[0].Label != "Postgres" || q.Options[1].Description != "Keep it embedded" {
		t.Fatalf("options = %#v", q.Options)
	}
	if stdin.String() != "" {
		t.Fatalf("request_user_input should not write before the answer, got %q", stdin.String())
	}
}

func TestAppServerSession_HandleRequestUserInputWritesCodexResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		pendingApprovals: make(map[string]chan core.PermissionResult),
		stdin:            stdin,
	}

	s.handleServerRequest(serverRequestProbe(t, `"rui-2"`, "item/tool/requestUserInput", map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"itemId":   "call-2",
		"questions": []any{
			map[string]any{
				"id":       "database",
				"header":   "Database",
				"question": "Which database should we use?",
				"options": []any{
					map[string]any{"label": "Postgres", "description": "Use the existing relational database"},
					map[string]any{"label": "SQLite", "description": "Keep it embedded"},
				},
			},
		},
	}))

	var event core.Event
	select {
	case event = <-s.events:
	case <-time.After(time.Second):
		t.Fatal("expected AskUserQuestion event")
	}
	if err := s.RespondPermission(event.RequestID, core.PermissionResult{
		Behavior: "allow",
		UpdatedInput: map[string]any{
			"answers": map[string]any{
				"Which database should we use?": "Postgres",
			},
		},
	}); err != nil {
		t.Fatalf("RespondPermission() error = %v", err)
	}

	line := waitForWrittenJSONLine(t, stdin)
	var envelope struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Result  struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("decode response %q: %v", line, err)
	}
	if envelope.JSONRPC != "2.0" || envelope.ID != "rui-2" {
		t.Fatalf("envelope = %#v", envelope)
	}
	got := envelope.Result.Answers["database"].Answers
	if len(got) != 1 || got[0] != "Postgres" {
		t.Fatalf("answers[database] = %#v, want [Postgres]", got)
	}
}

func TestAppServerSession_SetSessionNameSyncsAndSkipsDuplicates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	var refreshed []string
	s := &appServerSession{
		ctx:               ctx,
		stdin:             stdin,
		pending:           make(map[int64]chan rpcResponseEnvelope),
		refreshThreadInUI: func(threadID string) { refreshed = append(refreshed, threadID) },
	}
	s.threadID.Store("thread-1")

	setName := func(name string) {
		t.Helper()
		wantLine := strings.Count(stdin.String(), "\n") + 1
		done := make(chan error, 1)
		go func() { done <- s.SetSessionName(name) }()

		line := waitForWrittenJSONLineCount(t, stdin, wantLine)
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("decode request %q: %v", line, err)
		}
		if req.Method != "thread/name/set" || req.Params["threadId"] != "thread-1" || req.Params["name"] != name {
			t.Fatalf("request = %#v", req)
		}

		s.pendingMu.Lock()
		responseCh := s.pending[req.ID]
		s.pendingMu.Unlock()
		responseCh <- rpcResponseEnvelope{ID: req.ID, Result: json.RawMessage(`{}`)}
		if err := <-done; err != nil {
			t.Fatalf("SetSessionName(%q): %v", name, err)
		}
	}

	setName("[Codex] Alpha")
	if !reflect.DeepEqual(refreshed, []string{"thread-1"}) {
		t.Fatalf("refreshes after first name = %#v, want immediate thread refresh", refreshed)
	}
	writesAfterFirstName := strings.Count(stdin.String(), "\n")
	if err := s.SetSessionName("[Codex] Alpha"); err != nil {
		t.Fatalf("duplicate SetSessionName: %v", err)
	}
	if got := strings.Count(stdin.String(), "\n"); got != writesAfterFirstName {
		t.Fatalf("duplicate name wrote another request: got %d lines, want %d", got, writesAfterFirstName)
	}
	if !reflect.DeepEqual(refreshed, []string{"thread-1", "thread-1"}) {
		t.Fatalf("refreshes after duplicate name = %#v, want a second UI refresh", refreshed)
	}
	setName("[Codex] Beta")
	if !reflect.DeepEqual(refreshed, []string{"thread-1", "thread-1", "thread-1"}) {
		t.Fatalf("refreshes after renamed thread = %#v", refreshed)
	}
}

func TestAppServerSession_EnsureThreadResumesMatchingRecoveryKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	absoluteWorkDir := t.TempDir()
	currentWorkDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeWorkDir, err := filepath.Rel(currentWorkDir, absoluteWorkDir)
	if err != nil {
		t.Fatal(err)
	}

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		ctx:     ctx,
		stdin:   stdin,
		pending: make(map[int64]chan rpcResponseEnvelope),
		workDir: relativeWorkDir,
	}
	done := make(chan error, 1)
	go func() {
		done <- s.ensureThread("", "cc-connect/new/op-1")
	}()

	listLine := waitForWrittenJSONLineCount(t, stdin, 1)
	var listReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(listLine), &listReq); err != nil {
		t.Fatal(err)
	}
	if listReq.Method != "thread/list" {
		t.Fatalf("first method = %q, want thread/list", listReq.Method)
	}
	s.pendingMu.Lock()
	listResponse := s.pending[listReq.ID]
	s.pendingMu.Unlock()
	listResponse <- rpcResponseEnvelope{ID: listReq.ID, Result: json.RawMessage(fmt.Sprintf(`{
		"data":[{
			"id":"thread-existing",
			"cwd":%q,
			"threadSource":"cc-connect/new/op-1"
		}]
	}`, absoluteWorkDir))}

	resumeLine := waitForWrittenJSONLineCount(t, stdin, 2)
	var resumeReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(resumeLine), &resumeReq); err != nil {
		t.Fatal(err)
	}
	if resumeReq.Method != "thread/resume" || resumeReq.Params["threadId"] != "thread-existing" {
		t.Fatalf("resume request = %#v", resumeReq)
	}
	s.pendingMu.Lock()
	resumeResponse := s.pending[resumeReq.ID]
	s.pendingMu.Unlock()
	resumeResponse <- rpcResponseEnvelope{ID: resumeReq.ID, Result: json.RawMessage(`{
		"cwd":"/tmp/project",
		"model":"gpt-5.4",
		"reasoningEffort":"high",
		"thread":{"id":"thread-existing"}
	}`)}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := s.CurrentSessionID(); got != "thread-existing" {
		t.Fatalf("CurrentSessionID() = %q, want thread-existing", got)
	}
	if got := strings.Count(stdin.String(), "\n"); got != 2 {
		t.Fatalf("request count = %d, want 2 without thread/start", got)
	}
}

func TestAppServerSession_EnsureThreadStartsWithRecoveryKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		ctx:     ctx,
		stdin:   stdin,
		pending: make(map[int64]chan rpcResponseEnvelope),
		workDir: "/tmp/project",
	}
	done := make(chan error, 1)
	go func() {
		done <- s.ensureThread("", "cc-connect/new/op-2")
	}()

	listLine := waitForWrittenJSONLineCount(t, stdin, 1)
	var listReq struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(listLine), &listReq); err != nil {
		t.Fatal(err)
	}
	if listReq.Method != "thread/list" {
		t.Fatalf("first method = %q, want thread/list", listReq.Method)
	}
	s.pendingMu.Lock()
	listResponse := s.pending[listReq.ID]
	s.pendingMu.Unlock()
	listResponse <- rpcResponseEnvelope{ID: listReq.ID, Result: json.RawMessage(`{"data":[]}`)}

	startLine := waitForWrittenJSONLineCount(t, stdin, 2)
	var startReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(startLine), &startReq); err != nil {
		t.Fatal(err)
	}
	if startReq.Method != "thread/start" {
		t.Fatalf("second method = %q, want thread/start", startReq.Method)
	}
	if got := startReq.Params["threadSource"]; got != "cc-connect/new/op-2" {
		t.Fatalf("threadSource = %#v, want recovery key", got)
	}
	s.pendingMu.Lock()
	startResponse := s.pending[startReq.ID]
	s.pendingMu.Unlock()
	startResponse <- rpcResponseEnvelope{ID: startReq.ID, Result: json.RawMessage(`{
		"cwd":"/tmp/project",
		"model":"gpt-5.4",
		"reasoningEffort":"high",
		"thread":{"id":"thread-new"}
	}`)}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := s.CurrentSessionID(); got != "thread-new" {
		t.Fatalf("CurrentSessionID() = %q, want thread-new", got)
	}
}

func TestAppServerSession_EnsureThreadReplacesUnmaterializedResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		ctx:     ctx,
		stdin:   stdin,
		pending: make(map[int64]chan rpcResponseEnvelope),
		workDir: "/tmp/project",
	}
	done := make(chan error, 1)
	go func() {
		done <- s.ensureThread("thread-unmaterialized", "cc-connect/new/op-3")
	}()

	resumeLine := waitForWrittenJSONLineCount(t, stdin, 1)
	var resumeReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(resumeLine), &resumeReq); err != nil {
		t.Fatal(err)
	}
	if resumeReq.Method != "thread/resume" || resumeReq.Params["threadId"] != "thread-unmaterialized" {
		t.Fatalf("resume request = %#v", resumeReq)
	}
	s.pendingMu.Lock()
	resumeCh := s.pending[resumeReq.ID]
	s.pendingMu.Unlock()
	resumeCh <- rpcResponseEnvelope{ID: resumeReq.ID, Error: &rpcError{Code: -32600, Message: "no rollout found for thread id thread-unmaterialized"}}

	startLine := waitForWrittenJSONLineCount(t, stdin, 2)
	var startReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(startLine), &startReq); err != nil {
		t.Fatal(err)
	}
	if startReq.Method != "thread/start" || startReq.Params["threadSource"] != "cc-connect/new/op-3" {
		t.Fatalf("fresh start request = %#v", startReq)
	}
	s.pendingMu.Lock()
	startCh := s.pending[startReq.ID]
	s.pendingMu.Unlock()
	startCh <- rpcResponseEnvelope{ID: startReq.ID, Result: json.RawMessage(`{"thread":{"id":"thread-new"}}`)}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := s.CurrentSessionID(); got != "thread-new" {
		t.Fatalf("CurrentSessionID() = %q, want thread-new", got)
	}
	if !s.threadStartedFresh {
		t.Fatal("replacement thread was not marked fresh")
	}
}

func TestAppServerSession_EnsureThreadUnarchivesThenResumesSameThread(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	var refreshed []string
	s := &appServerSession{
		ctx:               ctx,
		stdin:             stdin,
		pending:           make(map[int64]chan rpcResponseEnvelope),
		refreshThreadInUI: func(threadID string) { refreshed = append(refreshed, threadID) },
	}
	done := make(chan error, 1)
	go func() { done <- s.ensureThread("thread-archived", "") }()

	resumeLine := waitForWrittenJSONLineCount(t, stdin, 1)
	var firstResume struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(resumeLine), &firstResume); err != nil {
		t.Fatal(err)
	}
	if firstResume.Method != "thread/resume" || firstResume.Params["threadId"] != "thread-archived" {
		t.Fatalf("first request = %#v", firstResume)
	}
	s.pendingMu.Lock()
	firstResumeCh := s.pending[firstResume.ID]
	s.pendingMu.Unlock()
	firstResumeCh <- rpcResponseEnvelope{ID: firstResume.ID, Error: &rpcError{
		Code:    -32600,
		Message: "session thread-archived is archived. Run codex unarchive thread-archived",
	}}

	unarchiveLine := waitForWrittenJSONLineCount(t, stdin, 2)
	var unarchiveReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(unarchiveLine), &unarchiveReq); err != nil {
		t.Fatal(err)
	}
	if unarchiveReq.Method != "thread/unarchive" || unarchiveReq.Params["threadId"] != "thread-archived" {
		t.Fatalf("unarchive request = %#v", unarchiveReq)
	}
	s.pendingMu.Lock()
	unarchiveCh := s.pending[unarchiveReq.ID]
	s.pendingMu.Unlock()
	unarchiveCh <- rpcResponseEnvelope{ID: unarchiveReq.ID, Result: json.RawMessage(`{}`)}

	retryLine := waitForWrittenJSONLineCount(t, stdin, 3)
	var retryResume struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(retryLine), &retryResume); err != nil {
		t.Fatal(err)
	}
	if retryResume.Method != "thread/resume" || retryResume.Params["threadId"] != "thread-archived" {
		t.Fatalf("retry request = %#v", retryResume)
	}
	s.pendingMu.Lock()
	retryCh := s.pending[retryResume.ID]
	s.pendingMu.Unlock()
	retryCh <- rpcResponseEnvelope{ID: retryResume.ID, Result: json.RawMessage(`{
		"cwd":"/tmp/project",
		"model":"gpt-5.4",
		"reasoningEffort":"high",
		"thread":{"id":"thread-archived"}
	}`)}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := s.CurrentSessionID(); got != "thread-archived" {
		t.Fatalf("CurrentSessionID() = %q, want thread-archived", got)
	}
	if !reflect.DeepEqual(refreshed, []string{"thread-archived"}) {
		t.Fatalf("UI refreshes = %#v, want archived thread refresh", refreshed)
	}
	if got := strings.Count(stdin.String(), "\n"); got != 3 {
		t.Fatalf("request count = %d, want 3 without thread/start", got)
	}
}

func TestAppServerSession_EnsureThreadUnarchiveFailurePreservesContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		ctx:     ctx,
		stdin:   stdin,
		pending: make(map[int64]chan rpcResponseEnvelope),
	}
	done := make(chan error, 1)
	go func() { done <- s.ensureThread("thread-archived", "") }()

	resumeLine := waitForWrittenJSONLineCount(t, stdin, 1)
	var resumeReq struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(resumeLine), &resumeReq); err != nil {
		t.Fatal(err)
	}
	s.pendingMu.Lock()
	resumeCh := s.pending[resumeReq.ID]
	s.pendingMu.Unlock()
	resumeCh <- rpcResponseEnvelope{ID: resumeReq.ID, Error: &rpcError{
		Code:    -32600,
		Message: "session thread-archived is archived. Run codex unarchive thread-archived",
	}}

	unarchiveLine := waitForWrittenJSONLineCount(t, stdin, 2)
	var unarchiveReq struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(unarchiveLine), &unarchiveReq); err != nil {
		t.Fatal(err)
	}
	if unarchiveReq.Method != "thread/unarchive" {
		t.Fatalf("second method = %q, want thread/unarchive", unarchiveReq.Method)
	}
	s.pendingMu.Lock()
	unarchiveCh := s.pending[unarchiveReq.ID]
	s.pendingMu.Unlock()
	unarchiveCh <- rpcResponseEnvelope{ID: unarchiveReq.ID, Error: &rpcError{Code: -32600, Message: "permission denied"}}

	err := <-done
	if !errors.Is(err, core.ErrSessionContextPreservationRequired) {
		t.Fatalf("ensureThread error = %v, want ErrSessionContextPreservationRequired", err)
	}
	if got := strings.Count(stdin.String(), "\n"); got != 2 {
		t.Fatalf("request count = %d, want 2 without thread/start", got)
	}
}

func TestCanonicalThreadWorkDir_ResolvesSymlink(t *testing.T) {
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	if got, want := canonicalThreadWorkDir(linkDir), canonicalThreadWorkDir(realDir); got != want {
		t.Fatalf("canonical symlink workdir = %q, want %q", got, want)
	}
}

func TestAppServerSession_FindThreadByRecoveryKeyPaginates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		ctx:     ctx,
		stdin:   stdin,
		pending: make(map[int64]chan rpcResponseEnvelope),
		workDir: "/tmp/project",
	}
	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := s.findThreadByRecoveryKey("cc-connect/new/op-paged")
		done <- result{id: id, err: err}
	}()

	firstLine := waitForWrittenJSONLineCount(t, stdin, 1)
	var firstReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(firstLine), &firstReq); err != nil {
		t.Fatal(err)
	}
	s.pendingMu.Lock()
	firstResponse := s.pending[firstReq.ID]
	s.pendingMu.Unlock()
	firstResponse <- rpcResponseEnvelope{ID: firstReq.ID, Result: json.RawMessage(`{
		"data":[{"id":"other","cwd":"/tmp/project","threadSource":"other"}],
		"nextCursor":"page-2"
	}`)}

	secondLine := waitForWrittenJSONLineCount(t, stdin, 2)
	var secondReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(secondLine), &secondReq); err != nil {
		t.Fatal(err)
	}
	if secondReq.Method != "thread/list" || secondReq.Params["cursor"] != "page-2" {
		t.Fatalf("second request = %#v", secondReq)
	}
	s.pendingMu.Lock()
	secondResponse := s.pending[secondReq.ID]
	s.pendingMu.Unlock()
	secondResponse <- rpcResponseEnvelope{ID: secondReq.ID, Result: json.RawMessage(`{
		"data":[{"id":"thread-paged","cwd":"/tmp/project","threadSource":"cc-connect/new/op-paged"}],
		"nextCursor":null
	}`)}

	got := <-done
	if got.err != nil || got.id != "thread-paged" {
		t.Fatalf("id = %q, err = %v", got.id, got.err)
	}
}

func TestAppServerSession_PrimeSessionCreatesVisibleTurnAndDiscardsBootstrapOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	origins := &desktopOriginTracker{}
	var refreshed []string
	s := &appServerSession{
		ctx:                ctx,
		stdin:              stdin,
		pending:            make(map[int64]chan rpcResponseEnvelope),
		events:             make(chan core.Event, 8),
		desktopOrigins:     origins,
		refreshThreadInUI:  func(threadID string) { refreshed = append(refreshed, threadID) },
		threadStartedFresh: true,
	}
	s.alive.Store(true)
	s.threadID.Store("thread-1")

	done := make(chan error, 1)
	go func() { done <- s.PrimeSession("新项目") }()

	line := waitForWrittenJSONLineCount(t, stdin, 1)
	var req struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		t.Fatalf("decode request %q: %v", line, err)
	}
	if req.Method != "turn/start" || req.Params["threadId"] != "thread-1" {
		t.Fatalf("request = %#v", req)
	}
	input, ok := req.Params["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v, want one bootstrap text item", req.Params["input"])
	}
	item, _ := input[0].(map[string]any)
	prompt, _ := item["text"].(string)
	if !strings.Contains(prompt, "新项目") || !strings.Contains(prompt, "不要调用工具") {
		t.Fatalf("bootstrap prompt = %q", prompt)
	}

	s.pendingMu.Lock()
	responseCh := s.pending[req.ID]
	s.pendingMu.Unlock()
	responseCh <- rpcResponseEnvelope{ID: req.ID, Result: json.RawMessage(`{"turn":{"id":"turn-1"}}`)}
	s.events <- core.Event{Type: core.EventText, Content: "会话已创建。"}
	s.events <- core.Event{Type: core.EventResult, SessionID: "thread-1", Done: true}

	if err := <-done; err != nil {
		t.Fatalf("PrimeSession() error = %v", err)
	}
	if len(s.events) != 0 {
		t.Fatalf("bootstrap events left in channel = %d, want 0", len(s.events))
	}
	if !reflect.DeepEqual(refreshed, []string{"thread-1"}) {
		t.Fatalf("refreshes = %#v, want post-turn refresh", refreshed)
	}
	if internal, pending := origins.classify("thread-1", "turn-1"); !internal || pending {
		t.Fatal("PrimeSession did not register its turn as cc-connect-originated")
	}

	writesAfterPrime := strings.Count(stdin.String(), "\n")
	if err := s.PrimeSession("新项目"); err != nil {
		t.Fatalf("duplicate PrimeSession() error = %v", err)
	}
	if got := strings.Count(stdin.String(), "\n"); got != writesAfterPrime {
		t.Fatalf("duplicate prime wrote another turn: got %d lines, want %d", got, writesAfterPrime)
	}
}

func TestAppServerSession_PrimeSessionSkipsThreadThatAlreadyHasTurns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	var refreshed []string
	s := &appServerSession{
		ctx:               ctx,
		stdin:             stdin,
		pending:           make(map[int64]chan rpcResponseEnvelope),
		events:            make(chan core.Event, 1),
		refreshThreadInUI: func(threadID string) { refreshed = append(refreshed, threadID) },
	}
	s.alive.Store(true)
	s.threadID.Store("thread-1")

	done := make(chan error, 1)
	go func() { done <- s.PrimeSession("新项目") }()

	line := waitForWrittenJSONLine(t, stdin)
	var req struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		t.Fatalf("decode request %q: %v", line, err)
	}
	if req.Method != "thread/read" || req.Params["threadId"] != "thread-1" || req.Params["includeTurns"] != true {
		t.Fatalf("request = %#v, want thread/read with turns", req)
	}
	s.pendingMu.Lock()
	responseCh := s.pending[req.ID]
	s.pendingMu.Unlock()
	responseCh <- rpcResponseEnvelope{ID: req.ID, Result: json.RawMessage(`{"thread":{"id":"thread-1","turns":[{"id":"turn-existing"}]}}`)}

	if err := <-done; err != nil {
		t.Fatalf("PrimeSession() error = %v", err)
	}
	if got := strings.Count(stdin.String(), "\n"); got != 1 {
		t.Fatalf("request count = %d, want only thread/read", got)
	}
	if !reflect.DeepEqual(refreshed, []string{"thread-1"}) {
		t.Fatalf("refreshes = %#v, want existing thread refresh", refreshed)
	}
}

func TestAppServerSession_PrimeSessionInterruptsUnexpectedPermissionTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		ctx:     ctx,
		stdin:   stdin,
		pending: make(map[int64]chan rpcResponseEnvelope),
		events:  make(chan core.Event, 8),
	}
	s.alive.Store(true)
	s.threadID.Store("thread-1")

	done := make(chan error, 1)
	go func() { done <- s.PrimeSession("新项目") }()

	readLine := waitForWrittenJSONLineCount(t, stdin, 1)
	var readReq struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(readLine), &readReq); err != nil {
		t.Fatalf("decode thread/read request: %v", err)
	}
	s.pendingMu.Lock()
	readCh := s.pending[readReq.ID]
	s.pendingMu.Unlock()
	readCh <- rpcResponseEnvelope{ID: readReq.ID, Result: json.RawMessage(`{"thread":{"id":"thread-1","turns":[]}}`)}

	startLine := waitForWrittenJSONLineCount(t, stdin, 2)
	var startReq struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(startLine), &startReq); err != nil {
		t.Fatalf("decode turn/start request: %v", err)
	}
	s.pendingMu.Lock()
	startCh := s.pending[startReq.ID]
	s.pendingMu.Unlock()
	startCh <- rpcResponseEnvelope{ID: startReq.ID, Result: json.RawMessage(`{"turn":{"id":"turn-1"}}`)}
	s.events <- core.Event{Type: core.EventPermissionRequest, RequestID: "permission-1"}

	interruptLine := waitForWrittenJSONLineCount(t, stdin, 3)
	var interruptReq struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(interruptLine), &interruptReq); err != nil {
		t.Fatalf("decode turn/interrupt request: %v", err)
	}
	if interruptReq.Method != "turn/interrupt" || interruptReq.Params["threadId"] != "thread-1" || interruptReq.Params["turnId"] != "turn-1" {
		t.Fatalf("interrupt request = %#v", interruptReq)
	}
	s.pendingMu.Lock()
	interruptCh := s.pending[interruptReq.ID]
	s.pendingMu.Unlock()
	interruptCh <- rpcResponseEnvelope{ID: interruptReq.ID, Result: json.RawMessage(`{}`)}
	s.events <- core.Event{Type: core.EventResult, SessionID: "thread-1", Done: true}

	if err := <-done; err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("PrimeSession() error = %v, want permission failure", err)
	}
}

func TestAppServerSession_ArchiveSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{ctx: ctx, stdin: stdin, pending: make(map[int64]chan rpcResponseEnvelope)}
	done := make(chan error, 1)
	go func() { done <- s.ArchiveSession("thread-old") }()

	line := waitForWrittenJSONLine(t, stdin)
	var req struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		t.Fatalf("decode request %q: %v", line, err)
	}
	if req.Method != "thread/archive" || req.Params["threadId"] != "thread-old" {
		t.Fatalf("request = %#v", req)
	}
	s.pendingMu.Lock()
	responseCh := s.pending[req.ID]
	s.pendingMu.Unlock()
	responseCh <- rpcResponseEnvelope{ID: req.ID, Result: json.RawMessage(`{}`)}
	if err := <-done; err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}
}

func TestRefreshCodexAppThreadReloadsPersistedTitle(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("cc-connect-ipc-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	received := make(chan []map[string]any, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		messages := make([]map[string]any, 0, 2)
		for len(messages) < 2 {
			header := make([]byte, 4)
			if _, err := io.ReadFull(conn, header); err != nil {
				return
			}
			length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16 | int(header[3])<<24
			body := make([]byte, length)
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
			var message map[string]any
			if json.Unmarshal(body, &message) == nil {
				messages = append(messages, message)
			}
		}
		received <- messages
	}()

	if err := refreshCodexAppThread(socketPath, "thread-1"); err != nil {
		t.Fatalf("refreshCodexAppThread: %v", err)
	}
	select {
	case messages := <-received:
		wantMethods := []string{"thread-archived", "thread-unarchived"}
		wantVersions := []float64{2, 1}
		for i, message := range messages {
			if message["type"] != "broadcast" || message["method"] != wantMethods[i] || message["version"] != wantVersions[i] {
				t.Fatalf("message[%d] = %#v", i, message)
			}
			params, ok := message["params"].(map[string]any)
			if !ok || params["hostId"] != "local" || params["conversationId"] != "thread-1" {
				t.Fatalf("params[%d] = %#v", i, message["params"])
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Codex App thread reload broadcasts")
	}
}

func TestAppServerSession_ConnectUsesSharedDaemonProxyWhenEnabled(t *testing.T) {
	binDir := t.TempDir()
	script := filepath.Join(binDir, "fake-codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 6\nwhile IFS= read -r _; do :; done\n"), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("CODEX_APP_SERVER_USE_LOCAL_DAEMON", "1")

	ctx, cancel := context.WithCancel(context.Background())
	s := &appServerSession{
		ctx:              ctx,
		cancel:           cancel,
		workDir:          t.TempDir(),
		cliBin:           script,
		cliExtraArgs:     []string{"--test-prefix"},
		events:           make(chan core.Event, 1),
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}
	if err := s.connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer s.Close()

	s.procMu.Lock()
	if s.cmd == nil || s.cmd.Process == nil {
		s.procMu.Unlock()
		t.Fatal("connect returned without a started Codex process")
	}
	got := append([]string(nil), s.cmd.Args[1:]...)
	s.procMu.Unlock()
	want := []string{"--test-prefix", "app-server", "proxy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codex args = %q, want %q", got, want)
	}
}

func TestAppServerSessionSendKeepsDesktopOriginWhenTurnStartResponseIsLost(t *testing.T) {
	a, transcript, threadID := newCompatibilityPollFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	s := &appServerSession{
		ctx:            ctx,
		cancel:         cancel,
		stdin:          &cancelAfterWriteCloser{cancel: cancel},
		events:         make(chan core.Event, 1),
		pending:        make(map[int64]chan rpcResponseEnvelope),
		desktopOrigins: &a.desktopOrigins,
	}
	s.alive.Store(true)
	s.threadID.Store(threadID)

	if err := s.Send("internal-only", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() error = %v, want context canceled after request write", err)
	}

	s.handleNotification("turn/started", json.RawMessage(`{"threadId":"`+threadID+`","turn":{"id":"turn-internal"}}`))
	a.desktopOrigins.mu.Lock()
	state := a.desktopOrigins.sessions[threadID]
	_, registered := state.turns["turn-internal"]
	pending := len(state.pending)
	a.desktopOrigins.mu.Unlock()
	if !registered || pending != 0 {
		t.Fatalf("origin after lost response = registered %v, pending %d; want reconciled internal turn", registered, pending)
	}

	appendTranscript(t, transcript,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-internal"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"internal-only"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"internal-answer"}}`,
	)
	if events, err := a.PollExternalConversation(context.Background(), threadID); err != nil || len(events) != 0 {
		t.Fatalf("events after lost turn/start response = %#v, %v; want internal turn isolated", events, err)
	}
}

func TestAppServerSessionSendCancelsDesktopOriginWhenTurnStartIsRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &lockedWriteCloser{}
	origins := &desktopOriginTracker{}
	s := &appServerSession{
		ctx:            ctx,
		cancel:         cancel,
		stdin:          stdin,
		events:         make(chan core.Event, 1),
		pending:        make(map[int64]chan rpcResponseEnvelope),
		desktopOrigins: origins,
	}
	s.alive.Store(true)
	s.threadID.Store("thread-rejected")

	done := make(chan error, 1)
	go func() { done <- s.Send("internal-only", nil, nil) }()

	line := waitForWrittenJSONLine(t, stdin)
	var request struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		t.Fatalf("decode turn/start request: %v", err)
	}
	s.handleResponse(rpcResponseEnvelope{ID: request.ID, Error: &rpcError{Message: "rejected"}})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Send() error = %v, want rejected turn/start error", err)
	}
	if internal, pending := origins.classify("thread-rejected", "turn-any"); internal || pending {
		t.Fatalf("origin after explicit rejection = internal %v, pending %v; want canceled", internal, pending)
	}
}

var _ interface {
	GetUsage(context.Context) (*core.UsageReport, error)
} = (*appServerSession)(nil)

var _ interface {
	GetContextUsage() *core.ContextUsage
} = (*appServerSession)(nil)

var _ core.AgentSessionNamer = (*appServerSession)(nil)

type lockedWriteCloser struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type cancelAfterWriteCloser struct {
	cancel context.CancelFunc
	once   sync.Once
}

func (w *cancelAfterWriteCloser) Write(p []byte) (int, error) {
	w.once.Do(w.cancel)
	return len(p), nil
}

func (w *cancelAfterWriteCloser) Close() error { return nil }

func (w *lockedWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriteCloser) Close() error { return nil }

func (w *lockedWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

var _ io.WriteCloser = (*lockedWriteCloser)(nil)

func serverRequestProbe(t *testing.T, idJSON, method string, params any) map[string]json.RawMessage {
	t.Helper()
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	methodJSON, err := json.Marshal(method)
	if err != nil {
		t.Fatalf("marshal method: %v", err)
	}
	return map[string]json.RawMessage{
		"id":     json.RawMessage(idJSON),
		"method": methodJSON,
		"params": paramsJSON,
	}
}

func waitForWrittenJSONLine(t *testing.T, w *lockedWriteCloser) string {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for JSON response, buffer=%q", w.String())
		case <-ticker.C:
			for _, line := range strings.Split(w.String(), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					return line
				}
			}
		}
	}
}

func waitForWrittenJSONLineCount(t *testing.T, w *lockedWriteCloser, want int) string {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for JSON line %d, buffer=%q", want, w.String())
		case <-ticker.C:
			var lines []string
			for _, line := range strings.Split(w.String(), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					lines = append(lines, line)
				}
			}
			if len(lines) >= want {
				return lines[want-1]
			}
		}
	}
}
