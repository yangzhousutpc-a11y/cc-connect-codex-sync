package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

const (
	defaultEndpoint        = "wss://openws.work.weixin.qq.com"
	defaultHeartbeat       = 30 * time.Second
	maxReconnectBackoff    = 30 * time.Second
	defaultAckTimeout      = 5 * time.Second
	defaultMaxMessageBytes = 2000

	chatTypeSingle = 1
	chatTypeGroup  = 2
)

var errAckTimeout = errors.New("wecom: ACK timeout")

type credentialError struct{ err error }

func (e *credentialError) Error() string { return e.err.Error() }
func (e *credentialError) Unwrap() error { return e.err }

type dialWebSocket func(context.Context, string) (*websocket.Conn, error)
type backoffWaiter func(context.Context, time.Duration) bool

type Platform struct {
	botID     string
	secret    string
	allowFrom string

	endpoint          string
	dial              dialWebSocket
	backoffWait       backoffWaiter
	heartbeatInterval time.Duration
	ackTimeout        time.Duration
	maxMessageBytes   int

	startMu sync.Mutex
	started bool
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	writeMu sync.Mutex
	conn    *websocket.Conn

	handler          core.MessageHandler
	lifecycleHandler core.PlatformLifecycleHandler

	reqSeq atomic.Uint64
	dedup  core.MessageDedup

	pendingMu   sync.Mutex
	pendingAcks map[string]chan ackResult

	inboundMu         sync.Mutex
	inboundAdmissions map[string]chan struct{}
}

type replyContext struct {
	reqID    string
	chatID   string
	targetID string
	chatType int
	userID   string
}

type wsFrame struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers wsFrameHeaders  `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode *int            `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

type wsFrameHeaders struct {
	ReqID string `json:"req_id"`
}

type wsMsgCallbackBody struct {
	MsgID    string `json:"msgid"`
	AibotID  string `json:"aibotid"`
	ChatID   string `json:"chatid"`
	ChatType string `json:"chattype"`
	From     struct {
		UserID string `json:"userid"`
	} `json:"from"`
	MsgType string `json:"msgtype"`
	Text    struct {
		Content string `json:"content"`
	} `json:"text"`
	Voice struct {
		Text    string `json:"text,omitempty"`
		Content string `json:"content,omitempty"`
	} `json:"voice"`
	Image      json.RawMessage `json:"image,omitempty"`
	File       json.RawMessage `json:"file,omitempty"`
	Mixed      json.RawMessage `json:"mixed,omitempty"`
	CreateTime int64           `json:"create_time"`
}

type ackResult struct {
	frame wsFrame
	err   error
}

func init() {
	core.RegisterPlatform("wecom", New)
}

func New(opts map[string]any) (core.Platform, error) {
	mode, _ := opts["mode"].(string)
	mode = strings.TrimSpace(mode)
	if mode != "" && mode != "websocket" {
		return nil, fmt.Errorf("wecom: unsupported mode %q; only websocket is supported", mode)
	}
	botID, _ := opts["bot_id"].(string)
	secret, _ := opts["bot_secret"].(string)
	botID = strings.TrimSpace(botID)
	secret = strings.TrimSpace(secret)
	if botID == "" || secret == "" {
		return nil, errors.New("wecom: bot_id and bot_secret are required")
	}
	allowFrom, _ := opts["allow_from"].(string)
	core.CheckAllowFrom("wecom", allowFrom)

	return &Platform{
		botID:             botID,
		secret:            secret,
		allowFrom:         allowFrom,
		endpoint:          defaultEndpoint,
		dial:              defaultDial,
		backoffWait:       waitBackoff,
		heartbeatInterval: defaultHeartbeat,
		ackTimeout:        defaultAckTimeout,
		maxMessageBytes:   defaultMaxMessageBytes,
		done:              make(chan struct{}),
		pendingAcks:       make(map[string]chan ackResult),
		inboundAdmissions: make(map[string]chan struct{}),
	}, nil
}

func defaultDial(ctx context.Context, endpoint string) (*websocket.Conn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	return conn, err
}

func waitBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *Platform) Name() string { return "wecom" }

func (p *Platform) SetLifecycleHandler(handler core.PlatformLifecycleHandler) {
	p.startMu.Lock()
	p.lifecycleHandler = handler
	p.startMu.Unlock()
}

func (p *Platform) Start(handler core.MessageHandler) error {
	if handler == nil {
		return errors.New("wecom: message handler is required")
	}
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if p.started {
		return errors.New("wecom: platform already started")
	}
	p.started = true
	p.handler = handler
	p.ctx, p.cancel = context.WithCancel(context.Background())
	go p.connectLoop()
	return nil
}

func (p *Platform) connectLoop() {
	defer close(p.done)
	backoff := time.Second
	for {
		if p.ctx.Err() != nil {
			return
		}
		err := p.runConnection()
		if p.ctx.Err() != nil {
			return
		}
		if p.lifecycleHandler != nil {
			p.lifecycleHandler.OnPlatformUnavailable(p, err)
		}
		var credentials *credentialError
		if errors.As(err, &credentials) {
			return
		}
		slog.Warn("wecom: WebSocket connection unavailable", "error", err, "retry_in", backoff)
		if !p.backoffWait(p.ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > maxReconnectBackoff {
			backoff = maxReconnectBackoff
		}
	}
}

func (p *Platform) runConnection() error {
	conn, err := p.dial(p.ctx, p.endpoint)
	if err != nil {
		return fmt.Errorf("wecom: dial WebSocket: %w", err)
	}
	p.setConnection(conn)
	defer func() {
		p.clearConnection(conn, errors.New("wecom: WebSocket disconnected"))
		_ = conn.Close()
	}()

	subscribeID := p.nextRequestID("aibot_subscribe")
	if err := p.writeJSON(map[string]any{
		"cmd":     "aibot_subscribe",
		"headers": map[string]string{"req_id": subscribeID},
		"body": map[string]string{
			"bot_id": p.botID,
			"secret": p.secret,
		},
	}); err != nil {
		return fmt.Errorf("wecom: write subscribe: %w", err)
	}
	var response wsFrame
	if err := conn.ReadJSON(&response); err != nil {
		return fmt.Errorf("wecom: read subscribe response: %w", err)
	}
	if response.ErrCode == nil {
		return errors.New("wecom: subscribe response omitted errcode")
	}
	if *response.ErrCode != 0 {
		return &credentialError{err: fmt.Errorf(
			"wecom: subscribe failed: errcode=%d errmsg=%s", *response.ErrCode, response.ErrMsg,
		)}
	}

	if p.lifecycleHandler != nil {
		p.lifecycleHandler.OnPlatformReady(p)
	}
	heartbeatCtx, cancelHeartbeat := context.WithCancel(p.ctx)
	defer cancelHeartbeat()
	go p.heartbeat(heartbeatCtx, conn)

	for {
		var frame wsFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return fmt.Errorf("wecom: read WebSocket: %w", err)
		}
		p.handleFrame(frame)
	}
}

func (p *Platform) heartbeat(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(p.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.writeJSONTo(conn, map[string]any{
				"cmd":     "ping",
				"headers": map[string]string{"req_id": p.nextRequestID("ping")},
			}); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func (p *Platform) handleFrame(frame wsFrame) {
	if frame.Cmd == "aibot_msg_callback" {
		p.handleMsgCallback(frame)
		return
	}
	if frame.Cmd != "" {
		return
	}
	reqID := frame.Headers.ReqID
	if strings.HasPrefix(reqID, "ping_") || strings.HasPrefix(reqID, "aibot_subscribe_") {
		return
	}
	var err error
	if frame.ErrCode == nil {
		err = errors.New("wecom: ACK omitted errcode")
	} else if *frame.ErrCode != 0 {
		err = fmt.Errorf("wecom: ACK failed: errcode=%d errmsg=%s", *frame.ErrCode, frame.ErrMsg)
	}
	p.dispatchAck(reqID, ackResult{frame: frame, err: err})
}

func (p *Platform) handleMsgCallback(frame wsFrame) {
	var body wsMsgCallbackBody
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		slog.Warn("wecom: ignoring malformed callback", "error", err)
		return
	}
	if p.dedup.IsDuplicate(body.MsgID) {
		return
	}
	if body.CreateTime > 0 {
		createdAt := time.Unix(body.CreateTime, 0)
		if body.CreateTime > 1_000_000_000_000 {
			createdAt = time.UnixMilli(body.CreateTime)
		}
		if core.IsOldMessage(createdAt) {
			return
		}
	}
	if !core.AllowList(p.allowFrom, body.From.UserID) {
		return
	}

	sessionKey, channelKey, chatName, targetID, chatType, ok := inboundRoute(body)
	if !ok {
		return
	}
	var content string
	fromVoice := false
	switch body.MsgType {
	case "text":
		content = body.Text.Content
	case "voice":
		content = body.Voice.Content
		if strings.TrimSpace(content) == "" {
			content = body.Voice.Text
		}
		fromVoice = true
	case "image", "file", "mixed":
		slog.Info("wecom: media message is not supported yet", "msg_type", body.MsgType)
		return
	default:
		slog.Info("wecom: unsupported message type", "msg_type", body.MsgType)
		return
	}
	content = stripWeComAtMentions(content, p.botID, body.AibotID)
	if content == "" {
		return
	}

	admission := make(chan struct{})
	message := &core.Message{
		SessionKey: sessionKey,
		Platform:   "wecom",
		MessageID:  body.MsgID,
		ChannelID:  targetID,
		ChannelKey: channelKey,
		UserID:     body.From.UserID,
		UserName:   body.From.UserID,
		ChatName:   chatName,
		Content:    content,
		FromVoice:  fromVoice,
		ReplyCtx: replyContext{
			reqID:    frame.Headers.ReqID,
			chatID:   targetID,
			targetID: targetID,
			chatType: chatType,
			userID:   body.From.UserID,
		},
		DispatchAdmission: admission,
	}
	p.admitInbound(sessionKey, message)
}

func inboundRoute(body wsMsgCallbackBody) (sessionKey, channelKey, chatName, targetID string, chatType int, ok bool) {
	switch body.ChatType {
	case "group":
		chatID := strings.TrimSpace(body.ChatID)
		if chatID == "" {
			return "", "", "", "", 0, false
		}
		return "wecom:g:" + chatID, "g:" + chatID,
			"企业微信群-" + shortID(chatID), chatID, chatTypeGroup, true
	case "single":
		userID := strings.TrimSpace(body.From.UserID)
		if userID == "" {
			return "", "", "", "", 0, false
		}
		return "wecom:u:" + userID, "u:" + userID,
			"企业微信用户-" + shortID(userID), userID, chatTypeSingle, true
	default:
		return "", "", "", "", 0, false
	}
}

func shortID(id string) string {
	runes := []rune(id)
	if len(runes) <= 6 {
		return id
	}
	return string(runes[len(runes)-6:])
}

func (p *Platform) admitInbound(sessionKey string, message *core.Message) {
	p.inboundMu.Lock()
	previous := p.inboundAdmissions[sessionKey]
	p.inboundAdmissions[sessionKey] = message.DispatchAdmission
	p.inboundMu.Unlock()

	ctx := p.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		if previous != nil {
			select {
			case <-previous:
			case <-ctx.Done():
				return
			}
		}
		p.handler(p, message)
	}()
	go func() {
		select {
		case <-message.DispatchAdmission:
		case <-ctx.Done():
		}
		p.inboundMu.Lock()
		if p.inboundAdmissions[sessionKey] == message.DispatchAdmission {
			delete(p.inboundAdmissions, sessionKey)
		}
		p.inboundMu.Unlock()
	}()
}

func (p *Platform) Reply(_ context.Context, value any, content string) error {
	rc, ok := value.(replyContext)
	if !ok {
		return fmt.Errorf("wecom: invalid reply context %T", value)
	}
	if content == "" {
		return nil
	}
	if strings.TrimSpace(rc.reqID) == "" {
		return errors.New("wecom: callback req_id is required for Reply")
	}
	return p.writeJSON(map[string]any{
		"cmd":     "aibot_respond_msg",
		"headers": map[string]string{"req_id": rc.reqID},
		"body": map[string]any{
			"msgtype": "stream",
			"stream": map[string]any{
				"id":      p.nextRequestID("stream"),
				"finish":  true,
				"content": content,
			},
		},
	})
}

func (p *Platform) Send(ctx context.Context, value any, content string) error {
	rc, ok := value.(replyContext)
	if !ok {
		return fmt.Errorf("wecom: invalid reply context %T", value)
	}
	if content == "" {
		return nil
	}
	targetID := strings.TrimSpace(rc.targetID)
	if targetID == "" {
		targetID = strings.TrimSpace(rc.chatID)
	}
	if targetID == "" || (rc.chatType != chatTypeSingle && rc.chatType != chatTypeGroup) {
		return errors.New("wecom: valid target and chat type are required for Send")
	}
	for _, chunk := range splitByBytes(content, p.maxMessageBytes) {
		reqID := p.nextRequestID("aibot_send_msg")
		frame := map[string]any{
			"cmd":     "aibot_send_msg",
			"headers": map[string]string{"req_id": reqID},
			"body": map[string]any{
				"chatid":    targetID,
				"chat_type": rc.chatType,
				"msgtype":   "markdown",
				"markdown":  map[string]string{"content": chunk},
			},
		}
		if err := p.writeAndWaitAck(ctx, frame, reqID); err != nil {
			return err
		}
	}
	return nil
}

func (p *Platform) writeAndWaitAck(ctx context.Context, frame any, reqID string) error {
	resultChannel := make(chan ackResult, 1)
	p.pendingMu.Lock()
	p.pendingAcks[reqID] = resultChannel
	p.pendingMu.Unlock()

	if err := p.writeJSON(frame); err != nil {
		p.deletePending(reqID)
		return err
	}
	timer := time.NewTimer(p.ackTimeout)
	defer timer.Stop()
	select {
	case result := <-resultChannel:
		return result.err
	case <-ctx.Done():
		p.deletePending(reqID)
		return ctx.Err()
	case <-timer.C:
		p.deletePending(reqID)
		return fmt.Errorf("%w waiting for %s", errAckTimeout, reqID)
	}
}

func (p *Platform) dispatchAck(reqID string, result ackResult) {
	p.pendingMu.Lock()
	channel, ok := p.pendingAcks[reqID]
	if ok {
		delete(p.pendingAcks, reqID)
	}
	p.pendingMu.Unlock()
	if ok {
		select {
		case channel <- result:
		default:
		}
	}
}

func (p *Platform) deletePending(reqID string) {
	p.pendingMu.Lock()
	delete(p.pendingAcks, reqID)
	p.pendingMu.Unlock()
}

func (p *Platform) failPending(err error) {
	p.pendingMu.Lock()
	pending := p.pendingAcks
	p.pendingAcks = make(map[string]chan ackResult)
	p.pendingMu.Unlock()
	for _, channel := range pending {
		select {
		case channel <- ackResult{err: err}:
		default:
		}
	}
}

func (p *Platform) nextRequestID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, p.reqSeq.Add(1))
}

func (p *Platform) writeJSON(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.conn == nil {
		return errors.New("wecom: WebSocket is not connected")
	}
	return p.conn.WriteJSON(value)
}

func (p *Platform) writeJSONTo(conn *websocket.Conn, value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.conn != conn {
		return errors.New("wecom: WebSocket connection changed")
	}
	return conn.WriteJSON(value)
}

func (p *Platform) setConnection(conn *websocket.Conn) {
	p.writeMu.Lock()
	p.conn = conn
	p.writeMu.Unlock()
}

func (p *Platform) clearConnection(conn *websocket.Conn, err error) {
	p.writeMu.Lock()
	if p.conn == conn {
		p.conn = nil
	}
	p.writeMu.Unlock()
	p.failPending(err)
}

func (p *Platform) Stop() error {
	p.startMu.Lock()
	cancel := p.cancel
	p.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.writeMu.Lock()
	conn := p.conn
	p.writeMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return nil
}

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	parts := strings.Split(sessionKey, ":")
	if len(parts) != 3 || parts[0] != "wecom" || strings.TrimSpace(parts[2]) == "" ||
		parts[2] != strings.TrimSpace(parts[2]) {
		return nil, fmt.Errorf("wecom: invalid session key %q", sessionKey)
	}
	switch parts[1] {
	case "g":
		return replyContext{
			chatID: parts[2], targetID: parts[2], chatType: chatTypeGroup,
		}, nil
	case "u":
		return replyContext{
			chatID: parts[2], targetID: parts[2], chatType: chatTypeSingle, userID: parts[2],
		}, nil
	default:
		return nil, fmt.Errorf("wecom: invalid session key %q", sessionKey)
	}
}

func (p *Platform) ExternalConversationRelayEnabled() bool { return true }

func (p *Platform) FormattingInstructions() string {
	return "请使用简洁 Markdown；避免使用表格，优先使用短段落或列表。"
}

func (p *Platform) ManualNewConversationGuide() string {
	return "请手动新建一个企业微信内部群，在群聊中添加机器人并 @机器人 开始对话。当前群和当前会话不会改变。"
}

var (
	_ core.AsyncRecoverablePlatform           = (*Platform)(nil)
	_ core.ReplyContextReconstructor          = (*Platform)(nil)
	_ core.ExternalConversationRelayTarget    = (*Platform)(nil)
	_ core.FormattingInstructionProvider      = (*Platform)(nil)
	_ core.ManualNewConversationGuideProvider = (*Platform)(nil)
)
