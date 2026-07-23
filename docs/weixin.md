# 个人微信接入

本通道连接个人微信的 ilink 接口，不是企业微信。

## 配置

先复制示例配置并修改项目名、绝对工作目录：

```bash
install -m 600 config.example.toml ~/.cc-connect/config.toml
cc-connect weixin setup --project my-project
```

按照终端提示扫码登录。Token、账号标识和登录状态只应保存在本机，不能提交到 Git，也不要复制到其他设备共享。

启动服务后，在微信中向已连接入口发送 `/doctor` 或普通消息：

```bash
cc-connect daemon install
cc-connect daemon status
```

## 会话规则

- 微信联系人或会话稳定绑定一个独立 Codex 会话。
- 微信消息会出现在 Codex App；Codex App 中的用户消息和 Codex 回复也会回传到原微信会话。
- 会话名使用 `[微信-Codex]` 前缀，回复保留来源标识。
- 断线重连与进程重启后继续使用原绑定，不跳过离线期间待处理消息。

## 附件安全规则

- 文字消息和结构化图片会自动双向同步。
- 普通文件不会仅因出现在 Codex App 的 Files block 中就自动发送。
- 在 Codex App 附加文件后，请明确说“把这个文件发送到当前群”。这里的“当前群”指当前绑定的个人微信会话；Agent 会使用 `cc-connect send --file` 完成发送。
- 发送成功后只确认结果，不要在确认消息中展示本机路径。

文件可以发送；这项规则只避免未经明确指示的文件外发。
