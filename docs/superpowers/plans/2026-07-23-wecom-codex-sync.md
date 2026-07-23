# 企业微信与 Codex 双向同步实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在当前 Codex、飞书、个人微信精简版中加入企业微信智能机器人长连接，使企业微信群与 Codex 会话稳定一对一并支持双向同步。

**Architecture:** 从上游 `platform/wecom` 仅移植 WebSocket 智能机器人部分，通过稳定的 `wecom:g:{chatID}` / `wecom:u:{userID}` 会话键接入现有会话、桌面同步和多工作区恢复机制。企业微信平台显式实现异步生命周期、回复上下文恢复和桌面转发能力；`/new` 通过一个只读引导接口返回人工建群步骤，不触发任何会话写操作。

**Tech Stack:** Go 1.25、gorilla/websocket、TOML、macOS shell 安装器、Go 单元/集成测试。

---

## 文件结构

新增文件：

- `platform/wecom/websocket.go`：连接、订阅、心跳、重连、文本消息路由和主动发送。
- `platform/wecom/media.go`：企业微信入站媒体下载、AES 解密和出站分片上传。
- `platform/wecom/mention.go`：移除企业微信 `@机器人` 标记。
- `platform/wecom/websocket_test.go`：连接协议、稳定会话键、并发顺序、重连和发送确认测试。
- `platform/wecom/media_test.go`：媒体解密、大小限制、文件名和上传测试。
- `cmd/cc-connect/plugin_platform_wecom.go`：静态注册企业微信平台。
- `cmd/cc-connect/wecom.go`：`cc-connect wecom setup` 配置命令。
- `cmd/cc-connect/wecom_test.go`：企业微信配置命令测试。
- `docs/wecom.md`：企业微信后台配置与使用说明。
- `core/manual_new_guide_test.go`：企业微信 `/new` 只读引导回归测试。

修改文件：

- `core/interfaces.go`、`core/engine.go`：平台专属 `/new` 人工操作引导。
- `agent/codex/codex.go`、`agent/codex/conversation_name_test.go`：`[企业微信-Codex]` 标题。
- `config/config.go`、`config/config_test.go`：企业微信安全配置写入。
- `cmd/cc-connect/main.go`、`cmd/cc-connect/main_test.go`：命令入口、帮助和示例。
- `packaging/macos/setup.sh`、`tests/open_source_installer/setup_test.sh`：安装向导平台组合。
- `AGENTS.md`、`README.md`、`README.zh-CN.md`、`INSTALL.md`、`config.example.toml`、`packaging/macos/README.zh-CN.md`：公开范围和安装说明。
- `tests/minimal_scope_test.sh`、`packaging/macos/scan-public-bundle.sh`：允许 `wecom`，继续拒绝其他上游平台与秘密。

## Task 1：平台专属 `/new` 引导与会话标题

**Files:**

- Modify: `core/interfaces.go`
- Modify: `core/engine.go`
- Create: `core/manual_new_guide_test.go`
- Modify: `agent/codex/codex.go`
- Modify: `agent/codex/conversation_name_test.go`

- [ ] **Step 1：编写 `/new` 只读引导失败测试**

在 `core/manual_new_guide_test.go` 定义只实现引导能力的平台，并断言 `/new` 不创建会话、不调用智能体：

```go
type manualNewGuidePlatform struct {
	stubPlatformEngine
	replies []string
}

func (p *manualNewGuidePlatform) ManualNewConversationGuide() string {
	return "请手动新建企业微信群，添加机器人后 @机器人 发送第一条消息。"
}

func (p *manualNewGuidePlatform) Reply(_ context.Context, _ any, content string) error {
	p.replies = append(p.replies, content)
	return nil
}

func TestCmdNewManualGuideDoesNotMutateCurrentSession(t *testing.T) {
	agent := &stubAgent{name: "codex"}
	p := &manualNewGuidePlatform{stubPlatformEngine: stubPlatformEngine{n: "wecom"}}
	e := NewEngine("test", agent, []Platform{p}, "", LangChinese)
	before := len(e.sessions.ListSessions("wecom:g:chat-a"))

	e.cmdNew(p, &Message{
		Platform: "wecom", SessionKey: "wecom:g:chat-a",
		MessageID: "msg-1", UserID: "user-a", ReplyCtx: "ctx",
	}, nil)

	if got := len(e.sessions.ListSessions("wecom:g:chat-a")); got != before {
		t.Fatalf("sessions changed: got %d want %d", got, before)
	}
	if len(p.replies) != 1 || !strings.Contains(p.replies[0], "手动新建企业微信群") {
		t.Fatalf("replies = %#v", p.replies)
	}
}
```

- [ ] **Step 2：运行测试并确认失败**

Run:

```bash
go test ./core -run TestCmdNewManualGuideDoesNotMutateCurrentSession -count=1
```

Expected: FAIL，提示 `ManualNewConversationGuide` 尚未被 `cmdNew` 使用。

- [ ] **Step 3：实现最小引导接口**

在 `core/interfaces.go` 增加：

```go
// ManualNewConversationGuideProvider marks a platform whose native API cannot
// create a new external conversation. The returned text is informational only.
type ManualNewConversationGuideProvider interface {
	ManualNewConversationGuide() string
}
```

在 `core/engine.go` 的 `cmdNew` 开头、解析工作区之前增加：

```go
if guide, ok := p.(ManualNewConversationGuideProvider); ok {
	if text := strings.TrimSpace(guide.ManualNewConversationGuide()); text != "" {
		e.reply(p, msg.ReplyCtx, text)
		return
	}
}
```

这条分支不得调用 `GetOrCreateActive`、`NewOperationStore` 或智能体。

- [ ] **Step 4：添加企业微信标题前缀测试并实现**

向 `agent/codex/conversation_name_test.go` 的表驱动用例增加：

```go
{platform: "wecom", input: "企业微信群-ab12cd", want: "[企业微信-Codex] 企业微信群-ab12cd"},
{platform: "wecom", input: "[Codex] 项目A", want: "[企业微信-Codex] 项目A"},
```

在 `agent/codex/codex.go` 增加：

```go
wecomConversationNamePrefix          = "[企业微信-Codex]"
decoratedWecomConversationNamePrefix = "🏢 [企业微信-Codex]"
```

并在 `FormatConversationNameForPlatform` 的 `switch` 中处理 `wecom`，同时把两个前缀加入旧前缀剥离列表。

- [ ] **Step 5：运行测试并提交**

Run:

```bash
go test ./core ./agent/codex -run 'TestCmdNewManualGuide|TestFormatConversationNameForPlatform' -count=1
```

Expected: PASS。

Commit:

```bash
git add core/interfaces.go core/engine.go core/manual_new_guide_test.go agent/codex/codex.go agent/codex/conversation_name_test.go
git commit -m "add WeCom conversation rules"
```

## Task 2：企业微信长连接与稳定会话路由

**Files:**

- Create: `platform/wecom/websocket.go`
- Create: `platform/wecom/mention.go`
- Create: `platform/wecom/websocket_test.go`
- Create: `cmd/cc-connect/plugin_platform_wecom.go`

- [ ] **Step 1：移植上游协议夹具并先写路由失败测试**

从 `upstream/main:platform/wecom/websocket_test.go` 提取订阅、消息回调、发送确认和重连夹具，但测试导入路径使用当前模块。新增以下核心断言：

```go
func TestSessionKeySharesOneGroupAcrossUsers(t *testing.T) {
	if got := stableSessionKey("group", "chat-a", "user-1"); got != "wecom:g:chat-a" {
		t.Fatalf("group key = %q", got)
	}
	if got := stableSessionKey("group", "chat-a", "user-2"); got != "wecom:g:chat-a" {
		t.Fatalf("group key changed by sender: %q", got)
	}
}

func TestSessionKeySeparatesDirectUsers(t *testing.T) {
	if got := stableSessionKey("single", "", "user-1"); got != "wecom:u:user-1" {
		t.Fatalf("single key = %q", got)
	}
}

func TestReplyContextRoundTrip(t *testing.T) {
	p := &Platform{}
	ctx, err := p.ReconstructReplyCtx("wecom:g:chat-a")
	if err != nil {
		t.Fatal(err)
	}
	rc := ctx.(replyContext)
	if rc.chatID != "chat-a" || rc.chatType != "group" {
		t.Fatalf("reply context = %#v", rc)
	}
}
```

- [ ] **Step 2：运行测试并确认失败**

Run:

```bash
go test ./platform/wecom -run 'TestSessionKey|TestReplyContextRoundTrip' -count=1
```

Expected: FAIL，因为包和实现尚不存在。

- [ ] **Step 3：移植最小 WebSocket 实现**

以 `upstream/main` 的以下文件为协议基线：

```text
platform/wecom/websocket.go
platform/wecom/mention_strip.go
platform/wecom/message_split.go
```

只保留 WebSocket 代码，统一类型名为 `Platform`，构造函数固定要求：

```go
func New(opts map[string]any) (core.Platform, error) {
	mode, _ := opts["mode"].(string)
	if mode != "" && mode != "websocket" {
		return nil, fmt.Errorf("wecom: only websocket mode is supported")
	}
	botID, _ := opts["bot_id"].(string)
	secret, _ := opts["bot_secret"].(string)
	if strings.TrimSpace(botID) == "" || strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("wecom: bot_id and bot_secret are required")
	}
	allowFrom, _ := opts["allow_from"].(string)
	core.CheckAllowFrom("wecom", allowFrom)
	return &Platform{botID: botID, secret: secret, allowFrom: allowFrom}, nil
}
```

稳定会话键只能由会话类型与聊天目标决定：

```go
func stableSessionKey(chatType, chatID, userID string) string {
	if chatType == "group" {
		return "wecom:g:" + strings.TrimSpace(chatID)
	}
	return "wecom:u:" + strings.TrimSpace(userID)
}
```

构造 `core.Message` 时必须设置：

```go
sessionKey := stableSessionKey(body.ChatType, body.ChatID, body.From.UserID)
channelID := strings.TrimPrefix(sessionKey, "wecom:")

msg := &core.Message{
	SessionKey: sessionKey,
	Platform:   "wecom",
	ChannelKey: channelID,
	MessageID:  body.MsgID,
	UserID:     body.From.UserID,
	UserName:   body.From.UserID,
	ChatName:   conversationDisplayName(body.ChatType, body.ChatID, body.From.UserID),
	Content:    content,
	ReplyCtx:   replyContext{chatID: targetID, chatType: body.ChatType, userID: body.From.UserID, reqID: reqID},
}
```

群聊名称使用 `企业微信群-` 加 `chatID` 最后六个字符；单聊使用用户 ID 最后六个字符。

- [ ] **Step 4：接入异步生命周期与并发顺序**

`Platform` 增加：

```go
lifecycle core.PlatformLifecycleHandler
orderMu   sync.Mutex
previous map[string]chan struct{}
```

实现：

```go
func (p *Platform) SetLifecycleHandler(h core.PlatformLifecycleHandler) {
	p.lifecycle = h
}

func (p *Platform) notifyReady() {
	if p.lifecycle != nil {
		p.lifecycle.OnPlatformReady(p)
	}
}

func (p *Platform) notifyUnavailable(err error) {
	if p.lifecycle != nil {
		p.lifecycle.OnPlatformUnavailable(p, err)
	}
}
```

订阅确认成功后调用 `notifyReady`，连接读循环退出后调用 `notifyUnavailable`。同一稳定会话键的下一条消息只等待前一条消息的 `DispatchAdmission`，不得等待整个 Codex 回合结束。

订阅返回明确的凭据错误时结束连接循环并等待人工修正或服务重启；网络断开才进入 1、2、4 秒直至 30 秒的退避重连，避免错误 Secret 形成永久重试风暴。

- [ ] **Step 5：实现恢复与桌面同步能力**

实现：

```go
func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) != 3 || parts[0] != "wecom" || parts[2] == "" {
		return nil, fmt.Errorf("wecom: invalid session key %q", sessionKey)
	}
	switch parts[1] {
	case "g":
		return replyContext{chatID: parts[2], chatType: "group"}, nil
	case "u":
		return replyContext{chatID: parts[2], chatType: "single", userID: parts[2]}, nil
	default:
		return nil, fmt.Errorf("wecom: invalid session kind %q", parts[1])
	}
}

func (*Platform) ExternalConversationRelayEnabled() bool { return true }

func (*Platform) FormattingInstructions() string {
	return "Replies are delivered to WeCom. Use concise Markdown and avoid Markdown tables."
}

func (*Platform) ManualNewConversationGuide() string {
	return "企业微信暂不支持机器人自动建群。请手动新建企业微信内部群，添加本机器人，然后在新群中 @机器人 发送第一条消息；当前群和当前 Codex 会话不会改变。"
}
```

主动发送 `aibot_send_msg` 时必须携带 `chat_type`：单聊为 `1`，群聊为 `2`。只有收到成功 ACK 才返回 `nil`。

增加编译期能力断言：

```go
var (
	_ core.AsyncRecoverablePlatform          = (*Platform)(nil)
	_ core.ReplyContextReconstructor         = (*Platform)(nil)
	_ core.ExternalConversationRelayTarget   = (*Platform)(nil)
	_ core.FormattingInstructionProvider     = (*Platform)(nil)
	_ core.ManualNewConversationGuideProvider = (*Platform)(nil)
)
```

- [ ] **Step 6：注册插件、运行测试并提交**

创建 `cmd/cc-connect/plugin_platform_wecom.go`：

```go
//go:build !no_wecom

package main

import _ "github.com/yangzhousutpc-a11y/cc-connect-codex-sync/platform/wecom"
```

Run:

```bash
go test ./platform/wecom ./core -run 'WeCom|SessionKey|ReplyContext|Desktop' -count=1
```

Expected: PASS。

Commit:

```bash
git add platform/wecom cmd/cc-connect/plugin_platform_wecom.go
git commit -m "add WeCom WebSocket platform"
```

## Task 3：企业微信图片和文件双向传输

**Files:**

- Create: `platform/wecom/media.go`
- Create: `platform/wecom/media_test.go`
- Modify: `platform/wecom/websocket.go`

- [ ] **Step 1：移植媒体测试并加入边界测试**

从上游 `websocket_media_test.go`、`websocket_outbound_media.go` 提取 AES、混合消息和上传 ACK 夹具，增加：

```go
func TestMediaRejectsPayloadAboveLimit(t *testing.T) {
	p := &Platform{httpClient: http.DefaultClient}
	_, err := p.downloadAndDecrypt(context.Background(), wsMediaRef{
		URL: "https://example.invalid/oversize", AESKey: validAESKey,
	}, wecomMediaMaxBytes+1)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
}

func TestOutboundFileNameUsesBaseName(t *testing.T) {
	got := outboundFileName(core.FileAttachment{FileName: "../../report.pdf"})
	if got != "report.pdf" {
		t.Fatalf("name = %q", got)
	}
}
```

- [ ] **Step 2：运行测试并确认失败**

Run:

```bash
go test ./platform/wecom -run 'Media|File|Image' -count=1
```

Expected: FAIL，媒体函数尚不存在。

- [ ] **Step 3：实现媒体接收**

按上游协议实现以下固定边界：

```go
const wecomMediaMaxBytes = 20 << 20

type wsMediaRef struct {
	URL    string
	AESKey string
}
```

- 只允许 `https` 下载地址；
- `io.LimitReader` 最多读取 `20 MiB + 1`；
- AES-256-CBC，IV 使用密钥前 16 字节；
- PKCS#7 严格校验；
- 图片填入 `core.ImageAttachment`；
- 文件名只取 `filepath.Base`，文件填入 `core.FileAttachment`；
- 混合消息按文本、图片、文件原顺序收集，下载失败不丢弃已有文本。

- [ ] **Step 4：实现媒体发送**

移植上游三段式协议：

```text
aibot_upload_media_init
aibot_upload_media_chunk
aibot_upload_media_finish
```

固定分片 `512 KiB`、最多 100 片。`SendImage` 与 `SendFile` 必须先得到 `media_id`，再使用 `aibot_send_msg`，并等待严格 ACK；超时返回错误，不把桌面同步事件标记为已送达。

- [ ] **Step 5：运行测试并提交**

Run:

```bash
go test ./platform/wecom -run 'Media|File|Image|Upload' -count=1
```

Expected: PASS。

Commit:

```bash
git add platform/wecom/media.go platform/wecom/media_test.go platform/wecom/websocket.go platform/wecom/websocket_test.go
git commit -m "add WeCom media transfer"
```

## Task 4：安全配置命令

**Files:**

- Modify: `config/config.go`
- Modify: `config/config_test.go`
- Create: `cmd/cc-connect/wecom.go`
- Create: `cmd/cc-connect/wecom_test.go`
- Modify: `cmd/cc-connect/main.go`
- Modify: `cmd/cc-connect/main_test.go`

- [ ] **Step 1：编写配置保存失败测试**

在 `config/config_test.go` 增加测试，覆盖新建平台、更新既有平台和保留 `allow_from`：

```go
func TestSaveWeComCredentialsUpdatesOnlyTargetPlatform(t *testing.T) {
	configPath := writeConfigFixture(t, feishuConfigFixture)
	ConfigPath = configPath

	result, err := SaveWeComPlatformCredentials(WeComCredentialUpdateOptions{
		ProjectName: "test", BotID: "bot-new", BotSecret: "secret-new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PlatformType != "wecom" {
		t.Fatalf("platform = %q", result.PlatformType)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Projects[0].Platforms[result.PlatformAbsIndex]
	if stringMapValue(p.Options, "bot_id") != "bot-new" ||
		stringMapValue(p.Options, "bot_secret") != "secret-new" {
		t.Fatalf("options = %#v", p.Options)
	}
}
```

- [ ] **Step 2：运行测试并确认失败**

Run:

```bash
go test ./config -run WeCom -count=1
```

Expected: FAIL，企业微信配置类型和保存函数尚不存在。

- [ ] **Step 3：实现配置更新**

在 `config/config.go` 增加：

```go
type WeComCredentialUpdateOptions struct {
	ProjectName   string
	PlatformIndex int
	BotID         string
	BotSecret     string
	AllowFrom     string
}

type WeComCredentialUpdateResult struct {
	ProjectName      string
	ProjectIndex     int
	PlatformAbsIndex int
	PlatformType     string
	AllowFrom        string
}
```

实现 `EnsureProjectWithWeComPlatform` 与 `SaveWeComPlatformCredentials`，写入：

```toml
type = "wecom"
mode = "websocket"
bot_id = "..."
bot_secret = "..."
```

要求：BotID、Secret 为空时拒绝；重复执行更新同一平台；不覆盖其他平台；配置文件权限继续保持 `0600`。

- [ ] **Step 4：实现 `cc-connect wecom setup`**

`cmd/cc-connect/wecom.go` 提供：

```text
cc-connect wecom setup --project NAME --bot-id ID --bot-secret SECRET [--allow-from IDS]
cc-connect wecom bind  --project NAME --bot-id ID --bot-secret SECRET [--allow-from IDS]
```

无参数时使用 `term.ReadPassword` 读取 Secret；标准输出只显示 BotID 的末四位，不显示 Secret。成功提示下一步为“重启服务并在企业微信群中 @机器人 发送第一条消息”。

在 `cmd/cc-connect/main.go` 命令分派增加：

```go
case "wecom":
	runWeCom(os.Args[2:])
	return
```

- [ ] **Step 5：运行测试并提交**

Run:

```bash
go test ./config ./cmd/cc-connect -run 'WeCom|Usage|Bootstrap' -count=1
```

Expected: PASS，并且捕获输出中不存在测试 Secret。

Commit:

```bash
git add config/config.go config/config_test.go cmd/cc-connect/wecom.go cmd/cc-connect/wecom_test.go cmd/cc-connect/main.go cmd/cc-connect/main_test.go
git commit -m "add WeCom guided setup"
```

## Task 5：macOS 安装向导与公开包

**Files:**

- Modify: `packaging/macos/setup.sh`
- Modify: `tests/open_source_installer/setup_test.sh`
- Modify: `tests/minimal_scope_test.sh`
- Modify: `packaging/macos/scan-public-bundle.sh`
- Modify: `AGENTS.md`

- [ ] **Step 1：扩展安装器失败测试**

在 `tests/open_source_installer/setup_test.sh` 增加平台矩阵：

```sh
assert_platform_choice 1 feishu
assert_platform_choice 2 weixin
assert_platform_choice 3 feishu weixin
assert_platform_choice 4 wecom
assert_platform_choice 5 feishu wecom
assert_platform_choice 6 weixin wecom
assert_platform_choice 7 feishu weixin wecom
```

每个断言使用假的运行程序记录实际调用，企业微信组合必须出现且只出现一次：

```text
wecom setup --config <staging-config> --project <project>
```

- [ ] **Step 2：运行测试并确认失败**

Run:

```bash
bash tests/open_source_installer/setup_test.sh
```

Expected: FAIL，选项 4 至 7 尚不支持。

- [ ] **Step 3：实现安装器矩阵**

在 `packaging/macos/setup.sh` 初始化三个布尔值：

```sh
setup_feishu=0
setup_weixin=0
setup_wecom=0
```

保留 1 至 3 原含义，新增：

```sh
4) setup_wecom=1 ;;
5) setup_feishu=1; setup_wecom=1 ;;
6) setup_weixin=1; setup_wecom=1 ;;
7) setup_feishu=1; setup_weixin=1; setup_wecom=1 ;;
*) die '平台选择必须是 1 到 7' ;;
```

企业微信分支执行：

```sh
if [ "$setup_wecom" -eq 1 ]; then
  say '配置企业微信智能机器人'
  "$runtime" wecom setup --config "$staging_config" --project "$project"
fi
```

- [ ] **Step 4：更新精简范围和隐私扫描**

`AGENTS.md` 的消息入口改为“飞书、个人微信、企业微信”，并要求企业微信路由改动运行 `platform/wecom` 回归。

`tests/minimal_scope_test.sh` 允许 `platform/wecom` 与 `plugin_platform_wecom.go`，仍明确拒绝 Telegram、Slack、Discord、钉钉等目录。

`scan-public-bundle.sh` 增加 `bot_secret`、类似真实 BotID 的模式和企业微信本机状态目录检查；示例占位符允许通过。

- [ ] **Step 5：运行测试并提交**

Run:

```bash
bash tests/open_source_installer/setup_test.sh
bash tests/minimal_scope_test.sh
bash tests/open_source_installer/source_bundle_test.sh
```

Expected: PASS。

Commit:

```bash
git add packaging/macos/setup.sh packaging/macos/scan-public-bundle.sh tests/open_source_installer/setup_test.sh tests/minimal_scope_test.sh AGENTS.md
git commit -m "package WeCom setup"
```

## Task 6：公开文档与配置示例

**Files:**

- Create: `docs/wecom.md`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `INSTALL.md`
- Modify: `config.example.toml`
- Modify: `packaging/macos/README.zh-CN.md`
- Modify: `cmd/cc-connect/main.go`
- Modify: `tests/open_source_installer/source_bundle_test.sh`

- [ ] **Step 1：编写文档一致性失败测试**

在现有 shell 文档检查中断言以下文字同时存在：

```text
[企业微信-Codex]
cc-connect wecom setup
mode = "websocket"
bot_id
bot_secret
群内需要 @机器人
/new 不会自动创建企业微信群
```

并断言文档不要求公网 URL、CorpID、AgentID、Token、EncodingAESKey 或 cloudflared。

- [ ] **Step 2：运行测试并确认失败**

Run:

```bash
bash tests/open_source_installer/source_bundle_test.sh
```

Expected: FAIL，企业微信文档尚不存在。

- [ ] **Step 3：编写最小企业微信指南**

`docs/wecom.md` 必须按实际顺序说明：

1. 登录企业微信管理后台；
2. 创建智能机器人；
3. 开启 API 模式并选择“长连接”；
4. 取得 BotID 与 Secret；
5. 运行 `cc-connect wecom setup`；
6. 重启服务；
7. 手动创建内部群并添加机器人；
8. 群内 `@机器人` 发送第一条消息；
9. 一个群对应一个 Codex 会话；
10. `/new` 只给建群引导。

明确 Secret 只写本机配置，不提交 Git。

- [ ] **Step 4：统一公开入口**

README、安装文档、配置示例和命令帮助全部改为：

```text
Agent: Codex
Platforms: Feishu, personal Weixin, WeCom
```

`config.example.toml` 增加注释掉的企业微信块；不能启用占位凭据导致首次启动失败。

- [ ] **Step 5：运行测试并提交**

Run:

```bash
bash tests/open_source_installer/source_bundle_test.sh
go test ./cmd/cc-connect -run 'Usage|ConfigExample' -count=1
```

Expected: PASS。

Commit:

```bash
git add docs/wecom.md README.md README.zh-CN.md INSTALL.md config.example.toml packaging/macos/README.zh-CN.md cmd/cc-connect/main.go tests/open_source_installer/source_bundle_test.sh
git commit -m "document WeCom integration"
```

## Task 7：针对性复核、全量测试和本机实装

**Files:**

- Modify only if a test exposes a defect directly caused by Tasks 1–6.

- [ ] **Step 1：运行针对性平台与路由测试**

Run:

```bash
go test ./platform/wecom ./core ./agent/codex ./config ./cmd/cc-connect -count=1
go test ./tests/integration -count=1
```

Expected: PASS。

- [ ] **Step 2：运行全量验证**

Run:

```bash
make verify
make test-open-source-installer
```

Expected: 全部 PASS；不得通过跳过、放宽或删除既有测试获得通过。

- [ ] **Step 3：构建并扫描安装包**

Run:

```bash
make package-source-installer
```

Expected: 生成单一 macOS 源码安装包和 SHA-256 文件，隐私扫描通过，包内只包含 Codex、飞书、个人微信、企业微信及必要基础设施。

- [ ] **Step 4：备份并刷新本机 installer**

先将现有 `/Users/yangzhou/cc-connect/installer` 复制到带北京时间戳的备份目录，再用已验证的新安装包刷新 installer；不得覆盖 `/Users/yangzhou/cc-connect/data` 中的配置、会话和平台状态。

- [ ] **Step 5：部署本机运行程序**

使用正式安装流程构建新二进制，保留旧二进制备份，重启 launchd 服务，并核对：

```bash
/Users/yangzhou/cc-connect/runtime/cc-connect --version
/Users/yangzhou/cc-connect/runtime/cc-connect daemon status
/Users/yangzhou/cc-connect/installer/doctor.sh
```

Expected: 服务运行，飞书和个人微信原配置不变；企业微信在尚未配置时不影响启动。

- [ ] **Step 6：执行企业微信实机验收**

用户在本机正式配置中录入 BotID 与 Secret 后：

1. A、B 两个企业微信群分别添加机器人；
2. A 群两位成员分别 `@机器人`，只创建一个 A 群 Codex 会话；
3. B 群创建独立 Codex 会话；
4. 两个 Codex 会话分别发送消息，只回各自群；
5. 验证图片、文件、网络断开恢复和服务重启；
6. A 群执行 `/new`，只出现建群引导且 A 群上下文保持不变。

Expected: 全部通过；任一消息目标错误、重复会话或未确认发送被丢弃均视为失败。

- [ ] **Step 7：最终检查与提交**

Run:

```bash
git status --short
git log --oneline --max-count=10
```

Expected: 只包含计划内改动，无临时凭据、日志、会话数据或构建产物。

如 Task 7 产生了必要修复，单独提交：

```bash
git add <仅限修复文件>
git commit -m "fix WeCom acceptance findings"
```

本计划不自动推送 GitHub、不创建 Release；完成本地实机验收后，只有收到用户明确授权才执行外部发布。
