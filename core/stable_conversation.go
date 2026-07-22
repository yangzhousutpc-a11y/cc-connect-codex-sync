package core

import "strings"

// StableConversationSessionResolverRegistrar lets a platform recognize
// conversations created by /new and keep their group-level session key.
type StableConversationSessionResolverRegistrar interface {
	SetStableConversationSessionResolver(resolver func(sessionKey string) bool)
}

func (e *Engine) registerStableConversationSessionResolvers() {
	for _, platform := range e.platforms {
		registrar, ok := platform.(StableConversationSessionResolverRegistrar)
		if !ok {
			continue
		}
		registrar.SetStableConversationSessionResolver(e.hasStableConversationSession)
	}
}

func (e *Engine) hasStableConversationSession(sessionKey string) bool {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return false
	}

	e.newOperationMu.Lock()
	store := e.newOperations
	e.newOperationMu.Unlock()
	if store != nil && store.RecoveryKeyForTarget(sessionKey) != "" {
		return true
	}
	if len(e.sessions.ListSessions(sessionKey)) > 0 {
		return true
	}
	_, sessions := e.sessionContextForKey(sessionKey)
	return sessions != nil && sessions != e.sessions && len(sessions.ListSessions(sessionKey)) > 0
}
