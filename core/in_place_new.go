package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type inPlaceTransition struct {
	mu                      sync.Mutex
	pending                 []inPlacePendingMessage
	activeMessageID         string
	permissionControlActive bool
	stopControlActive       bool
}

type inPlacePendingMessage struct {
	platform Platform
	message  *Message
}

func cloneInPlaceMessage(msg *Message) *Message {
	if msg == nil {
		return nil
	}
	clone := *msg
	clone.inPlaceTransition = nil
	clone.inPlaceCompletion = nil
	clone.inPlaceControlTransition = nil
	clone.inPlaceControlKind = inPlaceControlNone
	clone.DispatchAdmission = nil
	return &clone
}

type inPlaceMessageCompletion struct {
	done chan struct{}
	once sync.Once
}

func newInPlaceMessageCompletion() *inPlaceMessageCompletion {
	return &inPlaceMessageCompletion{done: make(chan struct{})}
}

func (c *inPlaceMessageCompletion) complete() {
	if c != nil {
		c.once.Do(func() { close(c.done) })
	}
}

func (e *Engine) isInPlaceTransitionDispatch(p Platform, msg *Message) bool {
	if p == nil || msg == nil || len(msg.Images) != 0 {
		return false
	}
	parts := splitCommandArgs(e.resolveAlias(strings.TrimSpace(msg.Content)))
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	command := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	commandID := matchPrefix(command, builtinCommands)
	if commandID != "new" && commandID != "back" && commandID != "switch" {
		return false
	}
	if commandID == "new" {
		if _, external := p.(ConversationSpawner); external {
			return false
		}
	}
	forker, ok := p.(InPlaceConversationForker)
	return ok && forker.SupportsInPlaceConversationFork()
}

func isStopDispatch(msg *Message) bool {
	if msg == nil || len(msg.Images) != 0 {
		return false
	}
	parts := splitCommandArgs(strings.TrimSpace(msg.Content))
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	command := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	return matchPrefix(command, builtinCommands) == "stop"
}

// inPlaceActiveTurnControl classifies messages that may unblock the exact live
// turn currently being drained.
type inPlaceActiveTurnControl uint8

const (
	inPlaceControlNone inPlaceActiveTurnControl = iota
	inPlaceControlPermission
	inPlaceControlStop
)

func (e *Engine) inPlaceActiveTurnControl(msg *Message, activeMessageID string) inPlaceActiveTurnControl {
	if msg == nil || activeMessageID == "" {
		return inPlaceControlNone
	}
	interactiveKey := e.interactiveKeyForSessionKey(msg.SessionKey)
	e.interactiveMu.Lock()
	state := e.interactiveStates[interactiveKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return inPlaceControlNone
	}
	state.mu.Lock()
	currentMessageID := state.currentMessageID
	pendingPermission := state.pending != nil
	agentSession := state.agentSession
	state.mu.Unlock()
	if currentMessageID != activeMessageID || agentSession == nil || !agentSession.Alive() {
		return inPlaceControlNone
	}
	if isStopDispatch(msg) {
		return inPlaceControlStop
	}
	if pendingPermission {
		return inPlaceControlPermission
	}
	return inPlaceControlNone
}

func (e *Engine) finishInPlaceControl(msg *Message) {
	if msg == nil || msg.inPlaceControlTransition == nil {
		return
	}
	transition := msg.inPlaceControlTransition
	transition.mu.Lock()
	switch msg.inPlaceControlKind {
	case inPlaceControlPermission:
		transition.permissionControlActive = false
	case inPlaceControlStop:
		transition.stopControlActive = false
	}
	transition.mu.Unlock()
	msg.inPlaceControlTransition = nil
	msg.inPlaceControlKind = inPlaceControlNone
	msg.acknowledgeDispatchAdmission()
}

// prepareInPlaceDispatch atomically decides whether an incoming message owns a
// new in-place transition, joins the current transition queue, or can proceed
// normally. Capability/command classification happens while holding the same
// mutex used by ordinary arrivals, closing the gap between an active-changing
// navigation command's admission and gate registration.
func (e *Engine) prepareInPlaceDispatch(p Platform, msg *Message, bypass *inPlaceTransition) (*inPlaceTransition, bool) {
	if msg == nil || msg.SessionKey == "" {
		return nil, false
	}
	e.inPlaceTransitionMu.Lock()
	defer e.inPlaceTransitionMu.Unlock()
	if e.inPlaceTransitions == nil {
		e.inPlaceTransitions = make(map[string]*inPlaceTransition)
	}
	if current := e.inPlaceTransitions[msg.SessionKey]; current != nil {
		if current == bypass {
			return nil, false
		}
		current.mu.Lock()
		activeMessageID := current.activeMessageID
		current.mu.Unlock()
		activeControl := e.inPlaceActiveTurnControl(msg, activeMessageID)
		current.mu.Lock()
		if current.activeMessageID == activeMessageID {
			switch activeControl {
			case inPlaceControlStop:
				if !current.stopControlActive {
					current.stopControlActive = true
					msg.inPlaceControlTransition = current
					msg.inPlaceControlKind = inPlaceControlStop
					current.mu.Unlock()
					return nil, false
				}
			case inPlaceControlPermission:
				if !current.permissionControlActive && !current.stopControlActive {
					current.permissionControlActive = true
					msg.inPlaceControlTransition = current
					msg.inPlaceControlKind = inPlaceControlPermission
					current.mu.Unlock()
					return nil, false
				}
			}
		}
		current.pending = append(current.pending, inPlacePendingMessage{platform: p, message: cloneInPlaceMessage(msg)})
		current.mu.Unlock()
		return nil, true
	}
	if bypass != nil || !e.isInPlaceTransitionDispatch(p, msg) {
		return nil, false
	}
	transition := &inPlaceTransition{}
	e.inPlaceTransitions[msg.SessionKey] = transition
	msg.inPlaceTransition = transition
	return transition, false
}

func (e *Engine) beginInPlaceTransition(sessionKey string) (*inPlaceTransition, bool) {
	e.inPlaceTransitionMu.Lock()
	defer e.inPlaceTransitionMu.Unlock()
	if e.inPlaceTransitions == nil {
		e.inPlaceTransitions = make(map[string]*inPlaceTransition)
	}
	if existing := e.inPlaceTransitions[sessionKey]; existing != nil {
		return existing, false
	}
	transition := &inPlaceTransition{}
	e.inPlaceTransitions[sessionKey] = transition
	return transition, true
}

func (e *Engine) queueDuringInPlaceTransition(p Platform, msg *Message) bool {
	if msg == nil || msg.SessionKey == "" {
		return false
	}
	e.inPlaceTransitionMu.Lock()
	transition := e.inPlaceTransitions[msg.SessionKey]
	if transition == nil {
		e.inPlaceTransitionMu.Unlock()
		return false
	}
	transition.mu.Lock()
	transition.pending = append(transition.pending, inPlacePendingMessage{platform: p, message: cloneInPlaceMessage(msg)})
	transition.mu.Unlock()
	e.inPlaceTransitionMu.Unlock()
	return true
}

func (e *Engine) beginOrQueueInPlaceTransition(p Platform, msg *Message) (*inPlaceTransition, bool) {
	for {
		if transition, owner := e.beginInPlaceTransition(msg.SessionKey); owner {
			return transition, true
		}
		if e.queueDuringInPlaceTransition(p, msg) {
			return nil, false
		}
	}
}

func (e *Engine) finishInPlaceTransition(sessionKey string, transition *inPlaceTransition) {
	if transition == nil {
		return
	}
	for {
		e.inPlaceTransitionMu.Lock()
		if e.inPlaceTransitions[sessionKey] != transition {
			e.inPlaceTransitionMu.Unlock()
			return
		}
		transition.mu.Lock()
		if len(transition.pending) == 0 {
			delete(e.inPlaceTransitions, sessionKey)
			transition.mu.Unlock()
			e.inPlaceTransitionMu.Unlock()
			return
		}
		queued := transition.pending[0]
		transition.pending = transition.pending[1:]
		transition.activeMessageID = queued.message.MessageID
		transition.mu.Unlock()
		e.inPlaceTransitionMu.Unlock()

		completion := newInPlaceMessageCompletion()
		queued.message.inPlaceTransition = transition
		queued.message.inPlaceCompletion = completion
		e.handleMessageWithInPlaceTransition(queued.platform, queued.message, transition)
		select {
		case <-completion.done:
			transition.mu.Lock()
			if transition.activeMessageID == queued.message.MessageID {
				transition.activeMessageID = ""
			}
			transition.mu.Unlock()
			queued.message.inPlaceCompletion = nil
			queued.message.inPlaceTransition = nil
		case <-e.ctx.Done():
			e.inPlaceTransitionMu.Lock()
			if e.inPlaceTransitions[sessionKey] == transition {
				delete(e.inPlaceTransitions, sessionKey)
			}
			e.inPlaceTransitionMu.Unlock()
			return
		}
	}
}

func (e *Engine) startInPlaceNewOperation(p Platform, msg *Message, agent Agent, sessions *SessionManager, workspaceDir, name string) {
	if e.newOperations == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgNewInPlaceFailed, fmt.Errorf("recoverable operation store is unavailable")))
		return
	}
	if strings.TrimSpace(msg.MessageID) == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgNewInPlaceFailed, fmt.Errorf("source message ID is required for recoverable /new")))
		return
	}

	source := sessions.GetOrCreateActive(msg.SessionKey)
	opID := NewOperationID(e.name, msg.SessionKey, msg.MessageID)
	op, _, err := e.newOperations.CreateOrGet(NewOperation{
		ID:                   opID,
		Project:              e.name,
		Platform:             p.Name(),
		Mode:                 NewOperationModeInPlaceSession,
		SourceSessionKey:     msg.SessionKey,
		SourceLocalSessionID: source.ID,
		SourceMessageID:      msg.MessageID,
		UserID:               msg.UserID,
		Name:                 name,
		WorkspaceDir:         workspaceDir,
		Step:                 NewOperationAccepted,
		Status:               NewOperationRunning,
		TargetSessionKey:     msg.SessionKey,
		AgentRecoveryKey:     "cc-connect/new/" + opID,
	})
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgNewInPlaceFailed, err))
		return
	}
	if op.EffectiveMode() != NewOperationModeInPlaceSession {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgNewInPlaceFailed, fmt.Errorf("operation mode does not match platform capability")))
		return
	}
	if op.Status == NewOperationManualAction {
		op, err = e.newOperations.Update(op.ID, func(next *NewOperation) error {
			resetNewOperationError(next)
			return nil
		})
		if err != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgNewInPlaceFailed, err))
			return
		}
	}
	e.advanceInPlaceNewOperation(p, op.ID, msg.ReplyCtx, agent, sessions)
}

func inPlaceCandidateInteractiveKey(op NewOperation) string {
	base := op.SourceSessionKey + ":new:" + op.ID
	if op.WorkspaceDir == "" {
		return base
	}
	return op.WorkspaceDir + ":" + base
}

func inPlaceActiveInteractiveKey(op NewOperation) string {
	if op.WorkspaceDir == "" {
		return op.SourceSessionKey
	}
	return op.WorkspaceDir + ":" + op.SourceSessionKey
}

func (e *Engine) advanceInPlaceNewOperation(p Platform, opID string, sourceReplyCtx any, agent Agent, sessions *SessionManager) {
	operationCtx, ready := e.newOperationRequestContext(p)
	if e.newOperations == nil || !ready || !e.beginNewOperation(opID) {
		return
	}
	defer e.endNewOperation(opID)

	for {
		if operationCtx.Err() != nil || !e.newOperationPlatformCanAdvance(p) {
			return
		}
		op, ok := e.newOperations.Get(opID)
		if !ok || op.Status == NewOperationStatusCompleted || op.Status == NewOperationManualAction {
			return
		}
		if op.EffectiveMode() != NewOperationModeInPlaceSession {
			e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(fmt.Errorf("unexpected operation mode %q", op.EffectiveMode())))
			return
		}
		if agent == nil || sessions == nil {
			var err error
			agent, sessions, err = e.newOperationAgentContext(op)
			if err != nil {
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
		}

		switch op.Step {
		case NewOperationAccepted:
			candidate, err := sessions.EnsurePreparedSideSession(op.SourceSessionKey, op.Name, op.ID)
			if err != nil {
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
				next.LocalSessionID = candidate.ID
				next.TargetSessionKey = next.SourceSessionKey
				next.Step = NewOperationSessionBound
				resetNewOperationError(next)
				return nil
			}); err != nil {
				e.scheduleNewOperationPersistenceRetry(op, err)
				return
			}

		case NewOperationSessionBound:
			candidate := sessions.FindByID(op.LocalSessionID)
			if candidate == nil {
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(fmt.Errorf("candidate session %q not found", op.LocalSessionID)))
				return
			}
			if !candidate.TryLock() {
				e.deferNewOperationForBusyTarget(op)
				return
			}
			if sourceReplyCtx == nil {
				var err error
				sourceReplyCtx, err = reconstructNewOperationReplyContext(p, op.SourceSessionKey)
				if err != nil {
					candidate.UnlockWithoutUpdate()
					e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
					return
				}
			}
			tempKey := inPlaceCandidateInteractiveKey(op)
			state := e.getOrCreateInteractiveStateWithRecoveryContext(
				operationCtx,
				tempKey,
				p,
				sourceReplyCtx,
				candidate,
				sessions,
				agent,
				op.SourceSessionKey,
				op.AgentRecoveryKey,
				false,
			)
			if op.WorkspaceDir != "" {
				state.mu.Lock()
				state.workspaceDir = op.WorkspaceDir
				state.mu.Unlock()
			}
			agentSession, startupErr := interactiveStateSessionSnapshot(state)
			agentSessionID := candidate.GetAgentSessionID()
			if agentSession == nil || agentSessionID == "" {
				candidate.UnlockWithoutUpdate()
				e.cleanupInteractiveState(tempKey, state)
				if startupErr == nil {
					startupErr = fmt.Errorf("agent session initialization failed")
				}
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, startupErr)
				return
			}
			if err := primeAgentSessionVisibility(agentSession, op.Name); err != nil {
				candidate.UnlockWithoutUpdate()
				e.cleanupInteractiveState(tempKey, state)
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if err := sessions.SaveErr(); err != nil {
				candidate.UnlockWithoutUpdate()
				e.cleanupInteractiveState(tempKey, state)
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
				next.AgentSessionID = agentSessionID
				next.Step = NewOperationAgentSessionStarted
				resetNewOperationError(next)
				return nil
			}); err != nil {
				candidate.UnlockWithoutUpdate()
				e.cleanupInteractiveState(tempKey, state)
				e.scheduleNewOperationPersistenceRetry(op, err)
				return
			}
			candidate.Unlock()

		case NewOperationAgentSessionStarted:
			candidate := sessions.FindByID(op.LocalSessionID)
			if candidate == nil {
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(fmt.Errorf("candidate session %q not found", op.LocalSessionID)))
				return
			}
			if sourceReplyCtx == nil {
				var err error
				sourceReplyCtx, err = reconstructNewOperationReplyContext(p, op.SourceSessionKey)
				if err != nil {
					e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
					return
				}
			}
			tempKey := inPlaceCandidateInteractiveKey(op)
			e.interactiveMu.Lock()
			state := e.interactiveStates[tempKey]
			e.interactiveMu.Unlock()
			if state == nil {
				state = e.getOrCreateInteractiveStateWithRecoveryContext(
					operationCtx, tempKey, p, sourceReplyCtx, candidate, sessions, agent,
					op.SourceSessionKey, op.AgentRecoveryKey, false,
				)
			}
			agentSession, startupErr := interactiveStateSessionSnapshot(state)
			if agentSession == nil || !agentSession.Alive() {
				e.cleanupInteractiveState(tempKey, state)
				if startupErr == nil {
					startupErr = fmt.Errorf("agent session resume failed")
				}
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, startupErr)
				return
			}
			if currentID := agentSession.CurrentSessionID(); currentID == "" || (op.AgentSessionID != "" && currentID != op.AgentSessionID) {
				e.cleanupInteractiveState(tempKey, state)
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, fmt.Errorf("agent session resume returned an unexpected task"))
				return
			}
			if err := syncAgentSessionName(agentSession, op.Name); err != nil {
				e.cleanupInteractiveState(tempKey, state)
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
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
			source := sessions.FindByID(op.SourceLocalSessionID)
			if source == nil {
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(fmt.Errorf("source session %q not found", op.SourceLocalSessionID)))
				return
			}
			activeID := sessions.ActiveSessionID(op.SourceSessionKey)
			if activeID == op.LocalSessionID {
				// ActivatePreparedSession persists the active pointer and back stack
				// before the operation step is updated. A crash in that small window
				// must only advance the record; activating again would push A twice.
				setExternalConversationRoutes(agent, sessions.AgentSessionRoutes())
				if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
					next.Step = NewOperationActivated
					resetNewOperationError(next)
					return nil
				}); err != nil {
					e.scheduleNewOperationPersistenceRetry(op, err)
					return
				}
				continue
			}
			if activeID != op.SourceLocalSessionID {
				err := NewPermanentOperationError(fmt.Errorf("%w: user selected another active session", ErrActiveSessionChanged))
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			// A persisted name_synced record may be recovered by a fresh Engine,
			// which has no in-memory temporary state. Resume the exact prepared
			// Codex task by stable recovery key before activation and promotion.
			var state *interactiveState
			var err error
			sourceReplyCtx, state, err = e.restoreNameSyncedInPlaceCandidate(operationCtx, p, op, sourceReplyCtx, agent, sessions)
			if err != nil {
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			// Commands bypass the normal session lock, so /new can reach this
			// point while the source task is still emitting its foreground turn.
			// Own the source lock across activation and live-state promotion. A
			// busy source is retried after its current turn has fully unlocked;
			// it must never be detached from underneath the old event loop.
			if !source.TryLock() {
				e.deferNewOperationForBusyTarget(op)
				return
			}
			if err := sessions.ActivatePreparedSession(op.SourceSessionKey, op.LocalSessionID, op.SourceLocalSessionID); err != nil {
				source.UnlockWithoutUpdate()
				if errors.Is(err, ErrActiveSessionChanged) {
					err = NewPermanentOperationError(fmt.Errorf("%w: user selected another active session", err))
				}
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if state == nil {
				source.UnlockWithoutUpdate()
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(fmt.Errorf("candidate interactive state is unavailable")))
				return
			}
			if err := e.promoteInPlaceInteractiveState(op); err != nil {
				source.UnlockWithoutUpdate()
				// Promotion is an in-memory invariant after durable activation. Treat
				// violations as manual so a retry cannot silently stop the wrong task.
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(err))
				return
			}
			source.UnlockWithoutUpdate()
			setExternalConversationRoutes(agent, sessions.AgentSessionRoutes())
			if _, err := e.newOperations.Update(op.ID, func(next *NewOperation) error {
				next.Step = NewOperationActivated
				resetNewOperationError(next)
				return nil
			}); err != nil {
				e.scheduleNewOperationPersistenceRetry(op, err)
				return
			}

		case NewOperationActivated:
			if activeID := sessions.ActiveSessionID(op.SourceSessionKey); activeID != op.LocalSessionID {
				err := NewPermanentOperationError(fmt.Errorf("%w: user selected another active session", ErrActiveSessionChanged))
				e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, err)
				return
			}
			if sourceReplyCtx == nil {
				var err error
				sourceReplyCtx, err = reconstructNewOperationReplyContext(p, op.SourceSessionKey)
				if err != nil {
					e.retryInPlaceActivatedNotification(op, err)
					return
				}
			}
			if !op.SourceNotified {
				message := e.decorateSessionReply(p, sourceReplyCtx, e.i18n.Tf(MsgNewInPlaceCreated, op.Name))
				if err := p.Reply(operationCtx, sourceReplyCtx, message); err != nil {
					e.retryInPlaceActivatedNotification(op, err)
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
			e.failInPlaceNewOperationWithNotice(operationCtx, p, op, sourceReplyCtx, NewPermanentOperationError(fmt.Errorf("unknown in-place operation step %q", op.Step)))
			return
		}
	}
}

func (e *Engine) restoreNameSyncedInPlaceCandidate(ctx context.Context, p Platform, op NewOperation, sourceReplyCtx any, agent Agent, sessions *SessionManager) (any, *interactiveState, error) {
	candidate := sessions.FindByID(op.LocalSessionID)
	if candidate == nil {
		return sourceReplyCtx, nil, NewPermanentOperationError(fmt.Errorf("candidate session %q not found", op.LocalSessionID))
	}
	if sourceReplyCtx == nil {
		var err error
		sourceReplyCtx, err = reconstructNewOperationReplyContext(p, op.SourceSessionKey)
		if err != nil {
			return sourceReplyCtx, nil, err
		}
	}
	tempKey := inPlaceCandidateInteractiveKey(op)
	e.interactiveMu.Lock()
	state := e.interactiveStates[tempKey]
	e.interactiveMu.Unlock()
	if state == nil {
		state = e.getOrCreateInteractiveStateWithRecoveryContext(
			ctx, tempKey, p, sourceReplyCtx, candidate, sessions, agent,
			op.SourceSessionKey, op.AgentRecoveryKey, false,
		)
	}
	agentSession, startupErr := interactiveStateSessionSnapshot(state)
	if agentSession == nil || !agentSession.Alive() {
		if startupErr == nil {
			startupErr = fmt.Errorf("agent session resume failed")
		}
		return sourceReplyCtx, state, startupErr
	}
	if currentID := agentSession.CurrentSessionID(); currentID == "" || (op.AgentSessionID != "" && currentID != op.AgentSessionID) {
		return sourceReplyCtx, state, fmt.Errorf("agent session resume returned an unexpected task")
	}
	return sourceReplyCtx, state, nil
}

func (e *Engine) retryInPlaceActivatedNotification(op NewOperation, err error) {
	updated := e.failNewOperation(op, err)
	if updated.ID == "" {
		return
	}
	slog.Warn("in-place new operation: activated notification will retry",
		"operation_id", op.ID,
		"status", updated.Status,
		"attempt", updated.StepAttempts,
		"error", err,
	)
}

func interactiveStateSessionSnapshot(state *interactiveState) (AgentSession, error) {
	if state == nil {
		return nil, nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.agentSession, state.startupErr
}

func (e *Engine) promoteInPlaceInteractiveState(op NewOperation) error {
	tempKey := inPlaceCandidateInteractiveKey(op)
	normalKey := inPlaceActiveInteractiveKey(op)
	e.interactiveMu.Lock()
	candidate := e.interactiveStates[tempKey]
	if candidate == nil {
		e.interactiveMu.Unlock()
		return fmt.Errorf("candidate interactive state %q not found", tempKey)
	}
	old := e.interactiveStates[normalKey]
	e.interactiveStates[normalKey] = candidate
	delete(e.interactiveStates, tempKey)
	e.interactiveMu.Unlock()

	if old != nil && old != candidate {
		e.retireDetachedInteractiveState(normalKey, old)
	}
	return nil
}

func (e *Engine) retireDetachedInteractiveState(sessionKey string, state *interactiveState) {
	e.stopUnsolicitedReader(state)
	state.mu.Lock()
	agentSession := state.agentSession
	state.agentSession = nil
	pending := state.pending
	state.pending = nil
	state.mu.Unlock()
	state.markStopped()
	if pending != nil {
		pending.resolve()
	}
	e.notifyDroppedQueuedMessages(state, fmt.Errorf("session switched"))
	e.closeAgentSessionAsync(sessionKey, agentSession)
}

func (e *Engine) cleanupInPlaceCandidate(op NewOperation) {
	tempKey := inPlaceCandidateInteractiveKey(op)
	e.interactiveMu.Lock()
	state := e.interactiveStates[tempKey]
	e.interactiveMu.Unlock()
	if state != nil {
		e.cleanupInteractiveState(tempKey, state)
	}
}

func (e *Engine) failInPlaceNewOperationWithNotice(ctx context.Context, p Platform, op NewOperation, sourceReplyCtx any, err error) {
	e.cleanupInPlaceCandidate(op)
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
		message = e.i18n.Tf(MsgNewInPlaceRetryScheduled, op.Name)
	case NewOperationManualAction:
		if errors.Is(err, ErrActiveSessionChanged) {
			message = e.i18n.Tf(MsgNewInPlaceActiveChanged, op.Name)
		} else {
			message = e.i18n.Tf(MsgNewInPlaceManualAction, op.Name)
		}
	default:
		return
	}
	if sourceReplyCtx == nil {
		var reconstructErr error
		sourceReplyCtx, reconstructErr = reconstructNewOperationReplyContext(p, op.SourceSessionKey)
		if reconstructErr != nil {
			slog.Warn("in-place new operation: cannot reconstruct source reply context", "operation_id", op.ID, "error", reconstructErr)
			return
		}
	}
	message = e.decorateSessionReply(p, sourceReplyCtx, message)
	_ = p.Reply(ctx, sourceReplyCtx, message)
}
