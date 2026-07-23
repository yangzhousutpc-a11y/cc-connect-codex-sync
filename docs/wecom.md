# 企业微信智能机器人配置

企业微信入口使用智能机器人的 API 长连接模式。消息在企业微信、Codex App 和 Codex 之间双向同步，一个企业微信群稳定对应一个 Codex 会话。

## 准备与配置

1. 登录企业微信管理后台。
2. 创建一个智能机器人。
3. 在机器人配置中开启 API 模式，并选择“长连接”。
4. 保存 BotID 与 Secret。
5. 在安装 cc-connect 的 Mac 上运行：

   ```bash
   cc-connect wecom setup --project my-project
   ```

   按提示输入 `bot_id` 和 `bot_secret`。配置使用：

   ```toml
   mode = "websocket"
   ```

6. 重启服务：

   ```bash
   cc-connect daemon restart
   ```

BotID 与 Secret 只写入本机配置。不要把包含 Secret 的 `config.toml` 提交到 Git，也不要粘贴到 Issue、聊天记录或公开日志中。

API 长连接模式不需要公网 URL、CorpID、AgentID、Token、EncodingAESKey 或 cloudflared。

## 创建会话

1. 在企业微信中手动创建内部群。
2. 把已配置的智能机器人加入群聊。
3. 群内需要 @机器人 发送第一条消息。
4. cc-connect 会创建并绑定一个标题带 `[企业微信-Codex]` 的 Codex 会话。

此后，这个企业微信群始终进入同一个 Codex 会话；不同群之间不会共用上下文。单聊则按企业微信用户分别绑定 Codex 会话。

请注意：/new 不会自动创建企业微信群，也不会改变当前群的绑定、历史或上下文。它只会回复“手动新建内部群、添加机器人并 @机器人 发送首条消息”的操作引导。

## 验证

重启服务后，在群内 @机器人 发送一条测试消息，确认：

- Codex App 出现对应的 `[企业微信-Codex]` 会话；
- 企业微信消息进入该会话；
- Codex App 中发送的消息和 Codex 回复都能返回原企业微信群。
