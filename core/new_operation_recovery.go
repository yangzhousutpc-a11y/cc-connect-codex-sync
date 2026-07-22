package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const newOperationRetryDelay = 5 * time.Second
const newOperationMaxRetryDelay = 5 * time.Minute
const newOperationMaxAttempts = 8

type permanentOperationError struct {
	err error
}

func (e permanentOperationError) Error() string              { return e.err.Error() }
func (e permanentOperationError) Unwrap() error              { return e.err }
func (e permanentOperationError) permanentOperationFailure() {}

// NewPermanentOperationError marks a platform/configuration failure that
// cannot succeed until a person changes permissions, configuration or input.
func NewPermanentOperationError(err error) error {
	if err == nil {
		return nil
	}
	return permanentOperationError{err: err}
}

func IsPermanentOperationError(err error) bool {
	var permanent interface{ permanentOperationFailure() }
	return errors.As(err, &permanent)
}

func newOperationRetryDelayForAttempt(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = newOperationRetryDelay
	}
	delay := base
	for i := 1; i < attempt && delay < newOperationMaxRetryDelay; i++ {
		if delay > newOperationMaxRetryDelay/2 {
			return newOperationMaxRetryDelay
		}
		delay *= 2
	}
	if delay > newOperationMaxRetryDelay {
		return newOperationMaxRetryDelay
	}
	return delay
}

func newOperationErrorSummary(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "operation timed out"
	case errors.Is(err, context.Canceled):
		return "operation canceled"
	case errors.Is(err, ErrNotSupported):
		return "operation is not supported"
	default:
		return "operation failed"
	}
}

func (e *Engine) SetNewOperationStore(store *NewOperationStore) {
	e.newOperationMu.Lock()
	e.newOperations = store
	e.newOperationMu.Unlock()
	e.registerStableConversationSessionResolvers()
}

func (e *Engine) recoverNewOperationsForPlatform(p Platform) {
	e.newOperationMu.Lock()
	store := e.newOperations
	e.newOperationMu.Unlock()
	if store == nil || e.ctx.Err() != nil {
		return
	}
	if err := store.PruneCompleted(time.Now().Add(-7 * 24 * time.Hour)); err != nil {
		slog.Warn("new operation: prune completed records failed", "project", e.name, "error", err)
	}
	now := time.Now()
	for _, op := range store.Pending(p.Name()) {
		if e.ctx.Err() != nil {
			return
		}
		if op.Status == NewOperationManualAction {
			// In-place manual-action records can represent a prepared Codex
			// session whose source ceased to be active while cc-connect was
			// offline. Automatically reactivating that record on every restart
			// would override (or repeatedly challenge) the user's later choice.
			// The retained candidate remains available through /list + /switch.
			if op.EffectiveMode() == NewOperationModeInPlaceSession {
				continue
			}
			var err error
			op, err = store.Update(op.ID, func(next *NewOperation) error {
				resetNewOperationError(next)
				return nil
			})
			if err != nil {
				slog.Error("new operation: reactivate manual operation failed", "operation_id", op.ID, "error", err)
				continue
			}
		}
		if op.Status == NewOperationRetryWait && op.NextAttemptAt.After(now) {
			e.scheduleNewOperationRetry(op)
			continue
		}
		e.advanceNewOperationByMode(p, op.ID, nil, nil, nil)
	}
}

func (e *Engine) advanceNewOperationByMode(p Platform, opID string, sourceReplyCtx any, agent Agent, sessions *SessionManager) {
	if e.newOperations == nil {
		return
	}
	op, ok := e.newOperations.Get(opID)
	if !ok {
		return
	}
	if op.EffectiveMode() == NewOperationModeInPlaceSession {
		transition, owner := e.beginInPlaceTransition(op.SourceSessionKey)
		if !owner {
			delay := e.newOperationRetryDelay
			if delay <= 0 {
				delay = newOperationRetryDelay
			}
			op.NextAttemptAt = time.Now().Add(delay)
			e.scheduleNewOperationRetry(op)
			return
		}
		defer e.finishInPlaceTransition(op.SourceSessionKey, transition)
		e.advanceInPlaceNewOperation(p, opID, sourceReplyCtx, agent, sessions)
		return
	}
	e.advanceNewOperation(p, opID, sourceReplyCtx, agent, sessions)
}

func (e *Engine) beginNewOperation(id string) bool {
	e.newOperationMu.Lock()
	defer e.newOperationMu.Unlock()
	if e.newOperationRun[id] {
		return false
	}
	e.newOperationRun[id] = true
	return true
}

func (e *Engine) endNewOperation(id string) {
	e.newOperationMu.Lock()
	delete(e.newOperationRun, id)
	e.newOperationMu.Unlock()
}

func (e *Engine) advanceNewOperation(p Platform, opID string, sourceReplyCtx any, agent Agent, sessions *SessionManager) {
	operationCtx, ready := e.newOperationRequestContext(p)
	if e.newOperations == nil || !ready || !e.beginNewOperation(opID) {
		return
	}
	defer e.endNewOperation(opID)

	var targetReplyCtx any
	for {
		if operationCtx.Err() != nil || !e.newOperationPlatformCanAdvance(p) {
			return
		}
		op, ok := e.newOperations.Get(opID)
		if !ok || op.Status == NewOperationStatusCompleted || op.Status == NewOperationManualAction {
			return
		}
		if agent == nil || sessions == nil {
			var err error
			agent, sessions, err = e.newOperationAgentContext(op)
			if err != nil {
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
		}

		switch op.Step {
		case NewOperationAccepted:
			spawner, ok := p.(ConversationSpawner)
			if !ok {
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(ErrNotSupported))
				return
			}
			spawned, err := spawner.SpawnConversation(operationCtx, ConversationSpawnRequest{
				Name:      op.Name,
				UserID:    op.UserID,
				MessageID: op.SourceMessageID,
			})
			if err != nil {
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if strings.TrimSpace(spawned.SessionKey) == "" || spawned.ReplyCtx == nil || spawned.SessionKey == op.SourceSessionKey {
				err := fmt.Errorf("platform returned an invalid conversation target")
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(err))
				return
			}
			recoveryKey := op.AgentRecoveryKey
			if recoveryKey == "" {
				recoveryKey = "cc-connect/new/" + op.ID
			}
			if err := e.newOperations.PublishTargetRecoveryKey(op.ID, spawned.SessionKey, recoveryKey); err != nil {
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(err))
				return
			}
			targetReplyCtx = spawned.ReplyCtx
			if e.multiWorkspace && op.WorkspaceDir != "" && e.workspaceBindings != nil {
				channelKey := extractWorkspaceChannelKey(spawned.SessionKey)
				if channelKey != "" {
					e.workspaceBindings.Bind("project:"+e.name, channelKey, op.Name, normalizeWorkspacePath(op.WorkspaceDir))
				}
			}
			if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
				next.TargetSessionKey = spawned.SessionKey
				next.AgentRecoveryKey = recoveryKey
				next.Step = NewOperationConversationSpawned
				resetNewOperationError(next)
				return nil
			}); err != nil {
				e.scheduleNewOperationPersistenceRetry(op, err)
				return
			}

		case NewOperationConversationSpawned:
			newSession := sessions.GetOrCreateActive(op.TargetSessionKey)
			newSession.SetName(op.Name)
			if err := sessions.SaveErr(); err != nil {
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
				next.LocalSessionID = newSession.ID
				next.Step = NewOperationSessionBound
				resetNewOperationError(next)
				return nil
			}); err != nil {
				e.scheduleNewOperationPersistenceRetry(op, err)
				return
			}

		case NewOperationSessionBound:
			newSession := sessions.FindByID(op.LocalSessionID)
			if newSession == nil {
				newSession = sessions.GetOrCreateActive(op.TargetSessionKey)
			}
			if !newSession.TryLock() {
				e.deferNewOperationForBusyTarget(op)
				return
			}
			if targetReplyCtx == nil {
				var err error
				targetReplyCtx, err = reconstructNewOperationReplyContext(p, op.TargetSessionKey)
				if err != nil {
					newSession.UnlockWithoutUpdate()
					e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
					return
				}
			}
			interactiveKey := op.TargetSessionKey
			if op.WorkspaceDir != "" {
				interactiveKey = op.WorkspaceDir + ":" + op.TargetSessionKey
			}
			state := e.getOrCreateInteractiveStateWithRecoveryContext(
				operationCtx,
				interactiveKey,
				p,
				targetReplyCtx,
				newSession,
				sessions,
				agent,
				op.TargetSessionKey,
				op.AgentRecoveryKey,
				false,
			)
			if op.WorkspaceDir != "" {
				state.mu.Lock()
				state.workspaceDir = op.WorkspaceDir
				state.mu.Unlock()
			}
			agentSessionID := newSession.GetAgentSessionID()
			_, agentSession, startupErr := e.newOperationInteractiveStateSnapshot(op)
			if agentSession == nil || agentSessionID == "" {
				e.releaseSpawnedSessionAfterInitialization(state, newSession, sessions, interactiveKey, agent, op.WorkspaceDir)
				initializationErr := startupErr
				if initializationErr == nil {
					initializationErr = fmt.Errorf("agent session initialization failed")
				}
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, initializationErr)
				return
			}
			if err := primeAgentSessionVisibility(agentSession, op.Name); err != nil {
				e.releaseSpawnedSessionAfterInitialization(state, newSession, sessions, interactiveKey, agent, op.WorkspaceDir)
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if err := sessions.SaveErr(); err != nil {
				e.releaseSpawnedSessionAfterInitialization(state, newSession, sessions, interactiveKey, agent, op.WorkspaceDir)
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
				next.LocalSessionID = newSession.ID
				next.AgentSessionID = agentSessionID
				next.Step = NewOperationAgentSessionStarted
				resetNewOperationError(next)
				return nil
			}); err != nil {
				e.releaseSpawnedSessionAfterInitialization(state, newSession, sessions, interactiveKey, agent, op.WorkspaceDir)
				e.scheduleNewOperationPersistenceRetry(op, err)
				return
			}
			e.releaseSpawnedSessionAfterInitialization(state, newSession, sessions, interactiveKey, agent, op.WorkspaceDir)

		case NewOperationAgentSessionStarted:
			state, agentSession, startupErr := e.newOperationInteractiveStateSnapshot(op)
			if state == nil || agentSession == nil {
				if targetReplyCtx == nil {
					var err error
					targetReplyCtx, err = reconstructNewOperationReplyContext(p, op.TargetSessionKey)
					if err != nil {
						e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
						return
					}
				}
				session := sessions.FindByID(op.LocalSessionID)
				if session == nil {
					e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, fmt.Errorf("target session %q not found", op.LocalSessionID))
					return
				}
				e.getOrCreateInteractiveStateWithRecoveryContext(
					operationCtx, e.newOperationInteractiveKey(op), p, targetReplyCtx, session, sessions, agent,
					op.TargetSessionKey, op.AgentRecoveryKey, false,
				)
				state, agentSession, startupErr = e.newOperationInteractiveStateSnapshot(op)
			}
			if state == nil || agentSession == nil || !agentSession.Alive() {
				resumeErr := fmt.Errorf("agent session resume failed")
				if startupErr != nil {
					resumeErr = startupErr
				}
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, resumeErr)
				return
			}
			if currentID := agentSession.CurrentSessionID(); currentID == "" || (op.AgentSessionID != "" && currentID != op.AgentSessionID) {
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, fmt.Errorf("agent session resume returned an unexpected task"))
				return
			}
			// agentSession is a local snapshot. The network-backed name RPC must
			// not run while interactiveMu or state.mu is held.
			if err := syncAgentSessionName(agentSession, op.Name); err != nil {
				e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
				next.Step = NewOperationNameSynced
				resetNewOperationError(next)
				return nil
			}); err != nil {
				e.scheduleNewOperationPersistenceRetry(op, err)
				return
			}

		case NewOperationNameSynced:
			if targetReplyCtx == nil {
				var err error
				targetReplyCtx, err = reconstructNewOperationReplyContext(p, op.TargetSessionKey)
				if err != nil {
					e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
					return
				}
			}
			if !op.TargetNotified {
				message := e.decorateSessionReply(p, targetReplyCtx, e.i18n.Tf(MsgNewConversationWelcome, op.Name))
				if err := p.Send(operationCtx, targetReplyCtx, message); err != nil {
					e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
					return
				}
				if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
					next.TargetNotified = true
					return nil
				}); err != nil {
					e.scheduleNewOperationPersistenceRetry(op, err)
					return
				}
				continue
			}
			if !op.SourceNotified {
				if sourceReplyCtx == nil {
					var err error
					sourceReplyCtx, err = reconstructNewOperationReplyContext(p, op.SourceSessionKey)
					if err != nil {
						e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
						return
					}
				}
				message := e.decorateSessionReply(p, sourceReplyCtx, e.i18n.Tf(MsgNewConversationCreated, op.Name))
				if err := p.Reply(operationCtx, sourceReplyCtx, message); err != nil {
					e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
					return
				}
				if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
					next.SourceNotified = true
					return nil
				}); err != nil {
					e.scheduleNewOperationPersistenceRetry(op, err)
					return
				}
				continue
			}
			now := time.Now()
			if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
				resetNewOperationError(next)
				next.Step = NewOperationCompleted
				next.Status = NewOperationStatusCompleted
				next.CompletedAt = now
				return nil
			}); err != nil {
				e.scheduleNewOperationPersistenceRetry(op, err)
				return
			}
			e.cancelNewOperationRetry(op.ID)
			return

		default:
			e.failNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(fmt.Errorf("unknown operation step %q", op.Step)))
			return
		}
	}
}

func (e *Engine) newOperationAgentContext(op NewOperation) (Agent, *SessionManager, error) {
	if op.WorkspaceDir == "" {
		return e.agent, e.sessions, nil
	}
	return e.getOrCreateWorkspaceAgent(op.WorkspaceDir)
}

func (e *Engine) newOperationInteractiveKey(op NewOperation) string {
	if op.WorkspaceDir == "" {
		return op.TargetSessionKey
	}
	return op.WorkspaceDir + ":" + op.TargetSessionKey
}

// newOperationInteractiveStateSnapshot returns the route state and a stable
// local AgentSession reference. Agent RPCs must run after this helper returns.
func (e *Engine) newOperationInteractiveStateSnapshot(op NewOperation) (*interactiveState, AgentSession, error) {
	e.interactiveMu.Lock()
	state := e.interactiveStates[e.newOperationInteractiveKey(op)]
	if state == nil {
		e.interactiveMu.Unlock()
		return nil, nil, nil
	}
	state.mu.Lock()
	agentSession := state.agentSession
	startupErr := state.startupErr
	state.mu.Unlock()
	e.interactiveMu.Unlock()
	return state, agentSession, startupErr
}

func (e *Engine) newOperationManagesTargetName(sessionKey string) bool {
	return e.newOperations != nil && e.newOperations.HasIncompleteTarget(sessionKey)
}

func reconstructNewOperationReplyContext(p Platform, sessionKey string) (any, error) {
	reconstructor, ok := p.(ReplyContextReconstructor)
	if !ok {
		return nil, fmt.Errorf("platform %q cannot reconstruct reply context", p.Name())
	}
	return reconstructor.ReconstructReplyCtx(sessionKey)
}

func resetNewOperationError(op *NewOperation) {
	op.Status = NewOperationRunning
	op.StepAttempts = 0
	op.NextAttemptAt = time.Time{}
	op.LastError = ""
}

func (e *Engine) failNewOperation(op NewOperation, err error) NewOperation {
	if e.newOperations == nil {
		return NewOperation{}
	}
	if IsPermanentOperationError(err) || op.StepAttempts+1 >= newOperationMaxAttempts {
		return e.failNewOperationManual(op, err)
	}
	baseDelay := e.newOperationRetryDelay
	delay := newOperationRetryDelayForAttempt(baseDelay, op.StepAttempts+1)
	updated, updateErr := e.newOperations.Update(op.ID, func(next *NewOperation) error {
		next.Status = NewOperationRetryWait
		next.StepAttempts++
		next.NextAttemptAt = time.Now().Add(delay)
		next.LastError = newOperationErrorSummary(err)
		return nil
	})
	if updateErr != nil {
		e.scheduleNewOperationPersistenceRetry(op, updateErr)
		return NewOperation{}
	}
	e.scheduleNewOperationRetry(updated)
	return updated
}

func (e *Engine) deferNewOperationForBusyTarget(op NewOperation) {
	if e.newOperations == nil {
		return
	}
	delay := e.newOperationRetryDelay
	if delay <= 0 {
		delay = newOperationRetryDelay
	}
	updated, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
		next.Status = NewOperationRetryWait
		next.NextAttemptAt = time.Now().Add(delay)
		next.LastError = "target session busy"
		return nil
	})
	if err != nil {
		e.scheduleNewOperationPersistenceRetry(op, err)
		return
	}
	e.scheduleNewOperationRetry(updated)
}

func (e *Engine) failNewOperationManual(op NewOperation, err error) NewOperation {
	if e.newOperations == nil {
		return NewOperation{}
	}
	updated, updateErr := e.newOperations.Update(op.ID, func(next *NewOperation) error {
		next.Status = NewOperationManualAction
		next.StepAttempts++
		next.NextAttemptAt = time.Time{}
		next.LastError = newOperationErrorSummary(err)
		return nil
	})
	if updateErr != nil {
		e.scheduleNewOperationPersistenceRetry(op, updateErr)
		return NewOperation{}
	}
	return updated
}

func (e *Engine) failNewOperationWithNotice(ctx context.Context, p Platform, op NewOperation, sourceReplyCtx any, err error) {
	updated := e.failNewOperation(op, err)
	if updated.ID == "" {
		return
	}
	var message string
	switch updated.Status {
	case NewOperationRetryWait:
		if op.StepAttempts != 0 {
			return
		}
		message = e.i18n.Tf(MsgNewConversationRetryScheduled, op.Name)
	case NewOperationManualAction:
		message = e.i18n.Tf(MsgNewConversationManualAction, op.Name)
	default:
		return
	}
	if sourceReplyCtx == nil {
		var reconstructErr error
		sourceReplyCtx, reconstructErr = reconstructNewOperationReplyContext(p, op.SourceSessionKey)
		if reconstructErr != nil {
			return
		}
	}
	message = e.decorateSessionReply(p, sourceReplyCtx, message)
	_ = p.Reply(ctx, sourceReplyCtx, message)
}

func (e *Engine) scheduleNewOperationPersistenceRetry(op NewOperation, err error) {
	delay := e.newOperationRetryDelay
	if delay <= 0 {
		delay = newOperationRetryDelay
	}
	retry := op
	retry.NextAttemptAt = time.Now().Add(delay)
	slog.Error("new operation: persist state failed; retry scheduled in memory",
		"operation_id", op.ID,
		"step", op.Step,
		"retry_at", retry.NextAttemptAt,
		"error", err,
	)
	e.scheduleNewOperationRetry(retry)
}

func (e *Engine) scheduleNewOperationRetry(op NewOperation) {
	delay := time.Until(op.NextAttemptAt)
	if delay < 0 {
		delay = 0
	}
	e.newOperationMu.Lock()
	if existing := e.newOperationTimer[op.ID]; existing != nil {
		existing.Stop()
	}
	e.newOperationTimer[op.ID] = time.AfterFunc(delay, func() {
		e.newOperationMu.Lock()
		delete(e.newOperationTimer, op.ID)
		e.newOperationMu.Unlock()
		if e.ctx.Err() != nil {
			return
		}
		p := e.newOperationPlatform(op.Platform)
		if p == nil || !e.newOperationPlatformReady(p) {
			return
		}
		e.advanceNewOperationByMode(p, op.ID, nil, nil, nil)
	})
	e.newOperationMu.Unlock()
}

func (e *Engine) cancelNewOperationRetry(id string) {
	e.newOperationMu.Lock()
	if timer := e.newOperationTimer[id]; timer != nil {
		timer.Stop()
		delete(e.newOperationTimer, id)
	}
	e.newOperationMu.Unlock()
}

func (e *Engine) newOperationPlatform(name string) Platform {
	for _, p := range e.platforms {
		if strings.EqualFold(p.Name(), name) {
			return p
		}
	}
	return nil
}

func (e *Engine) newOperationPlatformReady(p Platform) bool {
	e.platformLifecycleMu.Lock()
	defer e.platformLifecycleMu.Unlock()
	return !e.stopping && e.platformReady[p]
}

func (e *Engine) newOperationPlatformCanAdvance(p Platform) bool {
	e.platformLifecycleMu.Lock()
	defer e.platformLifecycleMu.Unlock()
	ready, tracked := e.platformReady[p]
	return !e.stopping && e.ctx.Err() == nil && (!tracked || ready)
}

func (e *Engine) newOperationRequestContext(p Platform) (context.Context, bool) {
	e.platformLifecycleMu.Lock()
	defer e.platformLifecycleMu.Unlock()
	if e.stopping || e.ctx.Err() != nil {
		return nil, false
	}
	ready, tracked := e.platformReady[p]
	if tracked && !ready {
		return nil, false
	}
	if ctx := e.platformOperationCtx[p]; ctx != nil {
		return ctx, true
	}
	if tracked {
		return nil, false
	}
	return e.ctx, true
}
