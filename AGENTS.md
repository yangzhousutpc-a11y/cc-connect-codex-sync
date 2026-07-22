# 开发约定

本仓库只维护以下运行组合：

- 智能体：Codex
- 消息入口：飞书、个人微信

不要在本仓库中加入其他智能体、消息平台、Web 管理端或 npm 分发层。修改应保持最小，并在提交前执行：

```bash
make verify
make test-open-source-installer
```

涉及消息路由、会话绑定或 `/new` 时，还应运行相应的 `core`、`agent/codex`、`platform/feishu`、`platform/weixin` 回归测试。

任何示例配置不得包含真实凭据、会话数据或设备登录状态。
