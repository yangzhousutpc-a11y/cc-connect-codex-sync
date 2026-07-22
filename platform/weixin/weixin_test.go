package weixin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestPlatformSupportsInPlaceConversationFork(t *testing.T) {
	platform := &Platform{}
	if !platform.SupportsInPlaceConversationFork() {
		t.Fatal("SupportsInPlaceConversationFork() = false, want true")
	}
	if _, ok := any(platform).(core.ConversationSpawner); ok {
		t.Fatal("Weixin platform must not implement ConversationSpawner")
	}
}

func TestPlatformEnablesCodexDesktopConversationRelay(t *testing.T) {
	platform := &Platform{}
	target, ok := any(platform).(core.ExternalConversationRelayTarget)
	if !ok {
		t.Fatal("Weixin platform must implement ExternalConversationRelayTarget")
	}
	if !target.ExternalConversationRelayEnabled() {
		t.Fatal("ExternalConversationRelayEnabled() = false, want true")
	}
}

func TestBodyFromItemList_Text(t *testing.T) {
	got := bodyFromItemList([]messageItem{
		{Type: messageItemText, TextItem: &textItem{Text: "  hello  "}},
	})
	if got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestDispatchInboundNamesCodexWeixinConversation(t *testing.T) {
	p := &Platform{}
	var got *core.Message

	p.dispatchInbound(context.Background(), &weixinMessage{
		MessageID:  1,
		FromUserID: "peer@im.wechat",
		ItemList: []messageItem{{
			Type:     messageItemText,
			TextItem: &textItem{Text: "hi"},
		}},
	}, func(_ core.Platform, msg *core.Message) {
		got = msg
	})

	if got == nil {
		t.Fatal("expected inbound message")
	}
	if got.ChatName != "[Codex] 微信" {
		t.Fatalf("ChatName = %q, want %q", got.ChatName, "[Codex] 微信")
	}
	if key := p.ReplySessionKey(got.ReplyCtx); key != sessionKeyPrefix+"peer@im.wechat" {
		t.Fatalf("ReplySessionKey() = %q, want %q", key, sessionKeyPrefix+"peer@im.wechat")
	}
}

func TestFormatSessionReply(t *testing.T) {
	p := &Platform{}
	tests := []struct {
		name        string
		sessionName string
		content     string
		want        string
	}{
		{name: "codex display name", sessionName: "[Codex] 销售方案", content: "已完成", want: "[销售方案] 已完成"},
		{name: "source-aware display name", sessionName: "[微信-Codex] 销售方案", content: "已完成", want: "[销售方案] 已完成"},
		{name: "decorated source-aware display name", sessionName: "💬 [微信-Codex] 销售方案", content: "已完成", want: "[销售方案] 已完成"},
		{name: "empty name", content: "已完成", want: "[微信] 已完成"},
		{name: "same prefix", sessionName: "[Codex] 销售方案", content: "[销售方案] 已完成", want: "[销售方案] 已完成"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.FormatSessionReply(tt.sessionName, tt.content); got != tt.want {
				t.Fatalf("FormatSessionReply(%q, %q) = %q, want %q", tt.sessionName, tt.content, got, tt.want)
			}
		})
	}
}

func TestFormatSessionReplySanitizesLabel(t *testing.T) {
	p := &Platform{}
	tests := []struct {
		name        string
		sessionName string
		wantPrefix  string
	}{
		{name: "closing bracket", sessionName: "A] B", wantPrefix: "[A B] "},
		{name: "newline", sessionName: "A\nB\r\nC", wantPrefix: "[A B C] "},
		{name: "unicode", sessionName: "销售🚀", wantPrefix: "[销售🚀] "},
		{name: "long", sessionName: strings.Repeat("长", 80), wantPrefix: "[" + strings.Repeat("长", 64) + "] "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.FormatSessionReply(tt.sessionName, "done"); got != tt.wantPrefix+"done" {
				t.Fatalf("FormatSessionReply() = %q, want %q", got, tt.wantPrefix+"done")
			}
		})
	}
}

func TestReplySessionKey(t *testing.T) {
	p := &Platform{}
	rc := &replyContext{sessionKey: sessionKeyPrefix + "user-aaa"}
	if got := p.ReplySessionKey(rc); got != sessionKeyPrefix+"user-aaa" {
		t.Fatalf("ReplySessionKey() = %q, want %q", got, sessionKeyPrefix+"user-aaa")
	}
}

func TestSendChunksPrefixesIncompleteDeliveryNotice(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sendMessageReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		text := ""
		if len(req.Msg.ItemList) > 0 && req.Msg.ItemList[0].TextItem != nil {
			text = req.Msg.ItemList[0].TextItem.Text
		}
		mu.Lock()
		sent = append(sent, text)
		call := len(sent)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"ret":-1,"errcode":500,"errmsg":"failed"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	defer server.Close()

	p := &Platform{httpClient: server.Client()}
	p.api = newAPIClient(server.URL+"/", "token", "", p.httpClient)
	err := p.sendChunks(context.Background(), &replyContext{peerUserID: "user-1", contextToken: "token"}, "[A] response")
	if err == nil {
		t.Fatal("sendChunks() error = nil, want first send failure")
	}
	mu.Lock()
	got := append([]string(nil), sent...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"[A] response", "[A] ⚠️ 消息发送不完整，请在终端查看完整结果。"}) {
		t.Fatalf("sent texts = %#v", got)
	}
}

func TestSendChunksRepeatsSessionPrefixWithinRuneLimit(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sendMessageReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(req.Msg.ItemList) != 1 || req.Msg.ItemList[0].TextItem == nil {
			t.Errorf("item_list = %#v, want one text item", req.Msg.ItemList)
		} else {
			mu.Lock()
			sent = append(sent, req.Msg.ItemList[0].TextItem.Text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	defer server.Close()

	p := &Platform{httpClient: server.Client()}
	p.api = newAPIClient(server.URL+"/", "token", "", p.httpClient)
	prefix := "[A B 🚀] "
	body := strings.Repeat("甲", maxWeixinChunk*2) + "🚀终"
	content := p.FormatSessionReply("A] B\n🚀", body)
	if err := p.sendChunks(context.Background(), &replyContext{peerUserID: "user-1", contextToken: "token"}, content); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]string(nil), sent...)
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("sent chunks = %d, want multiple chunks", len(got))
	}
	var rebuilt strings.Builder
	for i, chunk := range got {
		if !strings.HasPrefix(chunk, prefix) {
			t.Errorf("chunk %d = %q..., want prefix %q", i+1, truncatePreview(chunk, 32), prefix)
			continue
		}
		if runes := utf8.RuneCountInString(chunk); runes > maxWeixinChunk {
			t.Errorf("chunk %d rune count = %d, want <= %d", i+1, runes, maxWeixinChunk)
		}
		rebuilt.WriteString(strings.TrimPrefix(chunk, prefix))
	}
	if rebuilt.String() != body {
		t.Fatalf("rebuilt body rune count = %d, want %d", utf8.RuneCountInString(rebuilt.String()), utf8.RuneCountInString(body))
	}
}

func TestSplitSessionReplyChunksLeavesUnprefixedContentUnchanged(t *testing.T) {
	content := "甲乙丙丁戊"
	prefix, got := splitSessionReplyChunks(content, 2)
	if prefix != "" {
		t.Fatalf("prefix = %q, want empty", prefix)
	}
	if want := splitUTF8(content, 2); !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks = %#v, want original split %#v", got, want)
	}
}

func TestBodyFromItemList_VoiceText(t *testing.T) {
	got := bodyFromItemList([]messageItem{
		{Type: messageItemVoice, VoiceItem: &voiceItem{Text: "transcribed"}},
	})
	if got != "transcribed" {
		t.Fatalf("got %q", got)
	}
}

func TestBodyFromItemList_Quote(t *testing.T) {
	ref := &refMessage{
		Title: "t",
		MessageItem: &messageItem{
			Type:     messageItemText,
			TextItem: &textItem{Text: "inner"},
		},
	}
	got := bodyFromItemList([]messageItem{
		{Type: messageItemText, TextItem: &textItem{Text: "reply"}, RefMsg: ref},
	})
	want := "[引用: t | inner]\nreply"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSplitUTF8(t *testing.T) {
	s := string([]rune{'a', '啊', 'b', '吧', 'c'})
	parts := splitUTF8(s, 2)
	if len(parts) != 3 || parts[0] != "a啊" || parts[1] != "b吧" || parts[2] != "c" {
		t.Fatalf("parts=%#v", parts)
	}
}

func TestSplitUTF8Empty(t *testing.T) {
	parts := splitUTF8("", maxWeixinChunk)
	if len(parts) != 1 || parts[0] != "" {
		t.Fatalf("parts=%#v", parts)
	}
}

func TestMediaOnlyItems(t *testing.T) {
	if !mediaOnlyItems([]messageItem{{Type: messageItemImage}}) {
		t.Fatal("image should be media-only")
	}
	if mediaOnlyItems([]messageItem{{Type: messageItemVoice, VoiceItem: &voiceItem{Text: "x"}}}) {
		t.Fatal("voice with text is not media-only")
	}
}

func TestCollectInboundMediaUsesCDNHTTPClient(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download" {
			t.Fatalf("path = %q, want /download", r.URL.Path)
		}
		if r.URL.Query().Get("encrypted_query_param") != "image-ref" {
			t.Fatalf("encrypted_query_param = %q, want image-ref", r.URL.Query().Get("encrypted_query_param"))
		}
		_, _ = w.Write(png)
	}))
	defer server.Close()

	p := &Platform{
		cdnBaseURL: server.URL,
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("api client should not download media")
		})},
		cdnHttpClient: server.Client(),
	}

	images, files, audio := p.collectInboundMedia(context.Background(), []messageItem{{
		Type: messageItemImage,
		ImageItem: &imageItem{
			Media: &cdnMedia{EncryptQueryParam: "image-ref"},
		},
	}})

	if len(images) != 1 {
		t.Fatalf("images len = %d, want 1", len(images))
	}
	if images[0].MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", images[0].MimeType)
	}
	if string(images[0].Data) != string(png) {
		t.Fatalf("image data = %v, want %v", images[0].Data, png)
	}
	if len(files) != 0 {
		t.Fatalf("files len = %d, want 0", len(files))
	}
	if audio != nil {
		t.Fatalf("audio = %#v, want nil", audio)
	}
}

func TestSendMessageResp_JSON(t *testing.T) {
	var r sendMessageResp
	if err := json.Unmarshal([]byte(`{"ret":-1,"errcode":100,"errmsg":"rate limited"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Ret != -1 || r.Errcode != 100 || r.Errmsg != "rate limited" {
		t.Fatalf("got %+v", r)
	}
}

func TestSendAudioRejectsEmptyAudio(t *testing.T) {
	p := &Platform{}
	// resolveReplyContext checks context_token first, so provide one
	rc := &replyContext{peerUserID: "test", contextToken: "valid-token"}
	err := p.SendAudio(context.Background(), rc, []byte{}, "wav")
	if err == nil {
		t.Fatal("expected error for empty audio")
	}
	if !containsStr(err.Error(), "empty audio") {
		t.Fatalf("expected 'empty audio' error, got: %v", err)
	}
}

func TestSendAudioRejectsInvalidReplyContext(t *testing.T) {
	p := &Platform{}
	err := p.SendAudio(context.Background(), "invalid-context", []byte("audio-data"), "wav")
	if err == nil {
		t.Fatal("expected error for invalid reply context")
	}
	if !containsStr(err.Error(), "invalid reply context") {
		t.Fatalf("expected 'invalid reply context' error, got: %v", err)
	}
}

func TestSendAudioRejectsNilReplyContext(t *testing.T) {
	p := &Platform{}
	err := p.SendAudio(context.Background(), nil, []byte("audio-data"), "wav")
	if err == nil {
		t.Fatal("expected error for nil reply context")
	}
	if !containsStr(err.Error(), "invalid reply context") {
		t.Fatalf("expected 'invalid reply context' error, got: %v", err)
	}
}

func TestGetConfig_RejectsNonZeroErrcode(t *testing.T) {
	raw := `{"ret":0,"errcode":40001,"errmsg":"invalid token","typing_ticket":""}`
	var out getConfigResp
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if out.Errcode != 40001 {
		t.Fatalf("expected errcode 40001, got %d", out.Errcode)
	}
}

func TestGetConfig_RejectsNonZeroRet(t *testing.T) {
	raw := `{"ret":-1,"errcode":0,"errmsg":"internal error","typing_ticket":"tk"}`
	var out getConfigResp
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if out.Ret != -1 {
		t.Fatalf("expected ret -1, got %d", out.Ret)
	}
}

func TestGetConfigReq_JSONFieldName(t *testing.T) {
	req := getConfigReq{
		UserID:   "test_user",
		BaseInfo: baseInfo{ChannelVersion: "1.0"},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ilink_user_id"`) {
		t.Fatalf("expected ilink_user_id in JSON, got: %s", string(data))
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStrHelper(s, substr))
}

func containsStrHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// testLifecycleHandler captures lifecycle callbacks from a platform so tests
// can assert that OnPlatformReady is invoked at the right moment.
type testLifecycleHandler struct {
	mu          sync.Mutex
	readyCount  int32
	readyCh     chan struct{}
	unavailable []error
}

func newTestLifecycleHandler() *testLifecycleHandler {
	return &testLifecycleHandler{readyCh: make(chan struct{}, 1)}
}

func (h *testLifecycleHandler) OnPlatformReady(p core.Platform) {
	if atomic.AddInt32(&h.readyCount, 1) == 1 {
		h.readyCh <- struct{}{}
	}
}

func (h *testLifecycleHandler) OnPlatformUnavailable(p core.Platform, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unavailable = append(h.unavailable, err)
}

func (h *testLifecycleHandler) ReadyCount() int {
	return int(atomic.LoadInt32(&h.readyCount))
}

// newILinkTestServer returns an httptest.Server that responds to ilink
// long-poll getUpdates calls with the provided body and status. Tests can
// inspect callCount to confirm pollLoop actually issued requests.
type ilinkTestServer struct {
	server    *httptest.Server
	callCount atomic.Int32
	body      string
	status    int
}

func newILinkTestServer(status int, body string) *ilinkTestServer {
	s := &ilinkTestServer{body: body, status: status}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return s
}

func (s *ilinkTestServer) Close() { s.server.Close() }
func (s *ilinkTestServer) URL() string {
	return s.server.URL
}

func TestPollLoop_NotifiesReadyForPollAfterFirstSuccessfulGetUpdates(t *testing.T) {
	body := `{"ret":0,"errcode":0,"msgs":[],"get_updates_buf":"buf-1"}`
	srv := newILinkTestServer(http.StatusOK, body)
	defer srv.Close()

	p := &Platform{
		token:         "tok",
		baseURL:       srv.URL(),
		longPollMS:    100,
		accountLabel:  "default",
		httpClient:    &http.Client{},
		dedup:         make(map[string]time.Time),
		typingTickets: make(map[string]typingTicketEntry),
	}
	p.api = newAPIClient(srv.URL(), "tok", "", p.httpClient)

	handler := newTestLifecycleHandler()
	p.SetLifecycleHandler(handler)

	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	select {
	case <-handler.readyCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("OnPlatformReady not observed within timeout (readyCount=%d, getUpdatesCalls=%d)",
			handler.ReadyCount(), srv.callCount.Load())
	}

	// Give pollLoop enough time to issue at least one more getUpdates; the
	// ready signal must remain a one-shot event.
	time.Sleep(400 * time.Millisecond)

	if got := handler.ReadyCount(); got != 1 {
		t.Fatalf("ready callbacks = %d, want exactly 1 (one-shot)", got)
	}
	if got := srv.callCount.Load(); got < 2 {
		t.Fatalf("getUpdates calls = %d, want >= 2 (pollLoop should keep polling)", got)
	}
}

func TestPollLoopPreservesServerOrderPerPeerWithoutBlockingOtherPeers(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	cdnEntered := make(chan struct{}, 1)
	cdnRelease := make(chan struct{})
	cdnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cdnEntered <- struct{}{}
		<-cdnRelease
		_, _ = w.Write([]byte("image"))
	}))
	defer cdnServer.Close()

	updates := `{"ret":0,"errcode":0,"msgs":[` +
		`{"message_id":1,"from_user_id":"peer-a","message_type":1,"item_list":[{"type":2,"image_item":{"media":{"encrypt_query_param":"first"}}}]},` +
		`{"message_id":2,"from_user_id":"peer-a","message_type":1,"item_list":[{"type":1,"text_item":{"text":"second-a"}}]},` +
		`{"message_id":3,"from_user_id":"peer-b","message_type":1,"item_list":[{"type":1,"text_item":{"text":"peer-b"}}]}` +
		`],"get_updates_buf":"buf-1"}`
	updatesServer := newILinkTestServer(http.StatusOK, updates)
	defer updatesServer.Close()

	p := &Platform{
		token:         "tok",
		baseURL:       updatesServer.URL(),
		cdnBaseURL:    cdnServer.URL,
		longPollMS:    100,
		accountLabel:  "default",
		httpClient:    &http.Client{},
		cdnHttpClient: cdnServer.Client(),
		dedup:         make(map[string]time.Time),
		typingTickets: make(map[string]typingTicketEntry),
	}
	p.api = newAPIClient(updatesServer.URL(), "tok", "", p.httpClient)

	var orderMu sync.Mutex
	var order []string
	firstSeen := make(chan struct{}, 1)
	secondSeen := make(chan struct{}, 1)
	otherPeerSeen := make(chan struct{}, 1)
	if err := p.Start(func(_ core.Platform, msg *core.Message) {
		if msg.DispatchAdmission != nil {
			close(msg.DispatchAdmission)
		}
		orderMu.Lock()
		order = append(order, msg.MessageID)
		orderMu.Unlock()
		switch msg.MessageID {
		case "1":
			firstSeen <- struct{}{}
		case "2":
			secondSeen <- struct{}{}
		case "3":
			otherPeerSeen <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	select {
	case <-cdnEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("first peer-a message did not enter blocked media preparation")
	}
	select {
	case <-otherPeerSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("peer-b was blocked by peer-a admission")
	}
	select {
	case <-secondSeen:
		close(cdnRelease)
		<-firstSeen
		t.Fatal("second peer-a message overtook the first server-ordered message")
	case <-time.After(200 * time.Millisecond):
	}
	close(cdnRelease)
	select {
	case <-firstSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("first peer-a message was not delivered after media preparation")
	}
	select {
	case <-secondSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("second peer-a message was not delivered")
	}

	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	firstIndex := slices.Index(gotOrder, "1")
	secondIndex := slices.Index(gotOrder, "2")
	if firstIndex < 0 || secondIndex < 0 || firstIndex > secondIndex {
		t.Fatalf("peer-a delivery order = %#v, want message 1 before 2", gotOrder)
	}
}

func TestPollLoopAdmitsSamePeerAcrossBatchesWithoutWaitingForHandlerCompletion(t *testing.T) {
	var calls atomic.Int32
	updatesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0,"msgs":[{"message_id":11,"from_user_id":"peer-a","message_type":1,"item_list":[{"type":1,"text_item":{"text":"first"}}]}],"get_updates_buf":"buf-1"}`))
		case 2:
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0,"msgs":[{"message_id":12,"from_user_id":"peer-a","message_type":1,"item_list":[{"type":1,"text_item":{"text":"second"}}]}],"get_updates_buf":"buf-2"}`))
		default:
			<-r.Context().Done()
		}
	}))
	defer updatesServer.Close()

	p := &Platform{
		token:         "tok",
		baseURL:       updatesServer.URL,
		longPollMS:    100,
		accountLabel:  "default",
		httpClient:    updatesServer.Client(),
		dedup:         make(map[string]time.Time),
		typingTickets: make(map[string]typingTicketEntry),
	}
	p.api = newAPIClient(updatesServer.URL, "tok", "", p.httpClient)

	firstEntered := make(chan struct{}, 1)
	firstRelease := make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	if err := p.Start(func(_ core.Platform, msg *core.Message) {
		if msg.DispatchAdmission != nil {
			close(msg.DispatchAdmission)
		}
		switch msg.MessageID {
		case "11":
			firstEntered <- struct{}{}
			<-firstRelease
		case "12":
			secondEntered <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	select {
	case <-firstEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("first batch did not enter handler")
	}
	select {
	case <-secondEntered:
		close(firstRelease)
	case <-time.After(500 * time.Millisecond):
		close(firstRelease)
		t.Fatal("second batch waited for the first handler to complete instead of core admission")
	}
}

func TestDispatchInboundOrderedHighConcurrencyKeepsEachPeerSequence(t *testing.T) {
	const (
		peerCount    = 8
		messageCount = 64
	)
	p := &Platform{dedup: make(map[string]time.Time), typingTickets: make(map[string]typingTicketEntry)}
	var mu sync.Mutex
	got := make(map[string][]string, peerCount)
	handler := func(_ core.Platform, msg *core.Message) {
		mu.Lock()
		got[msg.UserID] = append(got[msg.UserID], msg.MessageID)
		mu.Unlock()
		close(msg.DispatchAdmission)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var admissions []<-chan struct{}
	for i := 0; i < messageCount; i++ {
		for peerIndex := 0; peerIndex < peerCount; peerIndex++ {
			peer := "peer-" + strconv.Itoa(peerIndex)
			messageID := int64(peerIndex*1000 + i + 1)
			admissions = append(admissions, p.dispatchInboundOrdered(ctx, &weixinMessage{
				MessageID:   messageID,
				FromUserID:  peer,
				MessageType: messageTypeUser,
				ItemList: []messageItem{{
					Type:     messageItemText,
					TextItem: &textItem{Text: strconv.Itoa(i)},
				}},
			}, handler))
		}
	}
	for _, admitted := range admissions {
		select {
		case <-admitted:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for ordered admissions")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for peerIndex := 0; peerIndex < peerCount; peerIndex++ {
		peer := "peer-" + strconv.Itoa(peerIndex)
		if len(got[peer]) != messageCount {
			t.Fatalf("%s messages = %d, want %d", peer, len(got[peer]), messageCount)
		}
		for i, id := range got[peer] {
			want := strconv.FormatInt(int64(peerIndex*1000+i+1), 10)
			if id != want {
				t.Fatalf("%s message %d = %q, want %q", peer, i, id, want)
			}
		}
	}
}

func TestPollLoop_DoesNotNotifyReadyForPollWhileGetUpdatesFails(t *testing.T) {
	srv := newILinkTestServer(http.StatusInternalServerError, `{"ret":-1}`)
	defer srv.Close()

	p := &Platform{
		token:         "tok",
		baseURL:       srv.URL(),
		longPollMS:    100,
		accountLabel:  "default",
		httpClient:    &http.Client{},
		dedup:         make(map[string]time.Time),
		typingTickets: make(map[string]typingTicketEntry),
	}
	p.api = newAPIClient(srv.URL(), "tok", "", p.httpClient)

	handler := newTestLifecycleHandler()
	p.SetLifecycleHandler(handler)

	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// While every getUpdates returns 500, the backoff grows (1s, 2s, 4s, …).
	// Within 2.5s we expect at least one more failed attempt but never a
	// ready-for-poll signal.
	time.Sleep(2500 * time.Millisecond)

	if got := handler.ReadyCount(); got != 0 {
		t.Fatalf("ready callbacks = %d, want 0 while getUpdates fails", got)
	}
	if got := srv.callCount.Load(); got < 1 {
		t.Fatalf("getUpdates calls = %d, want >= 1 (pollLoop should be retrying)", got)
	}
}

func TestPlatform_ImplementsAsyncRecoverablePlatform(t *testing.T) {
	var p core.Platform = &Platform{}
	if _, ok := p.(core.AsyncRecoverablePlatform); !ok {
		t.Fatal("weixin Platform must implement core.AsyncRecoverablePlatform so the engine waits for ready-for-poll")
	}
}

// TestContextToken_PersistAndReload verifies that context_token values written
// via setContextToken survive a process restart by being persisted to
// context_tokens.json and reloaded into the in-memory map on the next startup.
// This is the regression coverage for #1087.
func TestContextToken_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	tokensPath := filepath.Join(dir, "context_tokens.json")

	// 1. First "process": store two context_tokens, then verify the file exists
	//    with the expected JSON content.
	p1 := &Platform{
		tokens:     make(map[string]string),
		tokensPath: tokensPath,
	}
	p1.setContextToken("user-aaa", "token-A")
	p1.setContextToken("user-bbb", "token-B")

	if _, err := os.Stat(tokensPath); err != nil {
		t.Fatalf("expected context_tokens.json at %s, got: %v", tokensPath, err)
	}

	// Confirm the on-disk format is a JSON object keyed by peer user ID.
	raw, err := os.ReadFile(tokensPath)
	if err != nil {
		t.Fatalf("read tokens file: %v", err)
	}
	var onDisk map[string]string
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parse tokens file: %v (raw=%q)", err, string(raw))
	}
	if onDisk["user-aaa"] != "token-A" || onDisk["user-bbb"] != "token-B" {
		t.Fatalf("on-disk tokens = %v, want user-aaa=token-A user-bbb=token-B", onDisk)
	}

	// 2. Second "process": same stateDir, fresh in-memory map. loadTokens()
	//    must read the file and populate the map.
	p2 := &Platform{
		tokens:     make(map[string]string),
		tokensPath: tokensPath,
	}
	p2.loadTokens()

	if got := p2.getContextToken("user-aaa"); got != "token-A" {
		t.Errorf("after reload, user-aaa = %q, want %q", got, "token-A")
	}
	if got := p2.getContextToken("user-bbb"); got != "token-B" {
		t.Errorf("after reload, user-bbb = %q, want %q", got, "token-B")
	}

	// 3. ReconstructReplyCtx (the cron / cc-connect send path) must succeed
	//    using the reloaded token.
	rc, err := p2.ReconstructReplyCtx(sessionKeyPrefix + "user-aaa")
	if err != nil {
		t.Fatalf("ReconstructReplyCtx after reload: %v", err)
	}
	concrete, ok := rc.(*replyContext)
	if !ok {
		t.Fatalf("ReconstructReplyCtx returned %T, want *replyContext", rc)
	}
	if concrete.contextToken != "token-A" {
		t.Errorf("reloaded contextToken = %q, want %q", concrete.contextToken, "token-A")
	}
	if concrete.sessionKey != sessionKeyPrefix+"user-aaa" {
		t.Errorf("reconstructed sessionKey = %q, want %q", concrete.sessionKey, sessionKeyPrefix+"user-aaa")
	}
}

// TestContextToken_LoadMissingFile is a no-op fallback: if the persistence
// file does not exist (first run, or after a cleanup), loadTokens must not
// error and the in-memory map must remain empty.
func TestContextToken_LoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	p := &Platform{
		tokens:     make(map[string]string),
		tokensPath: filepath.Join(dir, "does-not-exist.json"),
	}
	// Should not panic, should not return an error.
	p.loadTokens()
	if got := p.getContextToken("anyone"); got != "" {
		t.Errorf("getContextToken on fresh state = %q, want empty", got)
	}
}

// TestReconstructReplyCtx_MissingToken verifies the cron / cc-connect send
// path returns the expected actionable error when no context_token has ever
// been stored for a peer. This is the "user must message the bot first"
// case that the original #1087 reporter hit.
func TestReconstructReplyCtx_MissingToken(t *testing.T) {
	p := &Platform{
		tokens: make(map[string]string),
	}
	_, err := p.ReconstructReplyCtx(sessionKeyPrefix + "never-messaged-user")
	if err == nil {
		t.Fatal("expected error for missing context_token, got nil")
	}
	if !containsStr(err.Error(), "no stored context_token") {
		t.Errorf("error = %q, want it to mention 'no stored context_token'", err.Error())
	}
}
