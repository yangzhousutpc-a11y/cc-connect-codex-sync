# 开发约定

本仓库只维护以下运行组合：

- 智能体：Codex
- 消息入口：飞书、个人微信、企业微信

不要在本仓库中加入其他智能体、消息平台、Web 管理端或 npm 分发层。修改应保持最小，并在提交前执行：

```bash
make verify
make test-open-source-installer
```

涉及消息路由、会话绑定或 `/new` 时，还应运行相应的 `core`、`agent/codex`、`platform/feishu`、`platform/weixin`、`platform/wecom` 回归测试。

任何示例配置不得包含真实凭据、会话数据或设备登录状态。

## 附件回传

- 飞书、个人微信和企业微信统一执行这一规则。
- 文字消息和结构化图片会自动双向同步。
- 普通文件不会仅因出现在 Codex App 的 Files block 中就自动发送。
- 用户在 Codex App 附加文件并明确说“把这个文件发送到当前群”后，Agent 才使用 `cc-connect send --file "/绝对路径/文件"` 回传。
- 命令明确成功后才能确认已发送；不要在确认消息中展示本机路径。
- 企业微信入站文件受支持。这项规则只限制 Codex 到平台的文件外发，不代表文件无法发送。
