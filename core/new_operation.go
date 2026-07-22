package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const newOperationSnapshotVersion = 1

type NewOperationStep string

const (
	NewOperationAccepted            NewOperationStep = "accepted"
	NewOperationConversationSpawned NewOperationStep = "conversation_spawned"
	NewOperationSessionBound        NewOperationStep = "session_bound"
	NewOperationAgentSessionStarted NewOperationStep = "agent_session_started"
	NewOperationNameSynced          NewOperationStep = "name_synced"
	NewOperationActivated           NewOperationStep = "activated"
	NewOperationCompleted           NewOperationStep = "completed"
)

type NewOperationMode string

const (
	NewOperationModeExternalConversation NewOperationMode = "external_conversation"
	NewOperationModeInPlaceSession       NewOperationMode = "in_place_session"
)

type NewOperationStatus string

const (
	NewOperationRunning         NewOperationStatus = "running"
	NewOperationRetryWait       NewOperationStatus = "retry_wait"
	NewOperationManualAction    NewOperationStatus = "manual_action_required"
	NewOperationStatusCompleted NewOperationStatus = "completed"
)

type NewOperation struct {
	ID                   string             `json:"id"`
	Project              string             `json:"project"`
	Platform             string             `json:"platform"`
	Mode                 NewOperationMode   `json:"mode,omitempty"`
	SourceSessionKey     string             `json:"source_session_key"`
	SourceLocalSessionID string             `json:"source_local_session_id,omitempty"`
	SourceMessageID      string             `json:"source_message_id"`
	UserID               string             `json:"user_id"`
	Name                 string             `json:"name"`
	WorkspaceDir         string             `json:"workspace_dir,omitempty"`
	Step                 NewOperationStep   `json:"step"`
	Status               NewOperationStatus `json:"status"`
	TargetSessionKey     string             `json:"target_session_key,omitempty"`
	LocalSessionID       string             `json:"local_session_id,omitempty"`
	AgentSessionID       string             `json:"agent_session_id,omitempty"`
	AgentRecoveryKey     string             `json:"agent_recovery_key,omitempty"`
	StepAttempts         int                `json:"step_attempts,omitempty"`
	NextAttemptAt        time.Time          `json:"next_attempt_at,omitempty"`
	LastError            string             `json:"last_error,omitempty"`
	SourceNotified       bool               `json:"source_notified,omitempty"`
	TargetNotified       bool               `json:"target_notified,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	UpdatedAt            time.Time          `json:"updated_at"`
	CompletedAt          time.Time          `json:"completed_at,omitempty"`
}

// EffectiveMode preserves the behavior of snapshots written before Mode was
// introduced: an empty mode represents an external platform conversation.
func (o NewOperation) EffectiveMode() NewOperationMode {
	if o.Mode == "" {
		return NewOperationModeExternalConversation
	}
	return o.Mode
}

func (o NewOperation) isExternalConversation() bool {
	return o.EffectiveMode() == NewOperationModeExternalConversation
}

type newOperationSnapshot struct {
	Version    int                     `json:"version"`
	Operations map[string]NewOperation `json:"operations"`
}

type NewOperationStore struct {
	mu                    sync.RWMutex
	path                  string
	operations            map[string]NewOperation
	provisionalTargetKeys map[string]newOperationTargetRecovery
	changed               chan struct{}
	now                   func() time.Time
}

type newOperationTargetRecovery struct {
	operationID string
	recoveryKey string
}

func NewOperationID(project, sourceSessionKey, sourceMessageID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{project, sourceSessionKey, sourceMessageID}, "\x00")))
	return fmt.Sprintf("new-%x", sum[:16])
}

func NewNewOperationStore(path string) (*NewOperationStore, error) {
	store := &NewOperationStore{
		path:                  path,
		operations:            make(map[string]NewOperation),
		provisionalTargetKeys: make(map[string]newOperationTargetRecovery),
		changed:               make(chan struct{}),
		now:                   time.Now,
	}
	if path == "" {
		return store, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, fmt.Errorf("new operation store: read %s: %w", path, err)
	}
	var snapshot newOperationSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("new operation store: decode %s: %w", path, err)
	}
	if snapshot.Version != newOperationSnapshotVersion {
		return nil, fmt.Errorf("new operation store: unsupported version %d", snapshot.Version)
	}
	if snapshot.Operations != nil {
		store.operations = snapshot.Operations
	}
	return store, nil
}

func (s *NewOperationStore) CreateOrGet(seed NewOperation) (NewOperation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.operations[seed.ID]; ok {
		return existing, false, nil
	}
	if seed.ID == "" {
		return NewOperation{}, false, fmt.Errorf("new operation store: operation ID is required")
	}
	now := s.now()
	if seed.Step == "" {
		seed.Step = NewOperationAccepted
	}
	if seed.Status == "" {
		seed.Status = NewOperationRunning
	}
	if seed.CreatedAt.IsZero() {
		seed.CreatedAt = now
	}
	seed.UpdatedAt = now
	s.operations[seed.ID] = seed
	if err := s.saveLocked(); err != nil {
		delete(s.operations, seed.ID)
		return NewOperation{}, false, err
	}
	s.notifyChangedLocked()
	logNewOperationTransition(NewOperation{}, seed)
	return seed, true, nil
}

func (s *NewOperationStore) Get(id string) (NewOperation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	op, ok := s.operations[id]
	return op, ok
}

func (s *NewOperationStore) Update(id string, mutate func(*NewOperation) error) (NewOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.operations[id]
	if !ok {
		return NewOperation{}, fmt.Errorf("new operation store: operation %q not found", id)
	}
	next := current
	if mutate == nil {
		return NewOperation{}, fmt.Errorf("new operation store: update function is required")
	}
	if err := mutate(&next); err != nil {
		return NewOperation{}, err
	}
	if next.ID != current.ID {
		return NewOperation{}, fmt.Errorf("new operation store: operation ID cannot change")
	}
	currentRank, currentOK := newOperationStepRank(current.Step)
	nextRank, nextOK := newOperationStepRank(next.Step)
	if !currentOK || !nextOK {
		return NewOperation{}, fmt.Errorf("new operation store: invalid step transition %q -> %q", current.Step, next.Step)
	}
	if nextRank < currentRank {
		return NewOperation{}, fmt.Errorf("new operation store: step cannot regress from %q to %q", current.Step, next.Step)
	}
	next.UpdatedAt = s.now()
	s.operations[id] = next
	if err := s.saveLocked(); err != nil {
		s.operations[id] = current
		return NewOperation{}, err
	}
	if next.isExternalConversation() && next.TargetSessionKey != "" {
		if provisional, ok := s.provisionalTargetKeys[next.TargetSessionKey]; ok && provisional.operationID == next.ID {
			delete(s.provisionalTargetKeys, next.TargetSessionKey)
		}
	}
	if !next.isExternalConversation() {
		for sessionKey, provisional := range s.provisionalTargetKeys {
			if provisional.operationID == next.ID {
				delete(s.provisionalTargetKeys, sessionKey)
			}
		}
	}
	s.notifyChangedLocked()
	logNewOperationTransition(current, next)
	return next, nil
}

func logNewOperationTransition(previous, next NewOperation) {
	if previous.ID != "" && previous.Step == next.Step && previous.Status == next.Status && previous.StepAttempts == next.StepAttempts {
		return
	}
	slog.Info("new operation state changed",
		"project", next.Project,
		"platform", next.Platform,
		"operation_id", next.ID,
		"from_step", previous.Step,
		"to_step", next.Step,
		"from_status", previous.Status,
		"to_status", next.Status,
		"target_session_key", next.TargetSessionKey,
		"local_session_id", next.LocalSessionID,
		"attempt", next.StepAttempts,
		"next_attempt_at", next.NextAttemptAt,
	)
}

func (s *NewOperationStore) Ready(platform string, now time.Time) []NewOperation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ready := make([]NewOperation, 0)
	for _, op := range s.operations {
		if !strings.EqualFold(op.Platform, platform) {
			continue
		}
		switch op.Status {
		case NewOperationRunning:
			ready = append(ready, op)
		case NewOperationRetryWait:
			if op.NextAttemptAt.IsZero() || !op.NextAttemptAt.After(now) {
				ready = append(ready, op)
			}
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	return ready
}

func (s *NewOperationStore) Pending(platform string) []NewOperation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pending := make([]NewOperation, 0)
	for _, op := range s.operations {
		if strings.EqualFold(op.Platform, platform) && op.Status != NewOperationStatusCompleted {
			pending = append(pending, op)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	return pending
}

func (s *NewOperationStore) HasIncompleteTarget(sessionKey string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.provisionalTargetKeys[sessionKey]; ok {
		return true
	}
	for _, op := range s.operations {
		if op.isExternalConversation() && op.TargetSessionKey == sessionKey && op.Status != NewOperationStatusCompleted && op.Status != NewOperationManualAction {
			return true
		}
	}
	return false
}

func (s *NewOperationStore) RecoveryKeyForTarget(sessionKey string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if provisional, ok := s.provisionalTargetKeys[sessionKey]; ok {
		return provisional.recoveryKey
	}
	for _, op := range s.operations {
		if op.isExternalConversation() && op.TargetSessionKey == sessionKey && op.Status != NewOperationStatusCompleted && op.AgentRecoveryKey != "" {
			return op.AgentRecoveryKey
		}
	}
	return ""
}

// PublishTargetRecoveryKey makes a newly-created conversation discoverable by
// inbound messages before the durable operation snapshot is updated. The
// mapping is deliberately memory-only: if persistence fails it remains active
// for this process, while a restarted process must replay the idempotent spawn
// before admitting messages for an unresolved operation.
func (s *NewOperationStore) PublishTargetRecoveryKey(operationID, sessionKey, recoveryKey string) error {
	operationID = strings.TrimSpace(operationID)
	sessionKey = strings.TrimSpace(sessionKey)
	recoveryKey = strings.TrimSpace(recoveryKey)
	if operationID == "" || sessionKey == "" || recoveryKey == "" {
		return fmt.Errorf("new operation store: provisional target recovery fields are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[operationID]
	if !ok {
		return fmt.Errorf("new operation store: operation %q not found", operationID)
	}
	if op.Status == NewOperationStatusCompleted || (op.AgentRecoveryKey != "" && op.AgentRecoveryKey != recoveryKey) {
		return fmt.Errorf("new operation store: provisional target recovery does not match operation %q", operationID)
	}
	if !op.isExternalConversation() {
		return fmt.Errorf("new operation store: in-place operation cannot publish target recovery")
	}
	for id, candidate := range s.operations {
		if id != operationID && candidate.isExternalConversation() && candidate.TargetSessionKey == sessionKey && candidate.AgentRecoveryKey != recoveryKey && candidate.Status != NewOperationStatusCompleted {
			return fmt.Errorf("new operation store: target %q has conflicting recovery ownership", sessionKey)
		}
	}
	if existing, ok := s.provisionalTargetKeys[sessionKey]; ok && existing != (newOperationTargetRecovery{operationID: operationID, recoveryKey: recoveryKey}) {
		return fmt.Errorf("new operation store: target %q has conflicting provisional recovery ownership", sessionKey)
	}
	s.provisionalTargetKeys[sessionKey] = newOperationTargetRecovery{operationID: operationID, recoveryKey: recoveryKey}
	s.notifyChangedLocked()
	return nil
}

// WaitUntilMessageRecoverySafe blocks an inbound message only while an
// accepted operation for the same platform still has an unknown target. Once
// the target is published provisionally or durably, the normal message path
// can resolve the stable recovery key. Source conversations remain available.
func (s *NewOperationStore) WaitUntilMessageRecoverySafe(ctx context.Context, platform, sessionKey string) bool {
	for {
		s.mu.RLock()
		unknownTarget := false
		sourceConversation := false
		if provisional, ok := s.provisionalTargetKeys[sessionKey]; ok && provisional.recoveryKey != "" {
			s.mu.RUnlock()
			return true
		}
		for _, op := range s.operations {
			if !op.isExternalConversation() || !strings.EqualFold(op.Platform, platform) || op.Status == NewOperationStatusCompleted || op.Status == NewOperationManualAction {
				continue
			}
			if op.TargetSessionKey == sessionKey && op.AgentRecoveryKey != "" {
				s.mu.RUnlock()
				return true
			}
			if op.Step == NewOperationAccepted && op.TargetSessionKey == "" {
				unknownTarget = true
				if op.SourceSessionKey == sessionKey {
					sourceConversation = true
				}
			}
		}
		changed := s.changed
		s.mu.RUnlock()
		if !unknownTarget || sourceConversation {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
}

func (s *NewOperationStore) notifyChangedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *NewOperationStore) PruneCompleted(before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := make(map[string]NewOperation)
	for id, op := range s.operations {
		if op.Status != NewOperationStatusCompleted || op.CompletedAt.IsZero() || !op.CompletedAt.Before(before) {
			continue
		}
		removed[id] = op
		delete(s.operations, id)
	}
	if len(removed) == 0 {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		for id, op := range removed {
			s.operations[id] = op
		}
		return err
	}
	s.notifyChangedLocked()
	return nil
}

func newOperationStepRank(step NewOperationStep) (int, bool) {
	switch step {
	case NewOperationAccepted:
		return 0, true
	case NewOperationConversationSpawned:
		return 1, true
	case NewOperationSessionBound:
		return 2, true
	case NewOperationAgentSessionStarted:
		return 3, true
	case NewOperationNameSynced:
		return 4, true
	case NewOperationActivated:
		return 5, true
	case NewOperationCompleted:
		return 6, true
	default:
		return 0, false
	}
}

func (s *NewOperationStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	snapshot := newOperationSnapshot{
		Version:    newOperationSnapshotVersion,
		Operations: s.operations,
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("new operation store: encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("new operation store: create directory: %w", err)
	}
	if err := AtomicWriteFile(s.path, data, 0o644); err != nil {
		return fmt.Errorf("new operation store: write %s: %w", s.path, err)
	}
	return nil
}
