package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

type compatibilityState string

const (
	compatibilityUninitialized compatibilityState = "uninitialized"
	compatibilityDisabled      compatibilityState = "disabled"
	compatibilityIdle          compatibilityState = "idle"
	compatibilityWaiting       compatibilityState = "waiting"
	compatibilityHealthy       compatibilityState = "healthy"
	compatibilityDegraded      compatibilityState = "degraded"
	compatibilityIncompatible  compatibilityState = "incompatible"

	compatibilityTranscriptGrace = 2 * time.Minute
	codexVersionProbeTimeout     = 2 * time.Second
)

type compatibilitySnapshot struct {
	Status                compatibilityState
	Reason                string
	CodexVersion          string
	ActiveRoutes          int
	LastSuccessAt         time.Time
	LastIssueAt           time.Time
	ObservedSupportedTurn bool
}

type compatibilitySentinel struct {
	mu                           sync.RWMutex
	enabled                      bool
	initialized                  bool
	now                          func() time.Time
	waitFrom                     time.Time
	routeChangeGraceAfterHealthy bool
	current                      compatibilitySnapshot
}

type compatibilityStateChangeLog struct {
	from         compatibilityState
	to           compatibilityState
	reason       string
	codexVersion string
	activeRoutes int
}

func newCompatibilitySentinel(enabled bool, version string, now func() time.Time) *compatibilitySentinel {
	if now == nil {
		now = time.Now
	}
	state, reason := compatibilityIdle, "no_active_routes"
	if !enabled {
		state, reason = compatibilityDisabled, "desktop_live_sync_disabled"
	}
	return &compatibilitySentinel{
		enabled: enabled,
		now:     now,
		current: compatibilitySnapshot{
			Status:       state,
			Reason:       reason,
			CodexVersion: displayCodexVersion(version),
		},
	}
}

func probeCodexVersion(ctx context.Context, binary string, extraArgs []string) (string, error) {
	args := append(append([]string(nil), extraArgs...), "--version")
	out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("codex version probe: %w", err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", errors.New("codex version probe returned empty output")
	}
	if len(version) > 80 {
		version = version[:80]
	}
	return version, nil
}

func displayCodexVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "unknown"
	}
	if len(version) > 80 {
		return version[:80]
	}
	return version
}

func (s *compatibilitySentinel) recordVersionProbe(version string, err error) {
	if s == nil {
		return
	}
	var logRecord *compatibilityStateChangeLog
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		logCompatibilityStateChange(logRecord)
	}()
	if err == nil {
		s.current.CodexVersion = displayCodexVersion(version)
		return
	}
	s.current.LastIssueAt = s.now()
	logRecord = s.transitionLocked(s.current.Status, "version_probe_failed")
}

func (s *compatibilitySentinel) logInitialState() {
	if s == nil {
		return
	}
	var logRecord *compatibilityStateChangeLog
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		logCompatibilityStateChange(logRecord)
	}()
	if s.initialized {
		return
	}
	s.initialized = true
	logRecord = s.newStateChangeLogLocked(compatibilityUninitialized, s.current.Status, s.current.Reason)
}

func (s *compatibilitySentinel) setRoutes(count int, changed bool) {
	logCompatibilityStateChange(s.setRoutesState(count, changed))
}

func (s *compatibilitySentinel) setRoutesState(count int, changed bool) *compatibilityStateChangeLog {
	return s.setRoutesWithActiveHealthyRouteState(count, changed, false)
}

func (s *compatibilitySentinel) setRoutesWithActiveHealthyRouteState(count int, changed, hasActiveHealthyRoute bool) *compatibilityStateChangeLog {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if count < 0 {
		count = 0
	}
	hadActiveRoutes := s.current.ActiveRoutes > 0
	if count == 0 {
		s.waitFrom = time.Time{}
		s.routeChangeGraceAfterHealthy = false
		s.current.ActiveRoutes = count
		if !s.enabled {
			return nil
		}
		return s.transitionLocked(compatibilityIdle, "no_active_routes")
	}
	s.current.ActiveRoutes = count
	if !s.enabled {
		return nil
	}
	if changed {
		if s.current.Status == compatibilityHealthy && hasActiveHealthyRoute {
			return nil
		}
		s.waitFrom = s.now()
		s.routeChangeGraceAfterHealthy = hadActiveRoutes && s.current.ObservedSupportedTurn
		return s.transitionLocked(compatibilityWaiting, "route_observation_pending")
	}
	return nil
}

func (s *compatibilitySentinel) transcriptFound() {
	logCompatibilityStateChange(s.transcriptFoundState())
}

func (s *compatibilitySentinel) transcriptFoundState() *compatibilityStateChangeLog {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.current.ActiveRoutes == 0 || s.current.Status != compatibilityWaiting {
		return nil
	}
	if s.waitFrom.IsZero() {
		s.waitFrom = s.now()
	}
	return s.transitionLocked(compatibilityWaiting, "supported_turn_pending")
}

func (s *compatibilitySentinel) transcriptMissing() {
	logCompatibilityStateChange(s.transcriptMissingState())
}

func (s *compatibilitySentinel) transcriptMissingState() *compatibilityStateChangeLog {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.current.ActiveRoutes == 0 {
		return nil
	}
	now := s.now()
	if s.waitFrom.IsZero() {
		if s.current.ObservedSupportedTurn {
			s.current.LastIssueAt = now
			return s.transitionLocked(compatibilityDegraded, "transcript_missing")
		}
		s.waitFrom = now
	}
	if now.Sub(s.waitFrom) < compatibilityTranscriptGrace {
		return s.transitionLocked(compatibilityWaiting, "transcript_pending")
	}
	s.waitFrom = time.Time{}
	s.routeChangeGraceAfterHealthy = false
	s.current.LastIssueAt = now
	return s.transitionLocked(compatibilityDegraded, "transcript_missing")
}

func (s *compatibilitySentinel) degraded(reason string) {
	logCompatibilityStateChange(s.degradedState(reason))
}

func (s *compatibilitySentinel) degradedState(reason string) *compatibilityStateChangeLog {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.current.ActiveRoutes == 0 {
		return nil
	}
	s.current.LastIssueAt = s.now()
	return s.transitionLocked(compatibilityDegraded, reason)
}

func (s *compatibilitySentinel) incompatible(reason string) {
	logCompatibilityStateChange(s.incompatibleState(reason))
}

func (s *compatibilitySentinel) incompatibleState(reason string) *compatibilityStateChangeLog {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.current.ActiveRoutes == 0 {
		return nil
	}
	s.current.LastIssueAt = s.now()
	return s.transitionLocked(compatibilityIncompatible, reason)
}

func (s *compatibilitySentinel) supportedTurn() {
	logCompatibilityStateChange(s.supportedTurnState())
}

func (s *compatibilitySentinel) supportedTurnState() *compatibilityStateChangeLog {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.current.ActiveRoutes == 0 {
		return nil
	}
	now := s.now()
	if s.waitFrom.IsZero() || !now.Before(s.waitFrom.Add(compatibilityTranscriptGrace)) || !s.routeChangeGraceAfterHealthy {
		s.waitFrom = time.Time{}
		s.routeChangeGraceAfterHealthy = false
	}
	s.current.LastSuccessAt = now
	s.current.ObservedSupportedTurn = true
	return s.transitionLocked(compatibilityHealthy, "supported_turn")
}

func (s *compatibilitySentinel) transitionLocked(next compatibilityState, reason string) *compatibilityStateChangeLog {
	previousState, previousReason := s.current.Status, s.current.Reason
	s.current.Status, s.current.Reason = next, reason
	if previousState == next && previousReason == reason {
		return nil
	}
	if s.initialized {
		return s.newStateChangeLogLocked(previousState, next, reason)
	}
	return nil
}

func (s *compatibilitySentinel) newStateChangeLogLocked(from, to compatibilityState, reason string) *compatibilityStateChangeLog {
	return &compatibilityStateChangeLog{
		from:         from,
		to:           to,
		reason:       reason,
		codexVersion: s.current.CodexVersion,
		activeRoutes: s.current.ActiveRoutes,
	}
}

func logCompatibilityStateChange(record *compatibilityStateChangeLog) {
	if record == nil {
		return
	}
	args := []any{
		"from", record.from,
		"to", record.to,
		"reason", record.reason,
		"codex_version", record.codexVersion,
		"active_routes", record.activeRoutes,
	}
	if record.to == compatibilityDegraded || record.to == compatibilityIncompatible || record.reason == "version_probe_failed" {
		slog.Warn("codex compatibility sentinel state changed", args...)
		return
	}
	slog.Info("codex compatibility sentinel state changed", args...)
}

func (s *compatibilitySentinel) snapshotValue() compatibilitySnapshot {
	if s == nil {
		return compatibilitySnapshot{Status: compatibilityDisabled, Reason: "desktop_live_sync_disabled", CodexVersion: "unknown"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *compatibilitySentinel) snapshot() compatibilitySnapshot {
	return s.snapshotValue()
}

func (a *Agent) DoctorChecks(_ context.Context) []core.DoctorCheckResult {
	snap := a.compatibility.snapshotValue()
	result := core.DoctorCheckResult{NameKey: core.MsgDoctorCodexCompatibilityName}
	version := displayCodexVersion(snap.CodexVersion)
	formatTime := func(value time.Time) string {
		if value.IsZero() {
			return "unknown"
		}
		return value.Format("2006-01-02 15:04:05 MST")
	}
	if snap.Reason == "version_probe_failed" {
		result.Status = core.DoctorWarn
		result.DetailKey = core.MsgDoctorCodexCompatibilityDegraded
		result.DetailArgs = []any{snap.Reason, formatTime(snap.LastIssueAt), version}
		return []core.DoctorCheckResult{result}
	}
	switch snap.Status {
	case compatibilityDisabled:
		result.Status, result.DetailKey, result.DetailArgs = core.DoctorPass, core.MsgDoctorCodexCompatibilityDisabled, []any{version}
	case compatibilityIdle:
		result.Status, result.DetailKey, result.DetailArgs = core.DoctorPass, core.MsgDoctorCodexCompatibilityIdle, []any{version}
	case compatibilityWaiting:
		result.Status, result.DetailKey, result.DetailArgs = core.DoctorWarn, core.MsgDoctorCodexCompatibilityWaiting, []any{snap.ActiveRoutes, version}
	case compatibilityHealthy:
		result.Status, result.DetailKey, result.DetailArgs = core.DoctorPass, core.MsgDoctorCodexCompatibilityHealthy, []any{formatTime(snap.LastSuccessAt), version}
	case compatibilityIncompatible:
		result.Status, result.DetailKey, result.DetailArgs = core.DoctorFail, core.MsgDoctorCodexCompatibilityIncompatible, []any{snap.Reason, formatTime(snap.LastIssueAt), version}
	default:
		result.Status, result.DetailKey, result.DetailArgs = core.DoctorWarn, core.MsgDoctorCodexCompatibilityDegraded, []any{snap.Reason, formatTime(snap.LastIssueAt), version}
	}
	return []core.DoctorCheckResult{result}
}
