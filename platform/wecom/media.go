package wecom

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

const (
	wecomMediaMaxBytes   = 20 << 20
	wecomUploadChunkSize = 512 << 10
	wecomUploadMaxChunks = 100
	wecomPKCS7BlockSize  = 32

	mediaDownloadFailureNotice = "图片或文件下载失败，请重新发送。"
)

type wsMediaRef struct {
	URL    string
	AESKey string
}

type wsTextBlock struct {
	Content string `json:"content"`
}

type wsMediaBlock struct {
	URL    string `json:"url"`
	AESKey string `json:"aeskey"`
}

type wsMixedItem struct {
	MsgType string        `json:"msgtype"`
	Text    *wsTextBlock  `json:"text,omitempty"`
	Image   *wsMediaBlock `json:"image,omitempty"`
	File    *wsMediaBlock `json:"file,omitempty"`
}

type wsMixedBlock struct {
	MsgItem []wsMixedItem `json:"msg_item"`
}

type wsQuoteBlock struct {
	MsgType string        `json:"msgtype"`
	File    *wsMediaBlock `json:"file,omitempty"`
}

type inboundMediaPart struct {
	kind string
	text string
	ref  wsMediaRef
}

func collectInboundMediaParts(body *wsMsgCallbackBody) []inboundMediaPart {
	if body.Mixed != nil && len(body.Mixed.MsgItem) > 0 {
		parts := make([]inboundMediaPart, 0, len(body.Mixed.MsgItem)+1)
		for _, item := range body.Mixed.MsgItem {
			switch item.MsgType {
			case "text":
				if item.Text != nil {
					parts = append(parts, inboundMediaPart{kind: "text", text: item.Text.Content})
				}
			case "image":
				if item.Image != nil {
					parts = append(parts, inboundMediaPart{
						kind: "image", ref: wsMediaRef{URL: item.Image.URL, AESKey: item.Image.AESKey},
					})
				}
			case "file":
				if item.File != nil {
					parts = append(parts, inboundMediaPart{
						kind: "file", ref: wsMediaRef{URL: item.File.URL, AESKey: item.File.AESKey},
					})
				}
			}
		}
		// Some callbacks carry a non-empty mixed block but keep the actual
		// top-level media reference outside it.
		switch body.MsgType {
		case "image":
			if body.Image != nil {
				parts = append(parts, inboundMediaPart{
					kind: "image", ref: wsMediaRef{URL: body.Image.URL, AESKey: body.Image.AESKey},
				})
			}
		case "file":
			if body.File != nil {
				parts = append(parts, inboundMediaPart{
					kind: "file", ref: wsMediaRef{URL: body.File.URL, AESKey: body.File.AESKey},
				})
			}
		}
		return appendQuotedFilePart(parts, body.Quote)
	}

	var parts []inboundMediaPart
	switch body.MsgType {
	case "text":
		parts = append(parts, inboundMediaPart{kind: "text", text: body.Text.Content})
	case "image":
		if body.Image != nil {
			parts = append(parts, inboundMediaPart{
				kind: "image", ref: wsMediaRef{URL: body.Image.URL, AESKey: body.Image.AESKey},
			})
		}
	case "file":
		if body.File != nil {
			parts = append(parts, inboundMediaPart{
				kind: "file", ref: wsMediaRef{URL: body.File.URL, AESKey: body.File.AESKey},
			})
		}
	}
	return appendQuotedFilePart(parts, body.Quote)
}

func appendQuotedFilePart(parts []inboundMediaPart, quote *wsQuoteBlock) []inboundMediaPart {
	if quote != nil && quote.MsgType == "file" && quote.File != nil {
		parts = append(parts, inboundMediaPart{
			kind: "file", ref: wsMediaRef{URL: quote.File.URL, AESKey: quote.File.AESKey},
		})
	}
	return parts
}

func (p *Platform) populateInboundMedia(
	ctx context.Context,
	body *wsMsgCallbackBody,
	message *core.Message,
	parts []inboundMediaPart,
) {
	p.populateInboundMediaWithinBudget(ctx, body, message, parts, wecomMediaMaxBytes)
}

func (p *Platform) populateInboundMediaWithinBudget(
	ctx context.Context,
	body *wsMsgCallbackBody,
	message *core.Message,
	parts []inboundMediaPart,
	budget int64,
) {
	var texts []string
	remaining := budget
	for _, part := range parts {
		switch part.kind {
		case "text":
			if text := strings.TrimSpace(part.text); text != "" {
				texts = append(texts, text)
			}
		case "image":
			if remaining <= 0 {
				continue
			}
			data, name, used, err := p.downloadAndDecryptWithinLimit(ctx, part.ref, 0, remaining)
			remaining = remainingAfterMediaRead(remaining, used)
			if err != nil {
				logInboundMediaFailure("image", err)
				continue
			}
			if name == "" {
				name = "image.png"
			}
			mimeType := http.DetectContentType(data)
			if !strings.HasPrefix(mimeType, "image/") {
				mimeType = "image/jpeg"
			}
			message.Images = append(message.Images, core.ImageAttachment{
				MimeType: mimeType,
				Data:     data,
				FileName: filepath.Base(name),
			})
		case "file":
			if remaining <= 0 {
				continue
			}
			data, name, used, err := p.downloadAndDecryptWithinLimit(ctx, part.ref, 0, remaining)
			remaining = remainingAfterMediaRead(remaining, used)
			if err != nil {
				logInboundMediaFailure("file", err)
				continue
			}
			if name == "" {
				name = "attachment"
			}
			message.Files = append(message.Files, core.FileAttachment{
				MimeType: http.DetectContentType(data),
				Data:     data,
				FileName: filepath.Base(name),
			})
		}
	}
	message.Content = stripWeComAtMentions(strings.Join(texts, "\n"), p.botID, body.AibotID)
	if message.Content == "" && len(message.Images) == 0 && len(message.Files) == 0 {
		message.Content = mediaDownloadFailureNotice
	}
}

func logInboundMediaFailure(mediaType string, err error) {
	slog.Warn(
		"wecom: inbound media unavailable",
		"media_type", mediaType,
		"category", inboundMediaFailureCategory(err),
	)
}

func inboundMediaFailureCategory(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}

	message := err.Error()
	switch {
	case strings.Contains(message, "download media: HTTP "):
		return "http_status"
	case strings.Contains(message, "media too large"),
		strings.Contains(message, "media exceeds remaining message budget"):
		return "size_limit"
	case strings.Contains(message, "AES"),
		strings.Contains(message, "encrypted media"),
		strings.Contains(message, "PKCS#7"):
		return "decrypt"
	case strings.Contains(message, "read media:"):
		return "read"
	case strings.Contains(message, "parse media URL:"),
		strings.Contains(message, "media URL must use https"),
		strings.Contains(message, "create media request:"),
		strings.Contains(message, "media redirect must use https"),
		strings.Contains(message, "stopped after 10 media redirects"):
		return "request"
	}

	var transportError *url.Error
	if errors.As(err, &transportError) {
		return "transport"
	}
	return "download"
}

func remainingAfterMediaRead(remaining, used int64) int64 {
	if used >= remaining {
		return 0
	}
	if used <= 0 {
		return remaining
	}
	return remaining - used
}

func (p *Platform) downloadAndDecrypt(
	ctx context.Context,
	ref wsMediaRef,
	sizeHint int64,
) ([]byte, string, error) {
	data, name, _, err := p.downloadAndDecryptWithinLimit(
		ctx, ref, sizeHint, wecomMediaMaxBytes,
	)
	return data, name, err
}

func (p *Platform) downloadAndDecryptWithinLimit(
	ctx context.Context,
	ref wsMediaRef,
	sizeHint int64,
	maxBytes int64,
) ([]byte, string, int64, error) {
	if maxBytes <= 0 || maxBytes > wecomMediaMaxBytes {
		maxBytes = wecomMediaMaxBytes
	}
	if sizeHint > wecomMediaMaxBytes {
		return nil, "", 0, fmt.Errorf("wecom: media too large: %d bytes", sizeHint)
	}
	if sizeHint > maxBytes {
		return nil, "", maxBytes,
			fmt.Errorf("wecom: media exceeds remaining message budget: %d bytes", sizeHint)
	}
	parsed, err := url.Parse(ref.URL)
	if err != nil {
		return nil, "", 0, fmt.Errorf("wecom: parse media URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return nil, "", 0, errors.New("wecom: media URL must use https")
	}
	timeout := p.mediaDownloadTimeout
	if timeout <= 0 {
		timeout = defaultMediaTimeout
	}
	downloadCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", 0, fmt.Errorf("wecom: create media request: %w", err)
	}
	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	clientCopy := *client
	existingRedirectCheck := client.CheckRedirect
	clientCopy.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != "https" {
			return errors.New("wecom: media redirect must use https")
		}
		if existingRedirectCheck != nil {
			return existingRedirectCheck(next, via)
		}
		if len(via) >= 10 {
			return errors.New("wecom: stopped after 10 media redirects")
		}
		return nil
	}
	response, err := clientCopy.Do(request)
	if err != nil {
		return nil, "", 0, fmt.Errorf("wecom: download media: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, "", 0, fmt.Errorf("wecom: download media: HTTP %s", response.Status)
	}
	if response.ContentLength > maxBytes {
		return nil, "", maxBytes,
			fmt.Errorf("wecom: media too large: %d bytes", response.ContentLength)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, "", int64(len(raw)), fmt.Errorf("wecom: read media: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, "", maxBytes,
			fmt.Errorf("wecom: media too large: more than %d bytes", maxBytes)
	}
	data, err := decryptWeComMedia(raw, ref.AESKey)
	if err != nil {
		return nil, "", int64(len(raw)), err
	}
	return data, contentDispositionBaseName(response.Header.Get("Content-Disposition")),
		int64(len(raw)), nil
}

func decryptWeComMedia(ciphertext []byte, encodedKey string) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("wecom: empty encrypted media")
	}
	key, err := decodeWeComMediaKey(encodedKey)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("wecom: encrypted media is not block aligned")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("wecom: create AES cipher: %w", err)
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(plain, ciphertext)
	return strictPKCS7Unpad(plain, wecomPKCS7BlockSize)
}

func decodeWeComMediaKey(encoded string) ([]byte, error) {
	compact := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, strings.TrimSpace(encoded))
	compact = strings.ReplaceAll(strings.ReplaceAll(compact, "-", "+"), "_", "/")
	if remainder := len(compact) % 4; remainder != 0 {
		if remainder == 1 {
			return nil, errors.New("wecom: invalid AES key encoding")
		}
		compact += strings.Repeat("=", 4-remainder)
	}
	key, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, fmt.Errorf("wecom: decode AES key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("wecom: AES-256 key length is %d, want 32", len(key))
	}
	return key, nil
}

func strictPKCS7Unpad(data []byte, maxPadding int) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("wecom: invalid PKCS#7 padded data")
	}
	padding := int(data[len(data)-1])
	if padding < 1 || padding > maxPadding || padding > len(data) {
		return nil, errors.New("wecom: invalid PKCS#7 padding")
	}
	for _, value := range data[len(data)-padding:] {
		if int(value) != padding {
			return nil, errors.New("wecom: invalid PKCS#7 padding")
		}
	}
	return data[:len(data)-padding], nil
}

func contentDispositionBaseName(header string) string {
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	name := filepath.Base(strings.TrimSpace(params["filename"]))
	if name == "." {
		return ""
	}
	return name
}

func (p *Platform) SendImage(ctx context.Context, value any, image core.ImageAttachment) error {
	rc, err := outboundMediaContext(value, "image")
	if err != nil {
		return err
	}
	if len(image.Data) == 0 {
		return errors.New("wecom: image data is empty")
	}
	mediaID, err := p.uploadMedia(ctx, "image", outboundImageName(image), image.Data)
	if err != nil {
		return fmt.Errorf("wecom: send image: %w", err)
	}
	return p.sendMedia(ctx, rc, "image", mediaID)
}

func (p *Platform) SendFile(ctx context.Context, value any, file core.FileAttachment) error {
	rc, err := outboundMediaContext(value, "file")
	if err != nil {
		return err
	}
	if len(file.Data) == 0 {
		return errors.New("wecom: file data is empty")
	}
	mediaID, err := p.uploadMedia(ctx, "file", outboundFileName(file), file.Data)
	if err != nil {
		return fmt.Errorf("wecom: send file: %w", err)
	}
	return p.sendMedia(ctx, rc, "file", mediaID)
}

func outboundMediaContext(value any, kind string) (replyContext, error) {
	rc, ok := value.(replyContext)
	if !ok {
		return replyContext{}, fmt.Errorf("wecom: Send%s: invalid reply context %T",
			strings.ToUpper(kind[:1])+kind[1:], value)
	}
	if strings.TrimSpace(rc.targetID) == "" {
		rc.targetID = strings.TrimSpace(rc.chatID)
	}
	if rc.targetID == "" || (rc.chatType != chatTypeSingle && rc.chatType != chatTypeGroup) {
		return replyContext{}, fmt.Errorf("wecom: Send%s: valid target and chat type are required",
			strings.ToUpper(kind[:1])+kind[1:])
	}
	return rc, nil
}

func (p *Platform) uploadMedia(
	ctx context.Context,
	mediaType string,
	filename string,
	data []byte,
) (string, error) {
	totalChunks := (len(data) + wecomUploadChunkSize - 1) / wecomUploadChunkSize
	if totalChunks == 0 {
		return "", errors.New("empty media data")
	}
	if totalChunks > wecomUploadMaxChunks {
		return "", fmt.Errorf("media requires %d chunks; maximum is %d", totalChunks, wecomUploadMaxChunks)
	}
	checksum := md5.Sum(data)
	initFrame, err := p.writeAndWaitMediaFrame(ctx, "aibot_upload_media_init", map[string]any{
		"type":         mediaType,
		"filename":     filename,
		"total_size":   len(data),
		"total_chunks": totalChunks,
		"md5":          hex.EncodeToString(checksum[:]),
	})
	if err != nil {
		return "", fmt.Errorf("upload init: %w", err)
	}
	var initialized struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(initFrame.Body, &initialized); err != nil {
		return "", fmt.Errorf("decode upload init response: %w", err)
	}
	if initialized.UploadID == "" {
		return "", errors.New("upload init: empty upload_id")
	}

	for index := 0; index < totalChunks; index++ {
		start := index * wecomUploadChunkSize
		end := min(start+wecomUploadChunkSize, len(data))
		if _, err := p.writeAndWaitMediaFrame(ctx, "aibot_upload_media_chunk", map[string]any{
			"upload_id":   initialized.UploadID,
			"chunk_index": index,
			"base64_data": base64.StdEncoding.EncodeToString(data[start:end]),
		}); err != nil {
			return "", fmt.Errorf("upload chunk %d: %w", index, err)
		}
	}

	finishedFrame, err := p.writeAndWaitMediaFrame(ctx, "aibot_upload_media_finish", map[string]any{
		"upload_id": initialized.UploadID,
	})
	if err != nil {
		return "", fmt.Errorf("upload finish: %w", err)
	}
	var finished struct {
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(finishedFrame.Body, &finished); err != nil {
		return "", fmt.Errorf("decode upload finish response: %w", err)
	}
	if finished.MediaID == "" {
		return "", errors.New("upload finish: empty media_id")
	}
	return finished.MediaID, nil
}

func (p *Platform) writeAndWaitMediaFrame(
	ctx context.Context,
	command string,
	body map[string]any,
) (wsFrame, error) {
	requestID := p.nextRequestID(command)
	resultChannel := make(chan ackResult, 1)
	p.pendingMu.Lock()
	p.pendingAcks[requestID] = resultChannel
	p.pendingMu.Unlock()
	if err := p.writeJSON(map[string]any{
		"cmd": command, "headers": map[string]string{"req_id": requestID}, "body": body,
	}); err != nil {
		p.deletePending(requestID)
		return wsFrame{}, err
	}
	timer := time.NewTimer(p.ackTimeout)
	defer timer.Stop()
	select {
	case result := <-resultChannel:
		return result.frame, result.err
	case <-ctx.Done():
		p.deletePending(requestID)
		return wsFrame{}, ctx.Err()
	case <-timer.C:
		p.deletePending(requestID)
		return wsFrame{}, fmt.Errorf("%w waiting for %s", errAckTimeout, requestID)
	}
}

func (p *Platform) sendMedia(
	ctx context.Context,
	rc replyContext,
	mediaType string,
	mediaID string,
) error {
	requestID := p.nextRequestID("aibot_send_msg")
	return p.writeAndWaitAck(ctx, map[string]any{
		"cmd":     "aibot_send_msg",
		"headers": map[string]string{"req_id": requestID},
		"body": map[string]any{
			"chatid":    rc.targetID,
			"chat_type": rc.chatType,
			"msgtype":   mediaType,
			mediaType:   map[string]string{"media_id": mediaID},
		},
	}, requestID)
}

func outboundImageName(image core.ImageAttachment) string {
	name := filepath.Base(strings.TrimSpace(image.FileName))
	if name != "" && name != "." {
		return name
	}
	switch strings.ToLower(image.MimeType) {
	case "image/jpeg", "image/jpg":
		return "image.jpg"
	case "image/gif":
		return "image.gif"
	case "image/webp":
		return "image.webp"
	default:
		return "image.png"
	}
}

func outboundFileName(file core.FileAttachment) string {
	name := filepath.Base(strings.TrimSpace(file.FileName))
	if name != "" && name != "." {
		return name
	}
	switch strings.ToLower(file.MimeType) {
	case "application/pdf":
		return "file.pdf"
	case "text/plain":
		return "file.txt"
	case "text/markdown":
		return "file.md"
	case "application/json":
		return "file.json"
	case "application/zip":
		return "file.zip"
	default:
		return "file.bin"
	}
}

var (
	_ core.ImageSender = (*Platform)(nil)
	_ core.FileSender  = (*Platform)(nil)
)
