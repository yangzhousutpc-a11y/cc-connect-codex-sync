package core

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

const desktopLiveSyncPollInterval = 400 * time.Millisecond

func (e *Engine) startDesktopLiveSync() {
	poller, ok := e.agent.(ExternalConversationPoller)
	if !ok {
		return
	}
	go e.runDesktopLiveSync(e.ctx, poller)
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

func (e *Engine) pollDesktopLiveSync(ctx context.Context, poller ExternalConversationPoller) {
	e.desktopSyncMu.Lock()
	defer e.desktopSyncMu.Unlock()

	e.pollDesktopLiveSyncRoutes(ctx, poller, e.sessions)
	if e.workspacePool == nil {
		return
	}
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
		e.pollDesktopLiveSyncRoutes(ctx, workspacePoller, sessions)
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
	routes := e.externalConversationRoutes(sessions.AgentSessionRoutes())
	setExternalConversationRoutes(poller, routes)
	e.pruneDesktopSyncPending(sessions, routes)
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
				e.desktopSyncPending[pendingKey] = events[i+1:]
				continue
			}

			if content != "" {
				prefix := "✣ Codex · 回复\n"
				if event.Role == "user" {
					prefix = "✣ Codex App · 你\n"
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

			e.desktopSyncPending[pendingKey] = events[i+1:]
			slog.Info("desktop live sync sent",
				"role", event.Role,
				"content_len", len(content),
				"image_count", imageCount,
			)
		}
		if len(e.desktopSyncPending[pendingKey]) == 0 {
			delete(e.desktopSyncPending, pendingKey)
		}
	}
}

func (e *Engine) pruneDesktopSyncPending(sessions *SessionManager, routes map[string]string) {
	for key := range e.desktopSyncPending {
		if key.sessions != sessions {
			continue
		}
		if routes[key.sessionID] != key.sessionKey {
			delete(e.desktopSyncPending, key)
		}
	}
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
