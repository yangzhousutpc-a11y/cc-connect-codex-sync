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
	latestTurnID         string
	nextTurnSequence     uint64
	turns                map[string]*desktopInFlightTurn
	legacyTurn           *desktopInFlightTurn
	deferred             []desktopDeferredTurn
	lastLookup           time.Time
	compatibilityHealthy bool
}

type desktopTurnOrigin uint8

const (
	desktopTurnUnknown desktopTurnOrigin = iota
	desktopTurnUnsafe
	desktopTurnInternal
	desktopTurnExternal
	desktopTurnDeferred
)

const (
	desktopInFlightTurnTTL = 24 * time.Hour
	desktopInFlightTurnMax = 256
	desktopDeferredTurnMax = 256
)

type desktopInFlightTurn struct {
	user           desktopRawUserEvent
	origin         desktopTurnOrigin
	sawTaskStarted bool
	sawUserMessage bool
	sequence       uint64
	startedAt      time.Time
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

func (s *desktopPollState) startTurn(turnID string) {
	now := time.Now()
	s.pruneTurns(now)
	turnID = strings.TrimSpace(turnID)
	turn := s.newTurn(now, true)
	if turnID == "" {
		s.legacyTurn = turn
	} else {
		if s.turns == nil {
			s.turns = make(map[string]*desktopInFlightTurn)
		}
		s.turns[turnID] = turn
	}
	s.pruneTurns(now)
}

func (s *desktopPollState) currentTurn() (string, *desktopInFlightTurn) {
	now := time.Now()
	s.pruneTurns(now)
	// user_message does not carry turn_id, so it belongs to the most recent
	// task_started event that has not already completed.
	if turnID, turn := s.latestTurn(); turn != nil {
		return turnID, turn
	}
	s.legacyTurn = s.newTurn(now, false)
	return "", s.legacyTurn
}

func (s *desktopPollState) takeCompletedTurn(turnID string) (string, *desktopInFlightTurn, bool) {
	s.pruneTurns(time.Now())
	turnID = strings.TrimSpace(turnID)
	if turnID != "" {
		turn, ok := s.turns[turnID]
		if !ok {
			return "", nil, false
		}
		delete(s.turns, turnID)
		s.latestTurn()
		return turnID, turn, true
	}

	// Legacy task_complete events omit turn_id. Consume one only when the
	// association is unambiguous; otherwise fail closed and preserve keyed turns.
	candidates := len(s.turns)
	if s.legacyTurn != nil {
		candidates++
	}
	if candidates != 1 {
		return "", nil, false
	}
	if s.legacyTurn != nil {
		turn := s.legacyTurn
		s.legacyTurn = nil
		s.latestTurn()
		return "", turn, true
	}
	for candidateID, turn := range s.turns {
		delete(s.turns, candidateID)
		s.latestTurn()
		return candidateID, turn, true
	}
	return "", nil, false
}

func (s *desktopPollState) newTurn(now time.Time, sawTaskStarted bool) *desktopInFlightTurn {
	s.nextTurnSequence++
	return &desktopInFlightTurn{
		sawTaskStarted: sawTaskStarted,
		sequence:       s.nextTurnSequence,
		startedAt:      now,
	}
}

func (s *desktopPollState) latestTurn() (string, *desktopInFlightTurn) {
	latestID := ""
	var latest *desktopInFlightTurn
	for turnID, turn := range s.turns {
		if turn == nil {
			continue
		}
		if latest == nil || turn.sequence > latest.sequence ||
			(turn.sequence == latest.sequence && turnID > latestID) {
			latestID = turnID
			latest = turn
		}
	}
	if s.legacyTurn != nil && (latest == nil || s.legacyTurn.sequence > latest.sequence) {
		latestID = ""
		latest = s.legacyTurn
	}
	s.latestTurnID = latestID
	return latestID, latest
}

func (s *desktopPollState) pruneTurns(now time.Time) {
	for turnID, turn := range s.turns {
		if turn == nil || turn.startedAt.IsZero() || now.Sub(turn.startedAt) > desktopInFlightTurnTTL {
			delete(s.turns, turnID)
		}
	}
	if turn := s.legacyTurn; turn != nil &&
		(turn.startedAt.IsZero() || now.Sub(turn.startedAt) > desktopInFlightTurnTTL) {
		s.legacyTurn = nil
	}
	for len(s.turns) > desktopInFlightTurnMax {
		oldestID := ""
		var oldest *desktopInFlightTurn
		for turnID, turn := range s.turns {
			if oldest == nil || turn.sequence < oldest.sequence ||
				(turn.sequence == oldest.sequence && turnID < oldestID) {
				oldestID = turnID
				oldest = turn
			}
		}
		delete(s.turns, oldestID)
	}
	s.pruneDeferredTurns()
	s.latestTurn()
}

func (s *desktopPollState) appendDeferredTurn(turn desktopDeferredTurn) {
	s.deferred = append(s.deferred, turn)
	s.pruneDeferredTurns()
}

func (s *desktopPollState) pruneDeferredTurns() {
	if len(s.deferred) <= desktopDeferredTurnMax {
		return
	}
	firstRetained := len(s.deferred) - desktopDeferredTurnMax
	copy(s.deferred, s.deferred[firstRetained:])
	clear(s.deferred[desktopDeferredTurnMax:])
	s.deferred = s.deferred[:desktopDeferredTurnMax]
}

func (s *desktopPollState) resetTurns() {
	s.latestTurnID = ""
	s.nextTurnSequence = 0
	s.turns = nil
	s.legacyTurn = nil
	s.deferred = nil
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

const (
	desktopAttachmentMaxBytes int64 = 20 << 20
	desktopImageMaxCount            = 10
)

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
	state.pruneTurns(time.Now())
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
		state.resetTurns()
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
			state.startTurn(rollout.Payload.TurnID)
		case "user_message":
			clientID := strings.TrimSpace(rollout.Payload.ClientID)
			turnID, turn := state.currentTurn()
			turn.sawUserMessage = true
			if turnID == "" && clientID == "" {
				turn.origin = desktopTurnUnsafe
				recordCompatibilityChange(a.compatibility.incompatibleState("unsafe_turn_identity"))
				continue
			}
			internal, pending := a.desktopOrigins.classify(sessionID, turnID)
			if internal {
				turn.origin = desktopTurnInternal
				continue
			}
			rawUser := desktopRawUserEvent{
				message:     rollout.Payload.Message,
				localImages: append([]string(nil), rollout.Payload.LocalImages...),
			}
			if clientID == "" && pending {
				turn.origin = desktopTurnDeferred
				turn.user = rawUser
				continue
			}
			turn.origin = desktopTurnExternal
			userEvent := materializeDesktopUserEvent(sessionID, rawUser)
			if desktopEventHasContent(userEvent) {
				events = append(events, userEvent)
			}
		case "task_complete":
			turnID, turn, ok := state.takeCompletedTurn(rollout.Payload.TurnID)
			if !ok {
				continue
			}
			assistant := strings.TrimSpace(rollout.Payload.LastAgentMessage)
			if turn.origin == desktopTurnUnsafe {
				continue
			}
			if turn.sawTaskStarted && turn.sawUserMessage {
				state.compatibilityHealthy = true
				recordCompatibilityChange(a.compatibility.supportedTurnState())
			}
			if !turn.sawUserMessage {
				continue
			}
			switch turn.origin {
			case desktopTurnInternal:
				continue
			case desktopTurnDeferred:
				internal, pending := a.desktopOrigins.classify(sessionID, turnID)
				switch {
				case internal:
				case pending:
					state.appendDeferredTurn(desktopDeferredTurn{turnID: turnID, user: turn.user, assistant: assistant})
				default:
					events = appendDesktopTurnEvents(events, materializeDesktopUserEvent(sessionID, turn.user), assistant)
				}
			case desktopTurnExternal:
				events = appendDesktopCompletionEvent(events, sessionID, assistant)
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
	return appendDesktopCompletionEvent(events, user.SessionID, assistant)
}

func appendDesktopCompletionEvent(events []core.ExternalConversationEvent, sessionID, assistant string) []core.ExternalConversationEvent {
	if assistant = strings.TrimSpace(assistant); assistant != "" {
		return append(events, core.ExternalConversationEvent{
			SessionID:     sessionID,
			Role:          "assistant",
			Content:       assistant,
			TurnCompleted: true,
		})
	}
	return append(events, core.ExternalConversationEvent{SessionID: sessionID, TurnCompleted: true})
}

func desktopEventHasContent(event core.ExternalConversationEvent) bool {
	return strings.TrimSpace(event.Content) != "" || len(event.Images) > 0
}

func materializeDesktopUserEvent(sessionID string, raw desktopRawUserEvent) core.ExternalConversationEvent {
	event := core.ExternalConversationEvent{SessionID: sessionID, Role: "user"}
	remainingBytes := desktopAttachmentMaxBytes
	for i, path := range raw.localImages {
		if i >= desktopImageMaxCount || remainingBytes <= 0 {
			break
		}
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		attachment, consumed, ok, exhausted := loadDesktopImageWithinBudget(path, remainingBytes)
		if exhausted {
			break
		}
		remainingBytes -= consumed
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
	return strings.TrimSpace(strings.Join(lines[:start], "\n"))
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
	attachment, _, ok, _ := loadDesktopImageWithinBudget(path, desktopAttachmentMaxBytes)
	return attachment, ok
}

func loadDesktopImageWithinBudget(path string, maxBytes int64) (core.ImageAttachment, int64, bool, bool) {
	data, name, mimeType, consumed, ok, exhausted := readDesktopAttachment(path, maxBytes)
	if !ok || !strings.HasPrefix(mimeType, "image/") {
		return core.ImageAttachment{}, consumed, false, exhausted
	}
	return core.ImageAttachment{MimeType: mimeType, Data: data, FileName: name}, consumed, true, false
}

func readDesktopAttachment(path string, maxBytes int64) ([]byte, string, string, int64, bool, bool) {
	if !filepath.IsAbs(path) || maxBytes <= 0 {
		return nil, "", "", 0, false, maxBytes <= 0
	}
	file, err := desktopAttachmentOpen(path)
	if err != nil {
		return nil, "", "", 0, false, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return nil, "", "", 0, false, false
	}
	if info.Size() > maxBytes || info.Size() > desktopAttachmentMaxBytes {
		return nil, "", "", 0, false, true
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	consumed := int64(len(data))
	if err != nil || consumed > maxBytes || consumed > desktopAttachmentMaxBytes {
		return nil, "", "", consumed, false, consumed > maxBytes
	}
	name := filepath.Base(path)
	mimeType := http.DetectContentType(data)
	if mimeType == "application/octet-stream" {
		if byExtension := mime.TypeByExtension(filepath.Ext(name)); byExtension != "" {
			mimeType = byExtension
		}
	}
	return data, name, mimeType, consumed, true, false
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
