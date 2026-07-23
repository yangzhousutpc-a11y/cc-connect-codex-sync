package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
	"golang.org/x/sys/unix"
)

type desktopPollState struct {
	path                 string
	offset               int64
	initialized          bool
	externalTurn         bool
	deferredTurn         bool
	deferredUser         desktopRawUserEvent
	deferred             []desktopDeferredTurn
	turnID               string
	lastLookup           time.Time
	sawTaskStarted       bool
	sawUserMessage       bool
	unsafeTurn           bool
	compatibilityHealthy bool
}

type desktopDeferredTurn struct {
	turnID    string
	user      desktopRawUserEvent
	assistant string
}

type desktopRawUserEvent struct {
	message     string
	localImages []string
}

type desktopRolloutEvent struct {
	Type    string `json:"type"`
	Payload struct {
		Type             string   `json:"type"`
		TurnID           string   `json:"turn_id"`
		ClientID         string   `json:"client_id"`
		Message          string   `json:"message"`
		LocalImages      []string `json:"local_images"`
		LastAgentMessage string   `json:"last_agent_message"`
	} `json:"payload"`
}

const desktopAttachmentMaxBytes int64 = 20 << 20

var desktopFileEntryPattern = regexp.MustCompile(`^## ([^:\r\n]+): (/[^\r\n]+)$`)

var desktopAttachmentOpen = openDesktopAttachmentNoFollow

type desktopOriginTicket struct {
	sessionID string
	id        uint64
}

type desktopOriginSession struct {
	pending        map[uint64]time.Time
	turns          map[string]time.Time
	expiredPending int
}

// desktopOriginTracker records turns started by cc-connect itself. Recent
// Codex releases may omit event_msg.payload.client_id for both app-originated
// and API-originated turns, so origin must be determined from turn identity.
type desktopOriginTracker struct {
	mu       sync.Mutex
	nextID   uint64
	sessions map[string]*desktopOriginSession
}

const (
	desktopOriginTTL        = 5 * time.Minute
	desktopOriginMaxPending = 64
	desktopOriginMaxTurns   = 256
)

func (t *desktopOriginTracker) begin(sessionID string) desktopOriginTicket {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return desktopOriginTicket{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.pruneLocked(now)
	t.nextID++
	ticket := desktopOriginTicket{sessionID: sessionID, id: t.nextID}
	state := t.sessionLocked(sessionID)
	if state.pending == nil {
		state.pending = make(map[uint64]time.Time)
	}
	for len(state.pending) >= desktopOriginMaxPending {
		deleteOldestDesktopOriginPending(state.pending)
		if state.expiredPending < desktopOriginMaxPending {
			state.expiredPending++
		}
	}
	state.pending[ticket.id] = now
	return ticket
}

func (t *desktopOriginTracker) complete(ticket desktopOriginTicket, turnID string) {
	turnID = strings.TrimSpace(turnID)
	if ticket.id == 0 || turnID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.pruneLocked(now)
	state := t.sessions[ticket.sessionID]
	if state == nil {
		return
	}
	if _, ok := state.pending[ticket.id]; ok {
		delete(state.pending, ticket.id)
		t.registerTurnLocked(state, turnID, now)
		return
	}
}

func (t *desktopOriginTracker) reconcile(sessionID, turnID string) {
	sessionID = strings.TrimSpace(sessionID)
	turnID = strings.TrimSpace(turnID)
	if sessionID == "" || turnID == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.pruneLocked(now)
	state := t.sessions[sessionID]
	if state == nil {
		return
	}
	if len(state.pending) > 0 {
		deleteOldestDesktopOriginPending(state.pending)
		t.registerTurnLocked(state, turnID, now)
		return
	}
	if state.expiredPending > 0 {
		state.expiredPending--
		t.registerTurnLocked(state, turnID, now)
		t.cleanupLocked(sessionID, state)
	}
}

func (t *desktopOriginTracker) cancel(ticket desktopOriginTicket) {
	if ticket.id == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.sessions[ticket.sessionID]
	if state == nil {
		return
	}
	delete(state.pending, ticket.id)
	t.cleanupLocked(ticket.sessionID, state)
}

func (t *desktopOriginTracker) register(sessionID, turnID string) {
	sessionID = strings.TrimSpace(sessionID)
	turnID = strings.TrimSpace(turnID)
	if sessionID == "" || turnID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.pruneLocked(now)
	state := t.sessionLocked(sessionID)
	t.registerTurnLocked(state, turnID, now)
}

func (t *desktopOriginTracker) classify(sessionID, turnID string) (internal bool, pending bool) {
	sessionID = strings.TrimSpace(sessionID)
	turnID = strings.TrimSpace(turnID)
	if sessionID == "" {
		return false, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(time.Now())
	state := t.sessions[sessionID]
	if state == nil {
		return false, false
	}
	if turnID != "" {
		if _, ok := state.turns[turnID]; ok {
			delete(state.turns, turnID)
			t.cleanupLocked(sessionID, state)
			return true, false
		}
	}
	if state.expiredPending > 0 {
		state.expiredPending--
		t.cleanupLocked(sessionID, state)
		return true, false
	}
	return false, len(state.pending) > 0
}

func (t *desktopOriginTracker) reset(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, sessionID)
}

func (t *desktopOriginTracker) sessionLocked(sessionID string) *desktopOriginSession {
	if t.sessions == nil {
		t.sessions = make(map[string]*desktopOriginSession)
	}
	state := t.sessions[sessionID]
	if state == nil {
		state = &desktopOriginSession{}
		t.sessions[sessionID] = state
	}
	return state
}

func (t *desktopOriginTracker) registerTurnLocked(state *desktopOriginSession, turnID string, now time.Time) {
	if state.turns == nil {
		state.turns = make(map[string]time.Time)
	}
	for len(state.turns) >= desktopOriginMaxTurns {
		deleteOldestDesktopOriginTurn(state.turns)
	}
	state.turns[turnID] = now
}

func (t *desktopOriginTracker) pruneLocked(now time.Time) {
	for sessionID, state := range t.sessions {
		for id, createdAt := range state.pending {
			if now.Sub(createdAt) > desktopOriginTTL {
				delete(state.pending, id)
				if state.expiredPending < desktopOriginMaxPending {
					state.expiredPending++
				}
			}
		}
		for turnID, createdAt := range state.turns {
			if now.Sub(createdAt) > desktopOriginTTL {
				delete(state.turns, turnID)
			}
		}
		t.cleanupLocked(sessionID, state)
	}
}

func deleteOldestDesktopOriginPending(entries map[uint64]time.Time) {
	var oldestID uint64
	var oldestTime time.Time
	for id, createdAt := range entries {
		if oldestTime.IsZero() || createdAt.Before(oldestTime) {
			oldestID, oldestTime = id, createdAt
		}
	}
	delete(entries, oldestID)
}

func deleteOldestDesktopOriginTurn(entries map[string]time.Time) {
	oldestID := ""
	var oldestTime time.Time
	for id, createdAt := range entries {
		if oldestTime.IsZero() || createdAt.Before(oldestTime) {
			oldestID, oldestTime = id, createdAt
		}
	}
	delete(entries, oldestID)
}

func (t *desktopOriginTracker) cleanupLocked(sessionID string, state *desktopOriginSession) {
	if len(state.pending) == 0 && len(state.turns) == 0 && state.expiredPending == 0 {
		delete(t.sessions, sessionID)
	}
}

func (a *Agent) SetExternalConversationRoutes(sessionIDs []string) {
	next := make(map[string]struct{}, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
			next[sessionID] = struct{}{}
		}
	}

	a.desktopPollMu.Lock()
	changed := len(next) != len(a.desktopRoutes)
	if !changed {
		for sessionID := range next {
			if _, ok := a.desktopRoutes[sessionID]; !ok {
				changed = true
				break
			}
		}
	}
	for sessionID := range a.desktopRoutes {
		if _, remainsActive := next[sessionID]; !remainsActive {
			delete(a.desktopPolls, sessionID)
			a.desktopOrigins.reset(sessionID)
		}
	}
	for sessionID := range a.desktopPolls {
		if _, remainsActive := next[sessionID]; !remainsActive {
			delete(a.desktopPolls, sessionID)
		}
	}
	a.desktopRoutes = next
	hasActiveHealthyRoute := false
	for sessionID := range a.desktopRoutes {
		if state := a.desktopPolls[sessionID]; state != nil && state.compatibilityHealthy {
			hasActiveHealthyRoute = true
			break
		}
	}
	logRecord := a.compatibility.setRoutesWithActiveHealthyRouteState(len(next), changed, hasActiveHealthyRoute)
	a.desktopPollMu.Unlock()
	logCompatibilityStateChange(logRecord)
}

func (a *Agent) PollExternalConversation(_ context.Context, sessionID string) ([]core.ExternalConversationEvent, error) {
	a.mu.RLock()
	enabled := a.desktopLiveSync
	codexHome := a.codexHome
	a.mu.RUnlock()
	if !enabled || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}

	var logRecords []*compatibilityStateChangeLog
	recordCompatibilityChange := func(record *compatibilityStateChangeLog) {
		if record != nil {
			logRecords = append(logRecords, record)
		}
	}
	a.desktopPollMu.Lock()
	defer func() {
		a.desktopPollMu.Unlock()
		for _, record := range logRecords {
			logCompatibilityStateChange(record)
		}
	}()
	if a.desktopPolls == nil {
		a.desktopPolls = make(map[string]*desktopPollState)
	}
	state := a.desktopPolls[sessionID]
	if state == nil {
		state = &desktopPollState{}
		a.desktopPolls[sessionID] = state
	}
	if state.path == "" {
		if !state.lastLookup.IsZero() && time.Since(state.lastLookup) < 30*time.Second {
			recordCompatibilityChange(a.compatibilityTranscriptMissingState(state))
			return nil, nil
		}
		state.lastLookup = time.Now()
		path, lookupErr := findDesktopTranscript(sessionID, codexHome)
		if lookupErr != nil {
			recordCompatibilityChange(a.compatibility.degradedState("transcript_lookup_failed"))
			return nil, lookupErr
		}
		state.path = path
		if state.path == "" {
			recordCompatibilityChange(a.compatibilityTranscriptMissingState(state))
			return nil, nil
		}
		recordCompatibilityChange(a.compatibility.transcriptFoundState())
	}

	info, err := os.Stat(state.path)
	if err != nil {
		if os.IsNotExist(err) {
			state.path = ""
			state.initialized = false
			recordCompatibilityChange(a.compatibilityTranscriptMissingState(state))
			return nil, nil
		}
		recordCompatibilityChange(a.compatibility.degradedState("transcript_stat_failed"))
		return nil, err
	}
	events := a.resolveDeferredDesktopTurns(sessionID, state)
	if !state.initialized || info.Size() < state.offset {
		state.offset = info.Size()
		state.initialized = true
		state.externalTurn = false
		state.deferredTurn = false
		state.deferredUser = desktopRawUserEvent{}
		state.deferred = nil
		state.turnID = ""
		state.sawTaskStarted = false
		state.sawUserMessage = false
		state.unsafeTurn = false
		return events, nil
	}
	if info.Size() == state.offset {
		return events, nil
	}

	f, err := os.Open(state.path)
	if err != nil {
		recordCompatibilityChange(a.compatibility.degradedState("transcript_open_failed"))
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(state.offset, io.SeekStart); err != nil {
		recordCompatibilityChange(a.compatibility.degradedState("transcript_seek_failed"))
		return nil, err
	}

	reader := bufio.NewReader(f)
	offset := state.offset
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			recordCompatibilityChange(a.compatibility.degradedState("transcript_read_failed"))
			return nil, err
		}
		offset += int64(len(line))
		var rollout desktopRolloutEvent
		if err := json.Unmarshal([]byte(line), &rollout); err != nil {
			recordCompatibilityChange(a.compatibility.degradedState("transcript_json_invalid"))
			continue
		}
		if rollout.Type != "event_msg" {
			continue
		}
		switch rollout.Payload.Type {
		case "task_started":
			state.turnID = strings.TrimSpace(rollout.Payload.TurnID)
			state.sawTaskStarted = true
			state.sawUserMessage = false
			state.unsafeTurn = false
			state.externalTurn = false
			state.deferredTurn = false
			state.deferredUser = desktopRawUserEvent{}
		case "user_message":
			clientID := strings.TrimSpace(rollout.Payload.ClientID)
			if state.turnID == "" && clientID == "" {
				state.unsafeTurn = true
				state.externalTurn = false
				state.deferredTurn = false
				state.deferredUser = desktopRawUserEvent{}
				recordCompatibilityChange(a.compatibility.incompatibleState("unsafe_turn_identity"))
				continue
			}
			state.sawUserMessage = true
			internal, pending := a.desktopOrigins.classify(sessionID, state.turnID)
			if internal {
				state.externalTurn = false
				continue
			}
			rawUser := desktopRawUserEvent{
				message:     rollout.Payload.Message,
				localImages: append([]string(nil), rollout.Payload.LocalImages...),
			}
			if clientID == "" && pending {
				state.deferredTurn = true
				state.deferredUser = rawUser
				continue
			}
			state.externalTurn = true
			userEvent := materializeDesktopUserEvent(sessionID, rawUser)
			if desktopEventHasContent(userEvent) {
				events = append(events, userEvent)
			}
		case "task_complete":
			assistant := strings.TrimSpace(rollout.Payload.LastAgentMessage)
			if state.unsafeTurn {
				state.turnID = ""
				state.sawTaskStarted = false
				state.sawUserMessage = false
				state.unsafeTurn = false
				state.externalTurn = false
				continue
			}
			supportedSequence := state.sawTaskStarted && state.sawUserMessage
			if state.deferredTurn {
				internal, pending := a.desktopOrigins.classify(sessionID, state.turnID)
				switch {
				case internal:
				case pending:
					state.deferred = append(state.deferred, desktopDeferredTurn{turnID: state.turnID, user: state.deferredUser, assistant: assistant})
				default:
					events = appendDesktopTurnEvents(events, materializeDesktopUserEvent(sessionID, state.deferredUser), assistant)
				}
				state.turnID = ""
				state.deferredTurn = false
				state.deferredUser = desktopRawUserEvent{}
				state.sawTaskStarted = false
				state.sawUserMessage = false
				if supportedSequence {
					state.compatibilityHealthy = true
					recordCompatibilityChange(a.compatibility.supportedTurnState())
				}
				continue
			}
			state.turnID = ""
			state.sawTaskStarted = false
			state.sawUserMessage = false
			if supportedSequence {
				state.compatibilityHealthy = true
				recordCompatibilityChange(a.compatibility.supportedTurnState())
			}
			if !state.externalTurn {
				continue
			}
			state.externalTurn = false
			if assistant != "" {
				events = append(events, core.ExternalConversationEvent{SessionID: sessionID, Role: "assistant", Content: assistant})
			}
		}
	}
	state.offset = offset
	return events, nil
}

func (a *Agent) compatibilityTranscriptMissingState(state *desktopPollState) *compatibilityStateChangeLog {
	if state.compatibilityHealthy {
		return a.compatibility.degradedState("transcript_missing")
	}
	for sessionID := range a.desktopRoutes {
		if routeState := a.desktopPolls[sessionID]; routeState != nil && routeState.compatibilityHealthy {
			return nil
		}
	}
	return a.compatibility.transcriptMissingState()
}

func (a *Agent) resolveDeferredDesktopTurns(sessionID string, state *desktopPollState) []core.ExternalConversationEvent {
	if len(state.deferred) == 0 {
		return nil
	}
	remaining := state.deferred[:0]
	var events []core.ExternalConversationEvent
	for _, deferred := range state.deferred {
		internal, pending := a.desktopOrigins.classify(sessionID, deferred.turnID)
		switch {
		case internal:
		case pending:
			remaining = append(remaining, deferred)
		default:
			events = appendDesktopTurnEvents(events, materializeDesktopUserEvent(sessionID, deferred.user), deferred.assistant)
		}
	}
	state.deferred = remaining
	return events
}

func appendDesktopTurnEvents(events []core.ExternalConversationEvent, user core.ExternalConversationEvent, assistant string) []core.ExternalConversationEvent {
	user.Content = strings.TrimSpace(user.Content)
	if desktopEventHasContent(user) {
		events = append(events, user)
	}
	if assistant = strings.TrimSpace(assistant); assistant != "" {
		events = append(events, core.ExternalConversationEvent{SessionID: user.SessionID, Role: "assistant", Content: assistant})
	}
	return events
}

func desktopEventHasContent(event core.ExternalConversationEvent) bool {
	return strings.TrimSpace(event.Content) != "" || len(event.Images) > 0
}

func materializeDesktopUserEvent(sessionID string, raw desktopRawUserEvent) core.ExternalConversationEvent {
	event := core.ExternalConversationEvent{SessionID: sessionID, Role: "user"}
	for _, path := range raw.localImages {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		attachment, ok := loadDesktopImage(path)
		if ok {
			event.Images = append(event.Images, attachment)
		}
	}
	event.Content = sanitizeDesktopFileMentions(raw.message)
	return event
}

func sanitizeDesktopFileMentions(message string) string {
	lines := strings.Split(message, "\n")
	start := -1
	for i, line := range lines {
		if line == "# Files mentioned by the user:" {
			start = i
			break
		}
	}
	if start < 0 {
		return strings.TrimSpace(message)
	}
	for i := start + 1; i < len(lines); i++ {
		if lines[i] == "## My request for Codex:" {
			return joinDesktopSafeSections(lines[:start], lines[i+1:])
		}
	}
	safe := append([]string(nil), lines[:start]...)
	for _, line := range lines[start+1:] {
		if desktopFileEntryPattern.MatchString(line) {
			continue
		}
		safe = append(safe, line)
	}
	return strings.TrimSpace(strings.Join(safe, "\n"))
}

func joinDesktopSafeSections(prefix, suffix []string) string {
	before := strings.TrimSpace(strings.Join(prefix, "\n"))
	after := strings.TrimSpace(strings.Join(suffix, "\n"))
	switch {
	case before == "":
		return after
	case after == "":
		return before
	default:
		return before + "\n" + after
	}
}

func loadDesktopImage(path string) (core.ImageAttachment, bool) {
	data, name, mimeType, ok := readDesktopAttachment(path)
	if !ok || !strings.HasPrefix(mimeType, "image/") {
		return core.ImageAttachment{}, false
	}
	return core.ImageAttachment{MimeType: mimeType, Data: data, FileName: name}, true
}

func readDesktopAttachment(path string) ([]byte, string, string, bool) {
	if !filepath.IsAbs(path) {
		return nil, "", "", false
	}
	file, err := desktopAttachmentOpen(path)
	if err != nil {
		return nil, "", "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() ||
		info.Size() < 0 || info.Size() > desktopAttachmentMaxBytes {
		return nil, "", "", false
	}
	data, err := io.ReadAll(io.LimitReader(file, desktopAttachmentMaxBytes+1))
	if err != nil || int64(len(data)) > desktopAttachmentMaxBytes {
		return nil, "", "", false
	}
	name := filepath.Base(path)
	mimeType := http.DetectContentType(data)
	if mimeType == "application/octet-stream" {
		if byExtension := mime.TypeByExtension(filepath.Ext(name)); byExtension != "" {
			mimeType = byExtension
		}
	}
	return data, name, mimeType, true
}

func openDesktopAttachmentNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "desktop-image")
	if file == nil {
		_ = unix.Close(fd)
		return nil, os.ErrInvalid
	}
	return file, nil
}

func findDesktopTranscript(sessionID, codexHome string) (string, error) {
	pattern := filepath.Join(resolveCodexHomeDir(codexHome), "sessions", "*", "*", "*", "rollout-*"+sessionID+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return "", err
	}
	return matches[0], nil
}
