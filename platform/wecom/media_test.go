package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

func TestInboundMediaFailureLogsAreRedacted(t *testing.T) {
	const (
		sensitiveHost     = "private-media.example.invalid"
		sensitivePath     = "customer-secret-path"
		sensitiveToken    = "signed-token-secret"
		sensitiveKey      = "aes-key-secret"
		sensitiveBody     = "encrypted-body-secret"
		sensitiveFileName = "confidential-filename.pdf"
		sensitiveID       = "internal-media-id-secret"
	)
	sensitiveURL := "https://" + sensitiveHost + "/" + sensitivePath +
		"?token=" + sensitiveToken + "&media_id=" + sensitiveID
	validKey := validAESKeyForTest()

	tests := []struct {
		name         string
		mediaType    string
		transport    http.RoundTripper
		key          string
		wantCategory string
	}{
		{
			name:      "image transport error",
			mediaType: "image",
			transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed for " + request.URL.String())
			}),
			key:          validKey,
			wantCategory: "transport",
		},
		{
			name:      "file HTTP status",
			mediaType: "file",
			transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Status:     "502 Bad Gateway",
					Body:       io.NopCloser(strings.NewReader(sensitiveBody)),
					Header: http.Header{
						"Content-Disposition": []string{
							`attachment; filename="` + sensitiveFileName + `"`,
						},
					},
					Request: request,
				}, nil
			}),
			key:          validKey,
			wantCategory: "http_status",
		},
		{
			name:      "invalid AES key",
			mediaType: "file",
			transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				return mediaResponseForLogTest(request, []byte(sensitiveBody), sensitiveFileName), nil
			}),
			key:          sensitiveKey,
			wantCategory: "decrypt",
		},
		{
			name:      "invalid padding",
			mediaType: "file",
			transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				return mediaResponseForLogTest(
					request, bytes.Repeat([]byte{0}, aes.BlockSize), sensitiveFileName,
				), nil
			}),
			key:          validKey,
			wantCategory: "decrypt",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			p := &Platform{httpClient: &http.Client{Transport: test.transport}}
			message := &core.Message{}
			p.populateInboundMediaWithinBudget(
				context.Background(),
				&wsMsgCallbackBody{},
				message,
				[]inboundMediaPart{{
					kind: test.mediaType,
					ref:  wsMediaRef{URL: sensitiveURL, AESKey: test.key},
				}},
				wecomMediaMaxBytes,
			)

			output := logs.String()
			if !strings.Contains(output, "category="+test.wantCategory) {
				t.Fatalf("log = %q, want category %q", output, test.wantCategory)
			}
			for _, secret := range []string{
				sensitiveHost,
				sensitivePath,
				sensitiveToken,
				validKey,
				sensitiveKey,
				sensitiveBody,
				sensitiveFileName,
				sensitiveID,
			} {
				if strings.Contains(output, secret) {
					t.Fatalf("log contains sensitive value %q: %s", secret, output)
				}
			}
		})
	}
}

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

func TestDecryptWeComMediaAcceptsOfficial32BytePadding(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	plain := []byte("1234567890123") // 13 bytes produces 19 bytes of official padding.
	ciphertext := encryptMediaWithPaddingBlockForTest(t, plain, key, 32)
	got, err := decryptWeComMedia(ciphertext, base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("data = %q, want %q", got, plain)
	}
}

func TestCollectInboundMediaPartsIncludesQuotedFileOnTextCallback(t *testing.T) {
	raw := json.RawMessage(`{
		"msgtype":"text",
		"text":{"content":"@bot analyze"},
		"quote":{
			"msgtype":"file",
			"file":{"url":"https://example.invalid/file","aeskey":"test-key"}
		}
	}`)
	var body wsMsgCallbackBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	parts := collectInboundMediaParts(&body)
	if len(parts) != 2 {
		t.Fatalf("parts = %#v", parts)
	}
	if parts[0].kind != "text" || parts[0].text != "@bot analyze" {
		t.Fatalf("first part = %#v", parts[0])
	}
	if parts[1].kind != "file" || parts[1].ref.URL != "https://example.invalid/file" ||
		parts[1].ref.AESKey != "test-key" {
		t.Fatalf("second part = %#v", parts[1])
	}
}

func TestQuotedFileTextCallbackDispatchesAttachment(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	plain := []byte("1234567890123")
	ciphertext := encryptMediaWithPaddingBlockForTest(t, plain, key, 32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../report.txt"`)
		_, _ = w.Write(ciphertext)
	}))
	defer server.Close()

	body := map[string]any{
		"msgid":       "quoted-file-1",
		"aibotid":     "bot",
		"chatid":      "group-1",
		"chattype":    "group",
		"from":        map[string]string{"userid": "user-1"},
		"msgtype":     "text",
		"text":        map[string]string{"content": "@bot analyze"},
		"create_time": time.Now().Unix(),
		"quote": map[string]any{
			"msgtype": "file",
			"file": map[string]string{
				"url": server.URL, "aeskey": strings.TrimRight(
					base64.StdEncoding.EncodeToString(key), "=",
				),
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	p := newInboundTestPlatform()
	p.httpClient = server.Client()
	got := make(chan *core.Message, 1)
	p.handler = func(_ core.Platform, message *core.Message) {
		got <- message
		close(message.DispatchAdmission)
	}
	p.handleMsgCallback(wsFrame{
		Cmd: "aibot_msg_callback", Headers: wsFrameHeaders{ReqID: "request-1"}, Body: raw,
	})
	select {
	case message := <-got:
		if message.Content != "analyze" {
			t.Fatalf("Content = %q", message.Content)
		}
		if len(message.Files) != 1 {
			t.Fatalf("Files = %#v", message.Files)
		}
		if message.Files[0].FileName != "report.txt" ||
			!bytes.Equal(message.Files[0].Data, plain) {
			t.Fatalf("File = %#v", message.Files[0])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for quoted file callback")
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

func TestInboundMediaBudgetCountsDecryptFailure(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	invalidCiphertext := bytes.Repeat([]byte{'x'}, 32)
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(invalidCiphertext)
	}))
	defer server.Close()

	p := &Platform{httpClient: server.Client()}
	message := &core.Message{}
	keyText := base64.StdEncoding.EncodeToString(key)
	body := wsMsgCallbackBody{AibotID: "bot"}
	p.populateInboundMediaWithinBudget(context.Background(), &body, message, []inboundMediaPart{
		{kind: "text", text: "keep text"},
		{kind: "image", ref: wsMediaRef{URL: server.URL + "/invalid", AESKey: keyText}},
		{kind: "image", ref: wsMediaRef{URL: server.URL + "/must-not-download", AESKey: keyText}},
	}, int64(len(invalidCiphertext)))
	if message.Content != "keep text" || len(message.Images) != 0 {
		t.Fatalf("message = %#v", message)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("download requests = %d, want 1", got)
	}
}

func TestInboundMediaBudgetStopsAfterKnownOversize(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, 33))
	}))
	defer server.Close()

	p := &Platform{httpClient: server.Client()}
	message := &core.Message{}
	body := wsMsgCallbackBody{AibotID: "bot"}
	p.populateInboundMediaWithinBudget(context.Background(), &body, message, []inboundMediaPart{
		{kind: "text", text: "keep text"},
		{kind: "file", ref: wsMediaRef{URL: server.URL + "/oversize", AESKey: validAESKeyForTest()}},
		{kind: "file", ref: wsMediaRef{URL: server.URL + "/must-not-download", AESKey: validAESKeyForTest()}},
	}, 32)
	if message.Content != "keep text" || len(message.Files) != 0 {
		t.Fatalf("message = %#v", message)
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

func encryptMediaWithPaddingBlockForTest(
	t *testing.T,
	plain []byte,
	key []byte,
	paddingBlock int,
) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := paddingBlock - len(plain)%paddingBlock
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, padded)
	return out
}

func validAESKeyForTest() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func mediaResponseForLogTest(
	request *http.Request,
	body []byte,
	filename string,
) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header: http.Header{
			"Content-Disposition": []string{`attachment; filename="` + filename + `"`},
		},
		Request: request,
	}
}
