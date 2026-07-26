package core

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"time"
)

const (
	desktopLiveSyncPollInterval       = 400 * time.Millisecond
	desktopNameReassertRetryInitial   = desktopLiveSyncPollInterval
	desktopNameReassertRetryMaximum   = 30 * time.Second
	desktopNameReassertPendingTTL     = 30 * time.Minute
	desktopNameReassertPendingMaximum = 512
	desktopSyncCompletionPendingTTL   = desktopNameReassertPendingTTL
	desktopSyncCompletionPendingMax   = desktopNameReassertPendingMaximum
)

type desktopNameReassertKey struct {
	sessions   *SessionManager
	sessionID  string
	sessionKey string
}

type desktopNameReassertState struct {
	agent          Agent
	interactiveKey string
	name           string
	generation     uint64
	attempts       int
	nextAttempt    time.Time
	createdAt      time.Time
	inFlight       bool
	inFlightGen    uint64
}

type desktopNameReassertRequest struct {
	key            desktopNameReassertKey
	agent          Agent
	interactiveKey string
	name           string
	generation     uint64
}

type desktopNameReassertResult struct {
	request   desktopNameReassertRequest
	attempted bool
	err       error
}

type desktopSyncCompletionState struct {
	createdAt time.Time
}

func (e *Engine) startDesktopLiveSync() {
	poller, ok := e.agent.(ExternalConversationPoller)
	if !ok {
		return
	}
	go e.runDesktopLiveSync(e.ctx, poller)
	go e.runDesktopNameReassertWorker(e.ctx)
}

func (e *Engine) runDesktopLiveSync(ctx context.Context, poller ExternalConversationPoller) {
	ticker := time.NewTicker(desktopLiveSyncPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.pollDesktopLiveSync(ctx, poller)
		}
	}
}

func (e *Engine) runDesktopNameReassertWorker(ctx context.Context) {
	ticker := time.NewTicker(desktopLiveSyncPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.processDueDesktopNameReasserts()
		}
	}
}

func (e *Engine) pollDesktopLiveSync(ctx context.Context, poller ExternalConversationPoller) {
	e.desktopSyncMu.Lock()
	defer e.desktopSyncMu.Unlock()
	e.pollDesktopLiveSyncRouteContext(ctx, poller, e.agent, e.sessions, nil)
	if e.workspacePool != nil {
		e.desktopSyncRestoreOnce.Do(e.restoreDesktopLiveSyncWorkspaces)
		for _, workspace := range e.workspacePool.All() {
			workspace.mu.Lock()
			agent, sessions := workspace.agent, workspace.sessions
			workspace.mu.Unlock()
			workspacePoller, ok := agent.(ExternalConversationPoller)
			if !ok || sessions == nil {
				continue
			}
			if len(e.externalConversationRoutes(sessions.AgentSessionRoutes())) > 0 {
				workspace.Touch()
			}
			e.pollDesktopLiveSyncRouteContext(
				ctx,
				workspacePoller,
				agent,
				sessions,
				workspace,
			)
		}
	}
}

func (e *Engine) processDueDesktopNameReasserts() {
	for {
		e.desktopSyncMu.Lock()
		requests := e.collectDueDesktopNameReassertsLocked(e.desktopNameReassertTime())
		execute := e.desktopNameReassertExecute
		e.desktopSyncMu.Unlock()
		if len(requests) == 0 {
			return
		}

		if execute == nil {
			execute = e.executeDesktopNameReassert
		}
		for _, request := range requests {
			result := execute(request)
			e.desktopSyncMu.Lock()
			e.commitDesktopNameReassertLocked(result, e.desktopNameReassertTime())
			e.desktopSyncMu.Unlock()
		}
	}
}

type desktopSyncPendingKey struct {
	sessions   *SessionManager
	sessionID  string
	sessionKey string
}

func (e *Engine) restoreDesktopLiveSyncWorkspaces() {
	if !e.multiWorkspace || e.workspaceBindings == nil || e.workspacePool == nil {
		return
	}

	bindings := e.workspaceBindings.ListByProject(sharedWorkspaceBindingsKey)
	for channelKey, binding := range e.workspaceBindings.ListByProject("project:" + e.name) {
		bindings[channelKey] = binding
	}
	workspaces := make(map[string]struct{}, len(bindings))
	for channelKey := range bindings {
		binding, _, usable := e.lookupEffectiveWorkspaceBinding(channelKey)
		if binding == nil || !usable {
			continue
		}
		workspace := normalizeWorkspacePath(binding.Workspace)
		if _, seen := workspaces[workspace]; seen {
			continue
		}
		workspaces[workspace] = struct{}{}
		if _, _, err := e.getOrCreateWorkspaceAgent(workspace); err != nil {
			slog.Warn("desktop live sync workspace restore failed")
		}
	}
}

func (e *Engine) pollDesktopLiveSyncRoutes(ctx context.Context, poller ExternalConversationPoller, sessions *SessionManager) {
	agent, _ := poller.(Agent)
	e.pollDesktopLiveSyncRouteContext(ctx, poller, agent, sessions, nil)
}

func (e *Engine) pollDesktopLiveSyncRouteContext(
	ctx context.Context,
	poller ExternalConversationPoller,
	agent Agent,
	sessions *SessionManager,
	workspace *workspaceState,
) {
	routes := e.externalConversationRoutes(sessions.AgentSessionRoutes())
	setExternalConversationRoutes(poller, routes)
	e.pruneDesktopSyncPending(sessions, routes, agent, workspace)
	for sessionID, sessionKey := range routes {
		platformName := sessionKeyPlatform(sessionKey)
		platform := e.lookupReadyPlatform(platformName)
		if platform == nil {
			continue
		}
		reconstructor, ok := platform.(ReplyContextReconstructor)
		if !ok {
			continue
		}

		pendingKey := desktopSyncPendingKey{sessions: sessions, sessionID: sessionID, sessionKey: sessionKey}
		events := e.desktopSyncPending[pendingKey]
		if len(events) == 0 {
			var err error
			events, err = poller.PollExternalConversation(ctx, sessionID)
			if err != nil {
				slog.Warn("desktop live sync poll failed")
				continue
			}
			if len(events) > 0 {
				if e.desktopSyncPending == nil {
					e.desktopSyncPending = make(map[desktopSyncPendingKey][]ExternalConversationEvent)
				}
				e.desktopSyncPending[pendingKey] = events
			}
		}
		if len(events) == 0 {
			e.scheduleCompletedDesktopNameReassertLocked(pendingKey, agent, workspace)
			continue
		}
		replyCtx, err := reconstructor.ReconstructReplyCtx(sessionKey)
		if err != nil {
			slog.Warn("desktop live sync reply context failed")
			continue
		}

	eventLoop:
		for i := range events {
			// The active session may have changed while PollExternalConversation
			// was reading the transcript. Never send using a stale route snapshot.
			if e.externalConversationRoutes(sessions.AgentSessionRoutes())[sessionID] != sessionKey {
				delete(e.desktopSyncPending, pendingKey)
				break
			}
			event := &events[i]
			content := strings.TrimSpace(event.Content)
			imageCount := len(event.Images)
			if content == "" && len(event.Images) == 0 {
				if event.TurnCompleted {
					e.markDesktopSyncCompletionLocked(pendingKey, e.desktopNameReassertTime())
				}
				e.desktopSyncPending[pendingKey] = events[i+1:]
				continue
			}

			if content != "" {
				prefix := "Codex · 回复\n"
				if event.Role == "user" {
					prefix = "Codex App · 你\n"
				}
				if err := e.sendWithError(platform, replyCtx, prefix+content); err != nil {
					slog.Warn("desktop live sync send failed", "role", event.Role, "stage", "text")
					e.desktopSyncPending[pendingKey] = events[i:]
					break
				}
				event.Content = ""
				e.desktopSyncPending[pendingKey] = events[i:]
			}

			if !e.attachmentSendEnabled {
				event.Images = nil
			} else {
				imageSender, imageSupported := platform.(ImageSender)
				if !imageSupported {
					event.Images = nil
				}
				for len(event.Images) > 0 {
					if e.externalConversationRoutes(sessions.AgentSessionRoutes())[sessionID] != sessionKey {
						delete(e.desktopSyncPending, pendingKey)
						break eventLoop
					}
					if err := e.waitOutgoing(platform); err != nil {
						slog.Warn("desktop live sync send failed", "role", event.Role, "stage", "image")
						e.desktopSyncPending[pendingKey] = events[i:]
						break
					}
					if err := imageSender.SendImage(e.ctx, replyCtx, event.Images[0]); err != nil {
						slog.Warn("desktop live sync send failed", "role", event.Role, "stage", "image")
						e.desktopSyncPending[pendingKey] = events[i:]
						break
					}
					event.Images = event.Images[1:]
					e.desktopSyncPending[pendingKey] = events[i:]
				}
				if len(event.Images) > 0 {
					break
				}
			}

			if event.TurnCompleted {
				e.markDesktopSyncCompletionLocked(pendingKey, e.desktopNameReassertTime())
			}
			e.desktopSyncPending[pendingKey] = events[i+1:]
			slog.Info("desktop live sync sent",
				"role", event.Role,
				"content_len", len(content),
				"image_count", imageCount,
			)
		}
		if len(e.desktopSyncPending[pendingKey]) == 0 {
			delete(e.desktopSyncPending, pendingKey)
			e.scheduleCompletedDesktopNameReassertLocked(pendingKey, agent, workspace)
		}
	}
}

func (e *Engine) markDesktopSyncCompletionLocked(key desktopSyncPendingKey, now time.Time) {
	e.pruneExpiredDesktopSyncCompletionsLocked(now)
	if e.desktopSyncCompletionPending == nil {
		e.desktopSyncCompletionPending = make(map[desktopSyncPendingKey]desktopSyncCompletionState)
	}
	if _, exists := e.desktopSyncCompletionPending[key]; exists {
		return
	}
	if len(e.desktopSyncCompletionPending) >= desktopSyncCompletionPendingMax {
		var oldestKey desktopSyncPendingKey
		var oldestTime time.Time
		found := false
		for pendingKey, state := range e.desktopSyncCompletionPending {
			if !found || state.createdAt.Before(oldestTime) {
				oldestKey, oldestTime, found = pendingKey, state.createdAt, true
			}
		}
		if found {
			delete(e.desktopSyncCompletionPending, oldestKey)
		}
	}
	e.desktopSyncCompletionPending[key] = desktopSyncCompletionState{createdAt: now}
}

func (e *Engine) pruneExpiredDesktopSyncCompletionsLocked(now time.Time) {
	for key, state := range e.desktopSyncCompletionPending {
		if state.createdAt.IsZero() || !now.Before(state.createdAt.Add(desktopSyncCompletionPendingTTL)) {
			delete(e.desktopSyncCompletionPending, key)
		}
	}
}

func (e *Engine) scheduleCompletedDesktopNameReassertLocked(
	pendingKey desktopSyncPendingKey,
	agent Agent,
	workspace *workspaceState,
) {
	e.pruneExpiredDesktopSyncCompletionsLocked(e.desktopNameReassertTime())
	if _, completed := e.desktopSyncCompletionPending[pendingKey]; !completed {
		return
	}
	interactiveKey := pendingKey.sessionKey
	if workspace != nil {
		interactiveKey = workspace.interactiveKey(pendingKey.sessions, pendingKey.sessionKey)
	}
	if e.scheduleDesktopNameReassertLocked(
		desktopNameReassertKey{
			sessions:   pendingKey.sessions,
			sessionID:  pendingKey.sessionID,
			sessionKey: pendingKey.sessionKey,
		},
		agent,
		interactiveKey,
		pendingKey.sessions.GetSessionName(pendingKey.sessionID),
		e.desktopNameReassertTime(),
	) {
		delete(e.desktopSyncCompletionPending, pendingKey)
	}
}

func (e *Engine) desktopNameReassertTime() time.Time {
	if e.desktopNameReassertNow != nil {
		return e.desktopNameReassertNow()
	}
	return time.Now()
}

func (e *Engine) scheduleDesktopNameReassertLocked(
	key desktopNameReassertKey,
	agent Agent,
	interactiveKey string,
	name string,
	now time.Time,
) bool {
	name = strings.TrimSpace(name)
	if key.sessions == nil || key.sessionID == "" || key.sessionKey == "" ||
		agent == nil || name == "" {
		return false
	}
	e.pruneExpiredDesktopNameReassertsLocked(now)
	if e.desktopNameReassertPending == nil {
		e.desktopNameReassertPending = make(map[desktopNameReassertKey]desktopNameReassertState)
	}
	state, exists := e.desktopNameReassertPending[key]
	if exists &&
		sameDesktopSyncAgent(state.agent, agent) &&
		state.interactiveKey == interactiveKey &&
		state.name == name {
		return true
	}
	if !exists &&
		len(e.desktopNameReassertPending) >= desktopNameReassertPendingMaximum {
		if !e.evictOldestDesktopNameReassertLocked() {
			return false
		}
	}
	e.desktopNameReassertGeneration++
	state.agent = agent
	state.interactiveKey = interactiveKey
	state.name = name
	state.generation = e.desktopNameReassertGeneration
	state.attempts = 0
	state.nextAttempt = now
	state.createdAt = now
	e.desktopNameReassertPending[key] = state
	return true
}

func (e *Engine) evictOldestDesktopNameReassertLocked() bool {
	var (
		oldestKey        desktopNameReassertKey
		oldestState      desktopNameReassertState
		oldestGeneration uint64
		found            bool
	)
	for key, state := range e.desktopNameReassertPending {
		if state.inFlight {
			continue
		}
		if !found || state.createdAt.Before(oldestState.createdAt) ||
			(state.createdAt.Equal(oldestState.createdAt) && state.generation < oldestGeneration) {
			oldestKey = key
			oldestState = state
			oldestGeneration = state.generation
			found = true
		}
	}
	if found {
		delete(e.desktopNameReassertPending, oldestKey)
	}
	return found
}

func (e *Engine) pruneExpiredDesktopNameReassertsLocked(now time.Time) {
	for key, state := range e.desktopNameReassertPending {
		if state.inFlight {
			continue
		}
		if state.createdAt.IsZero() || !now.Before(state.createdAt.Add(desktopNameReassertPendingTTL)) {
			delete(e.desktopNameReassertPending, key)
		}
	}
}

func (e *Engine) collectDueDesktopNameReassertsLocked(now time.Time) []desktopNameReassertRequest {
	e.pruneExpiredDesktopNameReassertsLocked(now)
	requests := make([]desktopNameReassertRequest, 0, len(e.desktopNameReassertPending))
	for key, state := range e.desktopNameReassertPending {
		if !e.desktopNameReassertRouteCurrent(key, state.name) {
			delete(e.desktopNameReassertPending, key)
			continue
		}
		if state.inFlight || state.nextAttempt.After(now) {
			continue
		}
		state.inFlight = true
		state.inFlightGen = state.generation
		e.desktopNameReassertPending[key] = state
		requests = append(requests, desktopNameReassertRequest{
			key:            key,
			agent:          state.agent,
			interactiveKey: state.interactiveKey,
			name:           state.name,
			generation:     state.generation,
		})
	}
	return requests
}

func (e *Engine) desktopNameReassertRouteCurrent(key desktopNameReassertKey, name string) bool {
	if key.sessions == nil {
		return false
	}
	routes := e.externalConversationRoutes(key.sessions.AgentSessionRoutes())
	return routes[key.sessionID] == key.sessionKey &&
		strings.TrimSpace(key.sessions.GetSessionName(key.sessionID)) == name
}

func (e *Engine) desktopNameReassertRequestCurrent(request desktopNameReassertRequest) bool {
	e.desktopSyncMu.Lock()
	state, ok := e.desktopNameReassertPending[request.key]
	current := ok &&
		state.inFlight &&
		state.inFlightGen == request.generation &&
		state.generation == request.generation &&
		state.interactiveKey == request.interactiveKey &&
		state.name == request.name &&
		sameDesktopSyncAgent(state.agent, request.agent)
	e.desktopSyncMu.Unlock()
	return current && e.desktopNameReassertRouteCurrent(request.key, request.name)
}

func (e *Engine) executeDesktopNameReassert(request desktopNameReassertRequest) desktopNameReassertResult {
	result := desktopNameReassertResult{request: request}
	if !e.desktopNameReassertRequestCurrent(request) {
		return result
	}
	result.attempted = true
	result.err = e.reassertDesktopSessionName(
		request.agent,
		request.key.sessionID,
		request.interactiveKey,
		request.name,
	)
	return result
}

func (e *Engine) commitDesktopNameReassertLocked(result desktopNameReassertResult, now time.Time) {
	state, ok := e.desktopNameReassertPending[result.request.key]
	if !ok || !state.inFlight || state.inFlightGen != result.request.generation {
		return
	}
	state.inFlight = false
	state.inFlightGen = 0
	if state.generation != result.request.generation {
		if !e.desktopNameReassertRouteCurrent(result.request.key, state.name) {
			delete(e.desktopNameReassertPending, result.request.key)
			return
		}
		state.nextAttempt = now
		e.desktopNameReassertPending[result.request.key] = state
		return
	}
	if !result.attempted || !e.desktopNameReassertRouteCurrent(result.request.key, result.request.name) {
		delete(e.desktopNameReassertPending, result.request.key)
		return
	}
	if result.err == nil {
		delete(e.desktopNameReassertPending, result.request.key)
		slog.Info("desktop live sync name reasserted")
		return
	}
	if errors.Is(result.err, ErrAgentSessionNameUnsupported) {
		delete(e.desktopNameReassertPending, result.request.key)
		slog.Warn("desktop live sync name reassert unsupported")
		return
	}
	if state.createdAt.IsZero() || !now.Before(state.createdAt.Add(desktopNameReassertPendingTTL)) {
		delete(e.desktopNameReassertPending, result.request.key)
		slog.Warn("desktop live sync name reassert expired")
		return
	}
	state.attempts++
	state.nextAttempt = now.Add(desktopNameReassertRetryDelay(state.attempts))
	e.desktopNameReassertPending[result.request.key] = state
	slog.Warn("desktop live sync name reassert failed", "attempt", state.attempts)
}

func desktopNameReassertRetryDelay(attempts int) time.Duration {
	delay := desktopNameReassertRetryInitial
	for attempt := 1; attempt < attempts && delay < desktopNameReassertRetryMaximum; attempt++ {
		if delay >= desktopNameReassertRetryMaximum/2 {
			return desktopNameReassertRetryMaximum
		}
		delay *= 2
	}
	if delay > desktopNameReassertRetryMaximum {
		return desktopNameReassertRetryMaximum
	}
	return delay
}

func (e *Engine) reassertDesktopSessionName(agent Agent, sessionID, interactiveKey, name string) error {
	name = strings.TrimSpace(name)
	if agent == nil || name == "" {
		return nil
	}

	e.interactiveMu.Lock()
	state := e.interactiveStates[interactiveKey]
	if state != nil {
		state.mu.Lock()
	}
	e.interactiveMu.Unlock()
	if state != nil {
		agentSession := state.agentSession
		liveAgent := state.agent
		state.mu.Unlock()
		if sameDesktopSyncAgent(liveAgent, agent) &&
			agentSession != nil &&
			agentSession.CurrentSessionID() == sessionID {
			return forceSyncRequiredAgentSessionName(agentSession, name)
		}
	}
	return e.restoreAgentSessionName(agent, sessionID, name, true)
}

func sameDesktopSyncAgent(left, right Agent) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftType := reflect.TypeOf(left)
	if leftType != reflect.TypeOf(right) || !leftType.Comparable() {
		return false
	}
	return left == right
}

func (e *Engine) pruneDesktopSyncPending(
	sessions *SessionManager,
	routes map[string]string,
	agent Agent,
	workspace *workspaceState,
) {
	for key := range e.desktopSyncPending {
		if key.sessions != sessions {
			continue
		}
		if routes[key.sessionID] != key.sessionKey {
			delete(e.desktopSyncPending, key)
		}
	}
	for key := range e.desktopSyncCompletionPending {
		if key.sessions == sessions && routes[key.sessionID] != key.sessionKey {
			delete(e.desktopSyncCompletionPending, key)
		}
	}
	for key, state := range e.desktopNameReassertPending {
		if key.sessions != sessions {
			continue
		}
		if routes[key.sessionID] != key.sessionKey {
			delete(e.desktopNameReassertPending, key)
			continue
		}
		interactiveKey := desktopNameRouteInteractiveKey(workspace, sessions, key.sessionKey)
		if !sameDesktopSyncAgent(state.agent, agent) || state.interactiveKey != interactiveKey {
			if state.inFlight {
				e.scheduleDesktopNameReassertLocked(
					key,
					agent,
					interactiveKey,
					sessions.GetSessionName(key.sessionID),
					e.desktopNameReassertTime(),
				)
			} else {
				delete(e.desktopNameReassertPending, key)
			}
		}
	}
}

func desktopNameRouteInteractiveKey(workspace *workspaceState, sessions *SessionManager, sessionKey string) string {
	if workspace == nil {
		return sessionKey
	}
	return workspace.interactiveKey(sessions, sessionKey)
}

func (e *Engine) externalConversationRoutes(routes map[string]string) map[string]string {
	eligible := make(map[string]string)
	for sessionID, sessionKey := range routes {
		platform := e.lookupPlatform(sessionKeyPlatform(sessionKey))
		target, ok := platform.(ExternalConversationRelayTarget)
		if ok && target.ExternalConversationRelayEnabled() {
			eligible[sessionID] = sessionKey
		}
	}
	return eligible
}

func (e *Engine) activeRoutesForAgentThread(agentThreadID string) []string {
	if !ValidAgentThreadID(agentThreadID) {
		return nil
	}

	matches := e.sessions.ActiveRoutesForAgentSessionID(agentThreadID)
	if e.workspacePool == nil {
		return matches
	}

	e.desktopSyncRestoreOnce.Do(e.restoreDesktopLiveSyncWorkspaces)
	for _, workspace := range e.workspacePool.All() {
		workspace.mu.Lock()
		sessions := workspace.sessions
		workspace.mu.Unlock()
		if sessions != nil {
			matches = append(matches, sessions.ActiveRoutesForAgentSessionID(agentThreadID)...)
		}
	}
	return matches
}

// ValidAgentThreadID accepts only the UUID-shaped identifier injected by the
// Codex desktop execution environment.
func ValidAgentThreadID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 36 {
		return false
	}
	for i := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (e *Engine) lookupPlatform(platformName string) Platform {
	for _, platform := range e.platforms {
		if strings.EqualFold(platform.Name(), platformName) {
			return platform
		}
	}
	return nil
}

func sessionKeyPlatform(sessionKey string) string {
	platform, _, _ := strings.Cut(sessionKey, ":")
	return strings.TrimSpace(platform)
}

func setExternalConversationRoutes(poller any, routes map[string]string) {
	tracker, ok := poller.(ExternalConversationRouteTracker)
	if !ok {
		return
	}
	sessionIDs := make([]string, 0, len(routes))
	for sessionID := range routes {
		sessionIDs = append(sessionIDs, sessionID)
	}
	tracker.SetExternalConversationRoutes(sessionIDs)
}
