package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestNewValidatesWebSocketConfiguration(t *testing.T) {
	tests := []struct {
		name string
		opts map[string]any
	}{
		{"unsupported mode", map[string]any{"mode": "webhook", "bot_id": "bot", "bot_secret": "secret"}},
		{"missing bot id", map[string]any{"bot_secret": "secret"}},
		{"missing secret", map[string]any{"bot_id": "bot"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}

	for _, mode := range []string{"", "websocket"} {
		p, err := New(map[string]any{
			"mode": mode, "bot_id": " bot ", "bot_secret": " secret ", "allow_from": "u1",
		})
		if err != nil {
			t.Fatalf("New(mode=%q): %v", mode, err)
		}
		if p.Name() != "wecom" {
			t.Fatalf("Name() = %q", p.Name())
		}
	}
}

func TestStripMentionAndSplitByBytes(t *testing.T) {
	if got := stripWeComAtMentions(" 允许 ＠BOT-1 @bot-1 ", "bot-1"); got != "允许" {
		t.Fatalf("stripWeComAtMentions() = %q", got)
	}
	if got := stripWeComAtMentions("@bot 第一行\n\n第二行", "bot"); got != "第一行\n\n第二行" {
		t.Fatalf("stripWeComAtMentions() destroyed Markdown layout: %q", got)
	}
	input := strings.Repeat("你", 5)
	parts := splitByBytes(input, 7)
	if strings.Join(parts, "") != input {
		t.Fatalf("split did not reassemble: %#v", parts)
	}
	for _, part := range parts {
		if len(part) > 7 {
			t.Fatalf("chunk length = %d, want <= 7", len(part))
		}
	}
}

func TestInboundStableKeysNamesAndReplyContext(t *testing.T) {
	p := newInboundTestPlatform()
	got := make(chan *core.Message, 3)
	p.handler = func(_ core.Platform, msg *core.Message) {
		got <- msg
		close(msg.DispatchAdmission)
	}

	p.handleMsgCallback(callbackFrame(t, "req-g1", callbackBody(
		"m-g1", "group", "group-12345678", "user-a", "text", "@bot hello",
	)))
	p.handleMsgCallback(callbackFrame(t, "req-g2", callbackBody(
		"m-g2", "group", "group-12345678", "user-b", "text", "second",
	)))
	p.handleMsgCallback(callbackFrame(t, "req-u", callbackBody(
		"m-u", "single", "", "user-ABCDEFGH", "voice", "transcribed",
	)))

	var messages []*core.Message
	for range 3 {
		select {
		case msg := <-got:
			messages = append(messages, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for inbound message")
		}
	}

	byID := make(map[string]*core.Message)
	for _, msg := range messages {
		byID[msg.MessageID] = msg
	}
	if byID["m-g1"].SessionKey != "wecom:g:group-12345678" ||
		byID["m-g2"].SessionKey != "wecom:g:group-12345678" {
		t.Fatalf("group keys differ: %q / %q", byID["m-g1"].SessionKey, byID["m-g2"].SessionKey)
	}
	if got := byID["m-g1"].ChannelKey; got != "g:group-12345678" {
		t.Fatalf("group ChannelKey = %q", got)
	}
	if got := byID["m-g1"].ChatName; got != "企业微信群-345678" {
		t.Fatalf("group ChatName = %q", got)
	}
	if got := byID["m-g1"].Content; got != "hello" {
		t.Fatalf("group Content = %q", got)
	}
	if got := byID["m-u"].SessionKey; got != "wecom:u:user-ABCDEFGH" {
		t.Fatalf("single SessionKey = %q", got)
	}
	if got := byID["m-u"].ChannelKey; got != "u:user-ABCDEFGH" {
		t.Fatalf("single ChannelKey = %q", got)
	}
	if got := byID["m-u"].ChatName; got != "企业微信用户-CDEFGH" {
		t.Fatalf("single ChatName = %q", got)
	}
	if !byID["m-u"].FromVoice {
		t.Fatal("voice transcription did not set FromVoice")
	}
	rc := byID["m-g1"].ReplyCtx.(replyContext)
	if rc.reqID != "req-g1" || rc.chatID != "group-12345678" ||
		rc.targetID != "group-12345678" || rc.chatType != chatTypeGroup || rc.userID != "user-a" {
		t.Fatalf("group ReplyCtx = %+v", rc)
	}
}

func TestInboundRejectsMissingStableKeyPartsAndUnsupportedMedia(t *testing.T) {
	p := newInboundTestPlatform()
	var calls atomic.Int32
	p.handler = func(core.Platform, *core.Message) { calls.Add(1) }

	p.handleMsgCallback(callbackFrame(t, "r1", callbackBody("m1", "group", "", "u1", "text", "x")))
	p.handleMsgCallback(callbackFrame(t, "r2", callbackBody("m2", "single", "", "", "text", "x")))
	p.handleMsgCallback(callbackFrame(t, "r3", callbackBody("m3", "group", "g1", "u1", "image", "")))
	time.Sleep(30 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
}

func TestReconstructReplyContextStrictRoundTrip(t *testing.T) {
	p := &Platform{}
	tests := []struct {
		key      string
		chatType int
		target   string
	}{
		{"wecom:g:group-1", chatTypeGroup, "group-1"},
		{"wecom:u:user-1", chatTypeSingle, "user-1"},
	}
	for _, tt := range tests {
		got, err := p.ReconstructReplyCtx(tt.key)
		if err != nil {
			t.Fatalf("ReconstructReplyCtx(%q): %v", tt.key, err)
		}
		rc := got.(replyContext)
		if rc.chatType != tt.chatType || rc.targetID != tt.target || rc.chatID != tt.target {
			t.Fatalf("ReconstructReplyCtx(%q) = %+v", tt.key, rc)
		}
	}
	for _, key := range []string{
		"", "wecom:g:", "wecom:u:", "wecom:x:id", "wecom:g:id:extra", "other:g:id", "wecom:g: id",
	} {
		if _, err := p.ReconstructReplyCtx(key); err == nil {
			t.Fatalf("ReconstructReplyCtx(%q) error = nil", key)
		}
	}
}

func TestPlatformCapabilitiesAndGuide(t *testing.T) {
	p := &Platform{}
	if !p.ExternalConversationRelayEnabled() {
		t.Fatal("ExternalConversationRelayEnabled() = false")
	}
	if got := p.FormattingInstructions(); !strings.Contains(got, "Markdown") || !strings.Contains(got, "表格") {
		t.Fatalf("FormattingInstructions() = %q", got)
	}
	guide := p.ManualNewConversationGuide()
	for _, want := range []string{"手动", "内部群", "添加机器人", "@机器人", "当前群", "当前会话", "不"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("ManualNewConversationGuide() = %q, missing %q", guide, want)
		}
	}
}

func TestInboundFiltersDuplicateOldUnauthorizedAndEmptyText(t *testing.T) {
	p := newInboundTestPlatform()
	p.allowFrom = "allowed"
	got := make(chan *core.Message, 2)
	p.handler = func(_ core.Platform, msg *core.Message) {
		got <- msg
		close(msg.DispatchAdmission)
	}

	valid := callbackBody("same", "single", "", "allowed", "text", "hello")
	p.handleMsgCallback(callbackFrame(t, "r1", valid))
	p.handleMsgCallback(callbackFrame(t, "r2", valid))
	old := callbackBody("old", "single", "", "allowed", "text", "old")
	old.CreateTime = core.StartTime.Add(-time.Minute).Unix()
	p.handleMsgCallback(callbackFrame(t, "r3", old))
	p.handleMsgCallback(callbackFrame(t, "r4", callbackBody("blocked", "single", "", "blocked", "text", "no")))
	p.handleMsgCallback(callbackFrame(t, "r5", callbackBody("empty", "single", "", "allowed", "text", "@bot")))

	select {
	case msg := <-got:
		if msg.MessageID != "same" {
			t.Fatalf("MessageID = %q", msg.MessageID)
		}
	case <-time.After(time.Second):
		t.Fatal("valid message not delivered")
	}
	select {
	case msg := <-got:
		t.Fatalf("unexpected message delivered: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func newInboundTestPlatform() *Platform {
	ctx, cancel := context.WithCancel(context.Background())
	return &Platform{
		botID:             "bot",
		allowFrom:         "*",
		ctx:               ctx,
		cancel:            cancel,
		inboundAdmissions: make(map[string]chan struct{}),
	}
}

func callbackBody(msgID, chatType string, chatID, userID, msgType, content string) wsMsgCallbackBody {
	body := wsMsgCallbackBody{
		MsgID: msgID, ChatType: chatType, ChatID: chatID, MsgType: msgType,
		CreateTime: time.Now().Unix(),
	}
	body.From.UserID = userID
	body.Text.Content = content
	body.Voice.Content = content
	return body
}

func callbackFrame(t *testing.T, reqID string, body wsMsgCallbackBody) wsFrame {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return wsFrame{Cmd: "aibot_msg_callback", Headers: wsFrameHeaders{ReqID: reqID}, Body: raw}
}

type lifecycleRecorder struct {
	ready       chan struct{}
	unavailable chan error
	readyOnce   sync.Once
}

func newLifecycleRecorder() *lifecycleRecorder {
	return &lifecycleRecorder{ready: make(chan struct{}), unavailable: make(chan error, 8)}
}

func (l *lifecycleRecorder) OnPlatformReady(core.Platform) {
	l.readyOnce.Do(func() { close(l.ready) })
}

func (l *lifecycleRecorder) OnPlatformUnavailable(_ core.Platform, err error) {
	l.unavailable <- err
}

func TestSubscribeLifecycleHeartbeatAndStopIdempotent(t *testing.T) {
	pingSeen := make(chan struct{}, 1)
	server := newWSServer(t, func(conn *websocket.Conn, attempt int) {
		expectSubscribeAndAck(t, conn, 0)
		for {
			var frame wsFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Cmd == "ping" {
				pingSeen <- struct{}{}
				_ = conn.WriteJSON(wsFrame{Headers: frame.Headers, ErrCode: intPtr(0)})
			}
		}
	})
	defer server.Close()

	p := mustPlatform(t)
	p.endpoint = wsURL(server.URL)
	p.heartbeatInterval = 10 * time.Millisecond
	p.backoffWait = noWaitBackoff
	lifecycle := newLifecycleRecorder()
	p.SetLifecycleHandler(lifecycle)
	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, lifecycle.ready, "ready")
	select {
	case <-pingSeen:
	case <-time.After(time.Second):
		t.Fatal("heartbeat not sent")
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, p.done, "connect loop stop")
}

func TestAuthenticationFailureDoesNotRetry(t *testing.T) {
	server := newWSServer(t, func(conn *websocket.Conn, attempt int) {
		expectSubscribeAndAck(t, conn, 40001)
	})
	defer server.Close()

	p := mustPlatform(t)
	p.endpoint = wsURL(server.URL)
	p.backoffWait = noWaitBackoff
	lifecycle := newLifecycleRecorder()
	p.SetLifecycleHandler(lifecycle)
	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, p.done, "authentication loop stop")
	if got := server.attempts.Load(); got != 1 {
		t.Fatalf("dial attempts = %d, want 1", got)
	}
	select {
	case err := <-lifecycle.unavailable:
		if err == nil || !strings.Contains(err.Error(), "subscribe") {
			t.Fatalf("unavailable error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unavailable not reported")
	}
	select {
	case <-lifecycle.ready:
		t.Fatal("ready reported for failed subscription")
	default:
	}
}

func TestNetworkFailureReconnectsWithExponentialBackoff(t *testing.T) {
	server := newWSServer(t, func(conn *websocket.Conn, attempt int) {
		expectSubscribeAndAck(t, conn, 0)
		_ = conn.Close()
	})
	defer server.Close()

	p := mustPlatform(t)
	p.endpoint = wsURL(server.URL)
	delays := make(chan time.Duration, 4)
	p.backoffWait = func(ctx context.Context, d time.Duration) bool {
		delays <- d
		return ctx.Err() == nil
	}
	p.SetLifecycleHandler(newLifecycleRecorder())
	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	var got []time.Duration
	for len(got) < 3 {
		select {
		case d := <-delays:
			got = append(got, d)
		case <-time.After(time.Second):
			t.Fatalf("backoff sequence = %v", got)
		}
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("backoff sequence = %v, want %v", got, want)
	}
	_ = p.Stop()
}

type wsTestServer struct {
	*httptest.Server
	attempts atomic.Int32
}

func newWSServer(t *testing.T, handle func(*websocket.Conn, int)) *wsTestServer {
	t.Helper()
	result := &wsTestServer{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	result.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		attempt := int(result.attempts.Add(1))
		defer conn.Close()
		handle(conn, attempt)
	}))
	return result
}

func expectSubscribeAndAck(t *testing.T, conn *websocket.Conn, errCode int) {
	t.Helper()
	var frame struct {
		Cmd     string         `json:"cmd"`
		Headers wsFrameHeaders `json:"headers"`
		Body    struct {
			BotID  string `json:"bot_id"`
			Secret string `json:"secret"`
		} `json:"body"`
	}
	if err := conn.ReadJSON(&frame); err != nil {
		t.Errorf("read subscribe: %v", err)
		return
	}
	if frame.Cmd != "aibot_subscribe" || frame.Body.BotID != "bot" || frame.Body.Secret != "secret" {
		t.Errorf("subscribe frame = %+v", frame)
		return
	}
	if err := conn.WriteJSON(wsFrame{
		Headers: frame.Headers, ErrCode: intPtr(errCode), ErrMsg: "test",
	}); err != nil {
		t.Errorf("write subscribe ack: %v", err)
	}
}

func mustPlatform(t *testing.T) *Platform {
	t.Helper()
	p, err := New(map[string]any{"bot_id": "bot", "bot_secret": "secret", "allow_from": "*"})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Platform)
}

func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }
func intPtr(v int) *int           { return &v }
func noWaitBackoff(ctx context.Context, _ time.Duration) bool {
	return ctx.Err() == nil
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestInboundAdmissionOrdersSameGroupAndAllowsDifferentGroups(t *testing.T) {
	p := newInboundTestPlatform()
	started := make(chan string, 4)
	releaseFirst := make(chan struct{})
	p.handler = func(_ core.Platform, msg *core.Message) {
		started <- msg.MessageID
		if msg.MessageID == "g1-first" {
			<-releaseFirst
		}
		close(msg.DispatchAdmission)
	}

	p.handleMsgCallback(callbackFrame(t, "r1", callbackBody("g1-first", "group", "group-1", "u1", "text", "first")))
	if got := waitString(t, started); got != "g1-first" {
		t.Fatalf("first started = %q", got)
	}
	p.handleMsgCallback(callbackFrame(t, "r2", callbackBody("g1-second", "group", "group-1", "u2", "text", "second")))
	p.handleMsgCallback(callbackFrame(t, "r3", callbackBody("g2-first", "group", "group-2", "u3", "text", "parallel")))

	if got := waitString(t, started); got != "g2-first" {
		t.Fatalf("message admitted while group-1 blocked = %q, want g2-first", got)
	}
	select {
	case got := <-started:
		t.Fatalf("same-group second started early: %q", got)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	if got := waitString(t, started); got != "g1-second" {
		t.Fatalf("same-group second = %q", got)
	}
}

func waitString(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for value")
		return ""
	}
}

func TestReplyUsesCallbackRequestAndStreamFinish(t *testing.T) {
	frames := make(chan map[string]any, 1)
	p, stop := connectedPlatform(t, func(conn *websocket.Conn) {
		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		frames <- frame
	})
	defer stop()

	rc := replyContext{reqID: "callback-1", userID: "u1"}
	if err := p.Reply(context.Background(), rc, "reply"); err != nil {
		t.Fatal(err)
	}
	frame := <-frames
	if frame["cmd"] != "aibot_respond_msg" {
		t.Fatalf("cmd = %#v", frame["cmd"])
	}
	headers := frame["headers"].(map[string]any)
	if headers["req_id"] != "callback-1" {
		t.Fatalf("req_id = %#v", headers["req_id"])
	}
	body := frame["body"].(map[string]any)
	stream := body["stream"].(map[string]any)
	if stream["finish"] != true || stream["content"] != "reply" {
		t.Fatalf("stream = %#v", stream)
	}
}

func TestSendChatTypesAndAcknowledgements(t *testing.T) {
	tests := []struct {
		name      string
		rc        replyContext
		errCode   int
		wantType  float64
		wantError bool
	}{
		{"group success", replyContext{chatID: "g1", targetID: "g1", chatType: chatTypeGroup}, 0, 2, false},
		{"single success", replyContext{chatID: "u1", targetID: "u1", chatType: chatTypeSingle}, 0, 1, false},
		{"server error", replyContext{chatID: "g1", targetID: "g1", chatType: chatTypeGroup}, 40001, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frames := make(chan map[string]any, 1)
			p, stop := connectedPlatform(t, func(conn *websocket.Conn) {
				var frame map[string]any
				if err := conn.ReadJSON(&frame); err != nil {
					return
				}
				frames <- frame
				headers := frame["headers"].(map[string]any)
				_ = conn.WriteJSON(map[string]any{
					"headers": map[string]any{"req_id": headers["req_id"]},
					"errcode": tt.errCode,
					"errmsg":  "server result",
				})
			})
			defer stop()
			err := p.Send(context.Background(), tt.rc, "hello")
			if (err != nil) != tt.wantError {
				t.Fatalf("Send() error = %v, wantError=%v", err, tt.wantError)
			}
			frame := <-frames
			body := frame["body"].(map[string]any)
			if body["chat_type"] != tt.wantType {
				t.Fatalf("chat_type = %#v, want %#v", body["chat_type"], tt.wantType)
			}
			if body["msgtype"] != "markdown" {
				t.Fatalf("msgtype = %#v", body["msgtype"])
			}
		})
	}
}

func TestSendTimeoutDisconnectAndContextCancelFail(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		p, stop := connectedPlatform(t, func(conn *websocket.Conn) {
			var frame map[string]any
			_ = conn.ReadJSON(&frame)
			time.Sleep(100 * time.Millisecond)
		})
		defer stop()
		p.ackTimeout = 15 * time.Millisecond
		err := p.Send(context.Background(), replyContext{chatID: "g", targetID: "g", chatType: chatTypeGroup}, "x")
		if err == nil || !errors.Is(err, errAckTimeout) {
			t.Fatalf("Send() error = %v, want ack timeout", err)
		}
	})

	t.Run("disconnect", func(t *testing.T) {
		p, stop := connectedPlatform(t, func(conn *websocket.Conn) {
			var frame map[string]any
			_ = conn.ReadJSON(&frame)
			_ = conn.Close()
		})
		defer stop()
		err := p.Send(context.Background(), replyContext{chatID: "g", targetID: "g", chatType: chatTypeGroup}, "x")
		if err == nil {
			t.Fatal("Send() error = nil after disconnect")
		}
	})

	t.Run("context cancel", func(t *testing.T) {
		received := make(chan struct{})
		p, stop := connectedPlatform(t, func(conn *websocket.Conn) {
			var frame map[string]any
			_ = conn.ReadJSON(&frame)
			close(received)
			time.Sleep(100 * time.Millisecond)
		})
		defer stop()
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-received
			cancel()
		}()
		err := p.Send(ctx, replyContext{chatID: "g", targetID: "g", chatType: chatTypeGroup}, "x")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send() error = %v, want context.Canceled", err)
		}
	})
}

func TestSendSplitsAtByteLimit(t *testing.T) {
	var count atomic.Int32
	p, stop := connectedPlatform(t, func(conn *websocket.Conn) {
		for {
			var frame map[string]any
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			count.Add(1)
			headers := frame["headers"].(map[string]any)
			_ = conn.WriteJSON(map[string]any{
				"headers": map[string]any{"req_id": headers["req_id"]}, "errcode": 0,
			})
		}
	})
	defer stop()
	p.maxMessageBytes = 7
	if err := p.Send(context.Background(), replyContext{
		chatID: "g", targetID: "g", chatType: chatTypeGroup,
	}, strings.Repeat("你", 5)); err != nil {
		t.Fatal(err)
	}
	if got := count.Load(); got != 3 {
		t.Fatalf("send frame count = %d, want 3", got)
	}
}

func connectedPlatform(t *testing.T, afterSubscribe func(*websocket.Conn)) (*Platform, func()) {
	t.Helper()
	server := newWSServer(t, func(conn *websocket.Conn, _ int) {
		expectSubscribeAndAck(t, conn, 0)
		afterSubscribe(conn)
	})
	p := mustPlatform(t)
	p.endpoint = wsURL(server.URL)
	p.backoffWait = func(ctx context.Context, _ time.Duration) bool {
		<-ctx.Done()
		return false
	}
	lifecycle := newLifecycleRecorder()
	p.SetLifecycleHandler(lifecycle)
	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, lifecycle.ready, "ready")
	return p, func() {
		_ = p.Stop()
		server.Close()
	}
}
