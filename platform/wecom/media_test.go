package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
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

func TestMediaDownloadDecryptsAES256CBCAndSanitizesName(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	plain := []byte("enterprise-wechat-media")
	ciphertext := encryptMediaForTest(t, plain, key)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../report.pdf"`)
		_, _ = w.Write(ciphertext)
	}))
	defer server.Close()

	p := &Platform{httpClient: server.Client()}
	got, name, err := p.downloadAndDecrypt(context.Background(), wsMediaRef{
		URL: server.URL, AESKey: base64.StdEncoding.EncodeToString(key),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("data = %q, want %q", got, plain)
	}
	if name != "report.pdf" {
		t.Fatalf("name = %q", name)
	}
}

func TestMediaRejectsNonHTTPSAndPayloadAboveLimit(t *testing.T) {
	p := &Platform{httpClient: http.DefaultClient}
	if _, _, err := p.downloadAndDecrypt(context.Background(), wsMediaRef{
		URL: "http://example.invalid/file", AESKey: validAESKeyForTest(),
	}, 0); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("non-HTTPS err = %v", err)
	}
	if _, _, err := p.downloadAndDecrypt(context.Background(), wsMediaRef{
		URL: "https://example.invalid/oversize", AESKey: validAESKeyForTest(),
	}, wecomMediaMaxBytes+1); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversize err = %v", err)
	}
}

func TestMediaRejectsRedirectFromHTTPSToHTTP(t *testing.T) {
	var insecureHits int
	insecure := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		insecureHits++
	}))
	defer insecure.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, insecure.URL, http.StatusFound)
	}))
	defer secure.Close()

	p := &Platform{httpClient: secure.Client()}
	if _, _, err := p.downloadAndDecrypt(context.Background(), wsMediaRef{
		URL: secure.URL, AESKey: validAESKeyForTest(),
	}, 0); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("redirect err = %v", err)
	}
	if insecureHits != 0 {
		t.Fatalf("insecure redirect target received %d requests", insecureHits)
	}
}

func TestMediaRejectsChunkedPayloadAboveLimit(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unavailable", http.StatusInternalServerError)
			return
		}
		chunk := bytes.Repeat([]byte{'x'}, 1<<20)
		for range 20 {
			_, _ = w.Write(chunk)
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "x")
	}))
	defer server.Close()

	p := &Platform{httpClient: server.Client()}
	if _, _, err := p.downloadAndDecrypt(context.Background(), wsMediaRef{
		URL: server.URL, AESKey: validAESKeyForTest(),
	}, 0); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("chunked oversize err = %v", err)
	}
}

func TestMediaRejectsInvalidPKCS7Padding(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	invalidPlain := bytes.Repeat([]byte{'x'}, aes.BlockSize)
	invalidPlain[len(invalidPlain)-1] = 2
	ciphertext := make([]byte, len(invalidPlain))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(ciphertext, invalidPlain)
	if _, err := decryptWeComMedia(ciphertext, base64.StdEncoding.EncodeToString(key)); err == nil ||
		!strings.Contains(err.Error(), "padding") {
		t.Fatalf("err = %v", err)
	}
}

func TestPureMediaTimeoutProducesNoticeAndReleasesInboundBarrier(t *testing.T) {
	requestStarted := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-requestStarted:
		default:
			close(requestStarted)
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	p := newInboundTestPlatform()
	p.httpClient = server.Client()
	p.mediaDownloadTimeout = 20 * time.Millisecond
	got := make(chan *core.Message, 2)
	p.handler = func(_ core.Platform, msg *core.Message) {
		got <- msg
		close(msg.DispatchAdmission)
	}
	first := callbackBody("media-timeout", "group", "group-1", "u1", "image", "")
	first.Image = &wsMediaBlock{URL: server.URL, AESKey: validAESKeyForTest()}
	p.handleMsgCallback(callbackFrame(t, "req-media", first))
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("media request did not start")
	}
	p.handleMsgCallback(callbackFrame(t, "req-text", callbackBody(
		"after-timeout", "group", "group-1", "u2", "text", "after",
	)))

	select {
	case msg := <-got:
		if msg.MessageID != "media-timeout" || msg.Content != mediaDownloadFailureNotice {
			t.Fatalf("first message = %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("pure media timeout did not produce a notice")
	}
	select {
	case msg := <-got:
		if msg.MessageID != "after-timeout" || msg.Content != "after" {
			t.Fatalf("second message = %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("media timeout did not release same-session barrier")
	}
}

func TestInboundMediaCumulativeBudgetSkipsLaterAttachmentAndKeepsText(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	encrypted := encryptMediaForTest(t, bytes.Repeat([]byte{'x'}, 17), key)
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Disposition", `attachment; filename="image.png"`)
		_, _ = w.Write(encrypted)
	}))
	defer server.Close()

	p := &Platform{httpClient: server.Client()}
	message := &core.Message{}
	keyText := base64.StdEncoding.EncodeToString(key)
	body := wsMsgCallbackBody{AibotID: "bot"}
	p.populateInboundMediaWithinBudget(context.Background(), &body, message, []inboundMediaPart{
		{kind: "text", text: "keep this"},
		{kind: "image", ref: wsMediaRef{URL: server.URL + "/first", AESKey: keyText}},
		{kind: "file", ref: wsMediaRef{URL: server.URL + "/second", AESKey: keyText}},
	}, int64(len(encrypted)))
	if message.Content != "keep this" {
		t.Fatalf("Content = %q", message.Content)
	}
	if len(message.Images) != 1 || len(message.Files) != 0 {
		t.Fatalf("Images=%d Files=%d", len(message.Images), len(message.Files))
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("download requests = %d, want 1", got)
	}
}

func TestInboundMixedMediaPreservesFetchOrderAndTextWhenDownloadFails(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var mu sync.Mutex
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/missing" {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="../../photo.png"`)
		_, _ = w.Write(encryptMediaForTest(t, []byte("\x89PNG\r\n\x1a\npayload"), key))
	}))
	defer server.Close()

	p := newInboundTestPlatform()
	p.httpClient = server.Client()
	got := make(chan *core.Message, 1)
	p.handler = func(_ core.Platform, msg *core.Message) {
		got <- msg
		close(msg.DispatchAdmission)
	}
	keyText := base64.StdEncoding.EncodeToString(key)
	body := wsMsgCallbackBody{
		MsgID: "mixed-1", AibotID: "bot", ChatID: "group-1", ChatType: "group",
		MsgType: "mixed", CreateTime: time.Now().Unix(),
		Mixed: &wsMixedBlock{MsgItem: []wsMixedItem{
			{MsgType: "text", Text: &wsTextBlock{Content: "@bot first"}},
			{MsgType: "image", Image: &wsMediaBlock{URL: server.URL + "/image", AESKey: keyText}},
			{MsgType: "text", Text: &wsTextBlock{Content: "second"}},
			{MsgType: "file", File: &wsMediaBlock{URL: server.URL + "/missing", AESKey: keyText}},
		}},
	}
	body.From.UserID = "user-1"
	p.handleMsgCallback(callbackFrame(t, "req-1", body))

	select {
	case msg := <-got:
		if msg.Content != "first\nsecond" {
			t.Fatalf("Content = %q", msg.Content)
		}
		if len(msg.Images) != 1 || msg.Images[0].FileName != "photo.png" {
			t.Fatalf("Images = %#v", msg.Images)
		}
		if len(msg.Files) != 0 {
			t.Fatalf("Files = %#v", msg.Files)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for mixed inbound message")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(paths, ",") != "/image,/missing" {
		t.Fatalf("download order = %v", paths)
	}
}

func TestOutboundFileNameUsesBaseName(t *testing.T) {
	got := outboundFileName(core.FileAttachment{FileName: "../../report.pdf"})
	if got != "report.pdf" {
		t.Fatalf("name = %q", got)
	}
}

func TestOutboundMediaUploadChunksThenSendsStrictlyAcknowledgedMessage(t *testing.T) {
	var mu sync.Mutex
	var commands []string
	var initName string
	var chunkSizes []int
	p, stop := connectedPlatform(t, func(conn *websocket.Conn) {
		for {
			var frame wsFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			mu.Lock()
			commands = append(commands, frame.Cmd)
			mu.Unlock()
			response := wsFrame{Headers: frame.Headers, ErrCode: intPtr(0)}
			switch frame.Cmd {
			case "aibot_upload_media_init":
				var body struct {
					Filename string `json:"filename"`
				}
				if err := json.Unmarshal(frame.Body, &body); err != nil {
					t.Errorf("decode init: %v", err)
					return
				}
				mu.Lock()
				initName = body.Filename
				mu.Unlock()
				response.Body = json.RawMessage(`{"upload_id":"upload-1"}`)
			case "aibot_upload_media_chunk":
				var body struct {
					Base64Data string `json:"base64_data"`
				}
				if err := json.Unmarshal(frame.Body, &body); err != nil {
					t.Errorf("decode chunk: %v", err)
					return
				}
				decoded, err := base64.StdEncoding.DecodeString(body.Base64Data)
				if err != nil {
					t.Errorf("decode chunk data: %v", err)
					return
				}
				mu.Lock()
				chunkSizes = append(chunkSizes, len(decoded))
				mu.Unlock()
			case "aibot_upload_media_finish":
				response.Body = json.RawMessage(`{"media_id":"media-1"}`)
			case "aibot_send_msg":
				if err := conn.WriteJSON(response); err != nil {
					t.Errorf("write send ACK: %v", err)
				}
				return
			default:
				t.Errorf("unexpected command %q", frame.Cmd)
				return
			}
			if err := conn.WriteJSON(response); err != nil {
				t.Errorf("write ACK: %v", err)
				return
			}
		}
	})
	defer stop()

	data := bytes.Repeat([]byte{'x'}, wecomUploadChunkSize+1)
	err := p.SendFile(context.Background(), replyContext{
		targetID: "group-1", chatID: "group-1", chatType: chatTypeGroup,
	}, core.FileAttachment{FileName: "../../report.pdf", MimeType: "application/pdf", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	wantCommands := "aibot_upload_media_init,aibot_upload_media_chunk,aibot_upload_media_chunk,aibot_upload_media_finish,aibot_send_msg"
	if strings.Join(commands, ",") != wantCommands {
		t.Fatalf("commands = %v", commands)
	}
	if initName != "report.pdf" {
		t.Fatalf("init filename = %q", initName)
	}
	if len(chunkSizes) != 2 || chunkSizes[0] != wecomUploadChunkSize || chunkSizes[1] != 1 {
		t.Fatalf("chunk sizes = %v", chunkSizes)
	}
}

func TestOutboundMediaStrictACKFailureIsReturned(t *testing.T) {
	p, stop := connectedPlatform(t, func(conn *websocket.Conn) {
		for {
			var frame wsFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			response := wsFrame{Headers: frame.Headers, ErrCode: intPtr(0)}
			switch frame.Cmd {
			case "aibot_upload_media_init":
				response.Body = json.RawMessage(`{"upload_id":"upload-1"}`)
			case "aibot_upload_media_finish":
				response.Body = json.RawMessage(`{"media_id":"media-1"}`)
			case "aibot_send_msg":
				response.ErrCode = intPtr(6000)
				response.ErrMsg = "rejected"
			}
			if err := conn.WriteJSON(response); err != nil {
				return
			}
			if frame.Cmd == "aibot_send_msg" {
				return
			}
		}
	})
	defer stop()

	err := p.SendImage(context.Background(), replyContext{
		targetID: "user-1", chatID: "user-1", chatType: chatTypeSingle,
	}, core.ImageAttachment{FileName: "image.png", MimeType: "image/png", Data: []byte("image")})
	if err == nil || !strings.Contains(err.Error(), "6000") {
		t.Fatalf("err = %v", err)
	}
}

func TestOutboundMediaRejectsMoreThanOneHundredChunks(t *testing.T) {
	p := mustPlatform(t)
	err := p.SendFile(context.Background(), replyContext{
		targetID: "group-1", chatID: "group-1", chatType: chatTypeGroup,
	}, core.FileAttachment{
		FileName: "huge.bin",
		Data:     make([]byte, wecomUploadChunkSize*wecomUploadMaxChunks+1),
	})
	if err == nil || !strings.Contains(err.Error(), "100") {
		t.Fatalf("err = %v", err)
	}
}

func encryptMediaForTest(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, padded)
	return out
}

func validAESKeyForTest() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}
