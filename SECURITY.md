# Security Policy

[中文](#安全政策) | [English](#english)

## 安全政策

### 支持范围

安全修复以本仓库最新 GitHub Release 为准。旧版本可能不会单独回补。

### 私密报告漏洞

请优先使用 GitHub Security Advisory 的私密报告入口：

<https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/security/advisories/new>

请提供受影响版本、macOS 版本与架构、最小复现步骤、影响范围和必要的脱敏错误信息。不要在公开 Issue、附件或日志中提交以下内容：

- `config.toml` 或其他真实配置；
- API Key、Token、`app_secret`、Cookie 或微信登录状态；
- 飞书群、微信用户、Codex 会话等可识别 ID；
- 会话正文、完整日志、个人目录或其他隐私数据。

如果只是普通缺陷且不涉及安全或隐私，可以使用公开 Issue。维护者会尽力确认报告，但不承诺固定响应时间。

## English

### Supported versions

Security fixes target the latest GitHub Release of this repository. Older releases may not receive backports.

### Privately reporting a vulnerability

Prefer GitHub Security Advisories for private reports:

<https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/security/advisories/new>

Include the affected version, macOS version and architecture, minimal reproduction steps, impact, and only the redacted diagnostics needed to investigate. Do not post real configuration files, credentials, tokens, cookies, Weixin login state, identifiable chat or session IDs, conversation content, full logs, or personal filesystem paths in public issues or attachments.

Use public issues for ordinary non-security bugs. Reports are handled on a best-effort basis without a guaranteed response time.
