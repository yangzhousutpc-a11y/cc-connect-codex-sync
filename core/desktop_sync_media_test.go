package core

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

type desktopMediaPlatform struct {
	desktopSyncPlatform
	muMedia       sync.Mutex
	sequence      []string
	failImageAt   int
	failFileAt    int
	imageAttempts int
	fileAttempts  int
}

func (p *desktopMediaPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	if err := p.desktopSyncPlatform.Send(ctx, replyCtx, content); err != nil {
		return err
	}
	p.muMedia.Lock()
	p.sequence = append(p.sequence, "text:"+content)
	p.muMedia.Unlock()
	return nil
}

func (p *desktopMediaPlatform) SendImage(_ context.Context, _ any, image ImageAttachment) error {
	p.muMedia.Lock()
	defer p.muMedia.Unlock()
	p.imageAttempts++
	p.sequence = append(p.sequence, "image:"+image.FileName)
	if p.failImageAt == p.imageAttempts {
		return errors.New("image failure")
	}
	return nil
}

func (p *desktopMediaPlatform) SendFile(_ context.Context, _ any, file FileAttachment) error {
	p.muMedia.Lock()
	defer p.muMedia.Unlock()
	p.fileAttempts++
	p.sequence = append(p.sequence, "file:"+file.FileName)
	if p.failFileAt == p.fileAttempts {
		return errors.New("file failure")
	}
	return nil
}

type desktopTextOnlyPlatform struct{ desktopSyncPlatform }

type desktopMediaRouteSwitchPlatform struct {
	desktopMediaPlatform
	sessions   *SessionManager
	sessionKey string
}

func (p *desktopMediaRouteSwitchPlatform) SendImage(ctx context.Context, replyCtx any, image ImageAttachment) error {
	if err := p.desktopMediaPlatform.SendImage(ctx, replyCtx, image); err != nil {
		return err
	}
	p.sessions.NewSession(p.sessionKey, "new route").SetAgentSessionID("thread-replaced", "codex")
	return nil
}

func TestDesktopLiveSyncSendsTextImagesAndFilesInOrder(t *testing.T) {
	event := ExternalConversationEvent{
		SessionID: "thread-media-order",
		Role:      "user",
		Content:   "body",
		Images: []ImageAttachment{
			{FileName: "one.png"}, {FileName: "two.png"},
		},
		Files: []FileAttachment{
			{FileName: "one.txt"}, {FileName: "two.txt"},
		},
	}
	agent, platform, engine := newDesktopMediaEngine(t, event)
	engine.pollDesktopLiveSync(context.Background(), agent)

	want := []string{
		"text:✣ Codex App · 你\nbody",
		"image:one.png", "image:two.png",
		"file:one.txt", "file:two.txt",
	}
	if got := desktopMediaSequence(platform); !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncRelaysMediaOnlyEvent(t *testing.T) {
	event := ExternalConversationEvent{
		SessionID: "thread-media-only",
		Role:      "user",
		Images:    []ImageAttachment{{FileName: "only.png"}},
	}
	agent, platform, engine := newDesktopMediaEngine(t, event)
	engine.pollDesktopLiveSync(context.Background(), agent)
	if got, want := desktopMediaSequence(platform), []string{"image:only.png"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncRetriesOnlyUnacknowledgedMediaItems(t *testing.T) {
	event := ExternalConversationEvent{
		SessionID: "thread-media-retry",
		Role:      "user",
		Content:   "body",
		Images: []ImageAttachment{
			{FileName: "one.png"}, {FileName: "two.png"}, {FileName: "three.png"},
		},
		Files: []FileAttachment{{FileName: "one.txt"}},
	}
	agent, platform, engine := newDesktopMediaEngine(t, event)
	platform.failImageAt = 2
	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.pollDesktopLiveSync(context.Background(), agent)

	want := []string{
		"text:✣ Codex App · 你\nbody",
		"image:one.png", "image:two.png",
		"image:two.png", "image:three.png",
		"file:one.txt",
	}
	if got := desktopMediaSequence(platform); !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence = %#v, want %#v", got, want)
	}
	if got := agent.pollCalls; len(got) != 1 {
		t.Fatalf("poll calls = %#v, want one poll while partial event is pending", got)
	}
}

func TestDesktopLiveSyncRetriesOnlyUnacknowledgedFiles(t *testing.T) {
	event := ExternalConversationEvent{
		SessionID: "thread-file-retry",
		Role:      "user",
		Files: []FileAttachment{
			{FileName: "one.txt"}, {FileName: "two.txt"}, {FileName: "three.txt"},
		},
	}
	agent, platform, engine := newDesktopMediaEngine(t, event)
	platform.failFileAt = 2
	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.pollDesktopLiveSync(context.Background(), agent)

	want := []string{
		"file:one.txt", "file:two.txt",
		"file:two.txt", "file:three.txt",
	}
	if got := desktopMediaSequence(platform); !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncStopsMediaWhenRouteChangesMidEvent(t *testing.T) {
	const (
		threadID   = "thread-route-media"
		sessionKey = "feishu:chat-route-media:user"
	)
	agent := &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
		threadID: {{
			SessionID: threadID,
			Role:      "user",
			Images: []ImageAttachment{
				{FileName: "one.png"}, {FileName: "two.png"},
			},
			Files: []FileAttachment{{FileName: "must-not-send.txt"}},
		}},
	}}}
	platform := &desktopMediaRouteSwitchPlatform{
		desktopMediaPlatform: desktopMediaPlatform{
			desktopSyncPlatform: desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}},
		},
		sessionKey: sessionKey,
	}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	platform.sessions = engine.sessions
	engine.sessions.NewSession(sessionKey, "old route").SetAgentSessionID(threadID, "codex")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	if got, want := desktopMediaSequence(&platform.desktopMediaPlatform), []string{"image:one.png"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence after route switch = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncSkipsMediaWhenAttachmentSendDisabled(t *testing.T) {
	event := ExternalConversationEvent{
		SessionID: "thread-media-disabled",
		Role:      "user",
		Content:   "body",
		Images:    []ImageAttachment{{FileName: "one.png"}},
		Files:     []FileAttachment{{FileName: "one.txt"}},
	}
	agent, platform, engine := newDesktopMediaEngine(t, event)
	engine.SetAttachmentSendEnabled(false)
	engine.pollDesktopLiveSync(context.Background(), agent)
	if got, want := desktopMediaSequence(platform), []string{"text:✣ Codex App · 你\nbody"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence = %#v, want %#v", got, want)
	}
}

func TestDesktopLiveSyncSkipsUnsupportedMediaWithoutRepeatingText(t *testing.T) {
	agent := &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
		"thread-no-media": {{
			SessionID: "thread-no-media",
			Role:      "user",
			Content:   "body",
			Images:    []ImageAttachment{{FileName: "one.png"}},
			Files:     []FileAttachment{{FileName: "one.txt"}},
		}},
	}}}
	platform := &desktopTextOnlyPlatform{desktopSyncPlatform: desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.sessions.NewSession("feishu:chat-no-media:user", "media").SetAgentSessionID("thread-no-media", "codex")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })

	engine.pollDesktopLiveSync(context.Background(), agent)
	engine.pollDesktopLiveSync(context.Background(), agent)
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if got, want := platform.sent, []string{"✣ Codex App · 你\nbody"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %#v, want %#v", got, want)
	}
}

func newDesktopMediaEngine(t *testing.T, event ExternalConversationEvent) (*trackingDesktopSyncAgent, *desktopMediaPlatform, *Engine) {
	t.Helper()
	agent := &trackingDesktopSyncAgent{desktopSyncAgent: &desktopSyncAgent{events: map[string][]ExternalConversationEvent{
		event.SessionID: {event},
	}}}
	platform := &desktopMediaPlatform{desktopSyncPlatform: desktopSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}}
	engine := NewEngine("test", agent, []Platform{platform}, "", LangChinese)
	engine.sessions.NewSession("feishu:chat-media:user", "media").SetAgentSessionID(event.SessionID, "codex")
	if !engine.markPlatformReady(platform) {
		t.Fatal("failed to mark platform ready")
	}
	t.Cleanup(func() { _ = engine.Stop() })
	return agent, platform, engine
}

func desktopMediaSequence(platform *desktopMediaPlatform) []string {
	platform.muMedia.Lock()
	defer platform.muMedia.Unlock()
	return append([]string(nil), platform.sequence...)
}
