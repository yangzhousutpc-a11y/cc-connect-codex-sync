package codex

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestProbeCodexVersionSuccessFailureAndTimeout(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		path := writeExecutable(t, "#!/bin/sh\nprintf 'codex-cli 9.9.9\\n'\n")
		got, err := probeCodexVersion(context.Background(), path, nil)
		if err != nil || got != "codex-cli 9.9.9" {
			t.Fatalf("probe = %q, %v", got, err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		path := writeExecutable(t, "#!/bin/sh\nexit 7\n")
		if _, err := probeCodexVersion(context.Background(), path, nil); err == nil {
			t.Fatal("probe succeeded, want failure")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		path := writeExecutable(t, "#!/bin/sh\nexec sleep 2\n")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := probeCodexVersion(ctx, path, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("probe error = %v, want deadline exceeded", err)
		}
	})
}

func TestNewKeepsStartingWhenVersionProbeFails(t *testing.T) {
	path := writeExecutable(t, "#!/bin/sh\nexit 7\n")
	created, err := New(map[string]any{"cmd": path, "desktop_live_sync": true})
	if err != nil {
		t.Fatalf("New returned version probe error: %v", err)
	}
	snap := created.(*Agent).compatibility.snapshot()
	if snap.Status != compatibilityIdle || snap.Reason != "version_probe_failed" {
		t.Fatalf("startup snapshot = %#v", snap)
	}
}

func TestCompatibilitySentinelTransitionsAndGracePeriod(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	s := newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now })
	assertCompatibilityState(t, s, compatibilityIdle, "no_active_routes")

	s.setRoutes(1, true)
	assertCompatibilityState(t, s, compatibilityWaiting, "route_observation_pending")
	s.transcriptMissing()
	assertCompatibilityState(t, s, compatibilityWaiting, "transcript_pending")

	now = now.Add(compatibilityTranscriptGrace + time.Second)
	s.transcriptMissing()
	assertCompatibilityState(t, s, compatibilityDegraded, "transcript_missing")

	s.incompatible("unsafe_turn_identity")
	assertCompatibilityState(t, s, compatibilityIncompatible, "unsafe_turn_identity")
	s.supportedTurn()
	snap := s.snapshot()
	if snap.Status != compatibilityHealthy || !snap.ObservedSupportedTurn || snap.LastSuccessAt.IsZero() {
		t.Fatalf("recovered snapshot = %#v", snap)
	}

	s.setRoutes(0, true)
	assertCompatibilityState(t, s, compatibilityIdle, "no_active_routes")
}

func TestCompatibilitySentinelChangedRouteGetsFreshTranscriptGraceAfterHealthy(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	s := newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now })
	s.setRoutes(1, true)
	s.supportedTurn()
	if !s.snapshot().ObservedSupportedTurn {
		t.Fatal("supported turn observation was not retained")
	}

	s.setRoutes(2, true)
	s.transcriptMissing()
	assertCompatibilityState(t, s, compatibilityWaiting, "transcript_pending")

	now = now.Add(compatibilityTranscriptGrace + time.Second)
	s.transcriptMissing()
	assertCompatibilityState(t, s, compatibilityDegraded, "transcript_missing")
}

func TestCompatibilitySentinelRouteUpdateKeepsHealthyWithActiveHealthyRoute(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	s := newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now })
	s.setRoutes(1, true)
	s.supportedTurn()
	before := s.snapshot()

	now = now.Add(time.Second)
	s.setRoutesWithActiveHealthyRouteState(2, true, true)
	assertCompatibilityState(t, s, compatibilityHealthy, "supported_turn")
	after := s.snapshot()
	if after.ActiveRoutes != 2 || !after.LastSuccessAt.Equal(before.LastSuccessAt) || after.ObservedSupportedTurn != before.ObservedSupportedTurn {
		t.Fatalf("route update changed healthy observation: before=%#v after=%#v", before, after)
	}
}

func TestCompatibilitySentinelInitialRouteSupportedTurnMissingTranscriptDegradesImmediately(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	s := newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now })

	s.setRoutes(1, true)
	s.supportedTurn()
	s.transcriptMissing()

	assertCompatibilityState(t, s, compatibilityDegraded, "transcript_missing")
}

func TestCompatibilitySentinelChangedRouteKeepsGraceAfterPriorRouteSupportedTurn(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	s := newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now })

	s.setRoutes(1, true)
	s.supportedTurn()
	assertCompatibilityState(t, s, compatibilityHealthy, "supported_turn")

	now = now.Add(time.Second)
	s.setRoutes(2, true)
	routeChangedAt := now
	now = now.Add(time.Second)
	s.supportedTurn() // A prior route can finish while the new route is still in its grace window.
	assertCompatibilityState(t, s, compatibilityHealthy, "supported_turn")
	snap := s.snapshot()
	if !snap.LastSuccessAt.Equal(now) || !snap.ObservedSupportedTurn {
		t.Fatalf("supported turn did not update aggregate observation: %#v", snap)
	}
	if !s.waitFrom.Equal(routeChangedAt) {
		t.Fatalf("waitFrom = %v, want original route-change time %v", s.waitFrom, routeChangedAt)
	}

	s.transcriptMissing()
	assertCompatibilityState(t, s, compatibilityWaiting, "transcript_pending")

	now = now.Add(compatibilityTranscriptGrace + time.Second)
	s.transcriptMissing()
	assertCompatibilityState(t, s, compatibilityDegraded, "transcript_missing")
}

func TestCompatibilitySentinelDisabled(t *testing.T) {
	s := newCompatibilitySentinel(false, "codex-cli 1.0", time.Now)
	s.setRoutes(3, true)
	s.incompatible("unsafe_turn_identity")
	assertCompatibilityState(t, s, compatibilityDisabled, "desktop_live_sync_disabled")
}

func TestCompatibilitySentinelDeduplicatesAndRedactsLogs(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	s := newCompatibilitySentinel(true, "codex-cli 1.0", time.Now)
	s.logInitialState()
	s.setRoutes(1, true)
	logs.Reset()
	s.incompatible("unsafe_turn_identity")
	s.incompatible("unsafe_turn_identity")
	s.supportedTurn()
	s.supportedTurn()

	got := logs.String()
	if strings.Count(got, "codex compatibility sentinel state changed") != 2 {
		t.Fatalf("state change logs = %q", got)
	}
	for _, forbidden := range []string{"secret-message", "oc_group", "ou_user", "019f-session"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("log contains forbidden value %q: %s", forbidden, got)
		}
	}
}

func TestCompatibilityLogHandlerCanSnapshotWithoutDeadlock(t *testing.T) {
	const childEnv = "CC_CONNECT_COMPATIBILITY_LOG_SNAPSHOT_CHILD"
	if os.Getenv(childEnv) == "1" {
		s := newCompatibilitySentinel(true, "codex-cli 1.0", time.Now)
		previous := slog.Default()
		slog.SetDefault(slog.New(snapshotOnLogHandler{sentinel: s}))
		t.Cleanup(func() { slog.SetDefault(previous) })

		s.logInitialState()
		s.setRoutes(1, true)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCompatibilityLogHandlerCanSnapshotWithoutDeadlock$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child test: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child test failed: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		killErr := cmd.Process.Kill()
		waitErr := <-done
		if killErr != nil && waitErr == nil {
			return
		}
		if killErr != nil {
			t.Fatalf("kill timed-out child test: %v\n%s", killErr, output.String())
		}
		if waitErr == nil {
			t.Fatal("timed-out child test exited without an error")
		}
		t.Fatalf("log handler snapshot timed out; likely sentinel lock re-entry deadlock\n%s", output.String())
	}
}

func TestCompatibilityAgentLogHandlerCanReenterDesktopPollAPIsWithoutDeadlock(t *testing.T) {
	const childEnv = "CC_CONNECT_COMPATIBILITY_AGENT_REENTRY_CHILD"
	if os.Getenv(childEnv) == "1" {
		codexHome := t.TempDir()
		threadID := "thread-log-handler-reentry"
		a := &Agent{
			codexHome:       codexHome,
			desktopLiveSync: true,
			compatibility:   newCompatibilitySentinel(true, "codex-cli 1.0", time.Now),
		}
		a.compatibility.logInitialState()

		previous := slog.Default()
		t.Cleanup(func() { slog.SetDefault(previous) })

		var pollErr error
		slog.SetDefault(slog.New(&callbackOnceLogHandler{callback: func() {
			_, pollErr = a.PollExternalConversation(context.Background(), threadID)
		}}))
		a.SetExternalConversationRoutes([]string{threadID})
		if pollErr != nil {
			t.Fatalf("poll callback: %v", pollErr)
		}

		a.compatibility.transcriptFound()
		slog.SetDefault(slog.New(&callbackOnceLogHandler{callback: func() {
			a.SetExternalConversationRoutes([]string{threadID})
		}}))
		if _, err := a.PollExternalConversation(context.Background(), threadID); err != nil {
			t.Fatalf("poll triggering route callback: %v", err)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCompatibilityAgentLogHandlerCanReenterDesktopPollAPIsWithoutDeadlock$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child test: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child test failed: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		killErr := cmd.Process.Kill()
		waitErr := <-done
		if killErr != nil {
			t.Fatalf("kill timed-out child test: %v\n%s", killErr, output.String())
		}
		if waitErr == nil {
			t.Fatal("timed-out child test exited without an error")
		}
		t.Fatalf("log handler API callback timed out; likely desktopPollMu re-entry deadlock\n%s", output.String())
	}
}

type snapshotOnLogHandler struct {
	sentinel *compatibilitySentinel
}

type callbackOnceLogHandler struct {
	called   atomic.Bool
	callback func()
}

func (h *callbackOnceLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *callbackOnceLogHandler) Handle(context.Context, slog.Record) error {
	if h.called.CompareAndSwap(false, true) {
		h.callback()
	}
	return nil
}

func (h *callbackOnceLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *callbackOnceLogHandler) WithGroup(string) slog.Handler {
	return h
}

func (h snapshotOnLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h snapshotOnLogHandler) Handle(context.Context, slog.Record) error {
	h.sentinel.snapshotValue()
	return nil
}

func (h snapshotOnLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h snapshotOnLogHandler) WithGroup(string) slog.Handler {
	return h
}

func TestCompatibilityDoctorStatusMapping(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	s := newCompatibilitySentinel(true, "codex-cli 1.0", func() time.Time { return now })
	a := &Agent{compatibility: s}

	s.setRoutes(1, true)
	if got := a.DoctorChecks(context.Background())[0]; got.Status != core.DoctorWarn || got.DetailKey != core.MsgDoctorCodexCompatibilityWaiting {
		t.Fatalf("waiting doctor = %#v", got)
	}
	s.supportedTurn()
	if got := a.DoctorChecks(context.Background())[0]; got.Status != core.DoctorPass || got.DetailKey != core.MsgDoctorCodexCompatibilityHealthy {
		t.Fatalf("healthy doctor = %#v", got)
	}
	s.incompatible("unsafe_turn_identity")
	if got := a.DoctorChecks(context.Background())[0]; got.Status != core.DoctorFail || got.DetailKey != core.MsgDoctorCodexCompatibilityIncompatible {
		t.Fatalf("incompatible doctor = %#v", got)
	}
}

func TestCompatibilityDoctorWarnsWhenVersionProbeFails(t *testing.T) {
	s := newCompatibilitySentinel(true, "", time.Now)
	s.recordVersionProbe("", errors.New("probe failed"))
	a := &Agent{compatibility: s}
	got := a.DoctorChecks(context.Background())[0]
	if got.Status != core.DoctorWarn || got.DetailKey != core.MsgDoctorCodexCompatibilityDegraded {
		t.Fatalf("version failure doctor = %#v", got)
	}
}

func writeExecutable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertCompatibilityState(t *testing.T, s *compatibilitySentinel, want compatibilityState, reason string) {
	t.Helper()
	got := s.snapshot()
	if got.Status != want || got.Reason != reason {
		t.Fatalf("snapshot = %#v, want status %q reason %q", got, want, reason)
	}
}
