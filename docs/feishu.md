# 飞书接入

## 前提

- Codex CLI 已安装并完成登录。
- 已在飞书开放平台创建企业自建应用。
- 应用已启用机器人能力与长连接事件订阅。

## 配置

先复制示例配置并修改项目名、绝对工作目录：

```bash
install -m 600 config.example.toml ~/.cc-connect/config.toml
cc-connect feishu setup --project my-project
```

按照终端提示填写飞书 `App ID`、`App Secret` 和允许访问的用户或群。凭据只保存在本机配置中，不要提交到 Git。

在飞书开放平台为应用开通收发消息、读取群信息、创建群及管理群成员所需权限，然后发布应用版本。将机器人加入目标群后发送 `/doctor`，应能看到平台与 Codex 均已连接。

## 一对一会话

- 每个飞书群稳定绑定一个 Codex 会话。
- 群消息和 Codex App 内消息双向可见。
- 在 A 群执行 `/new 名称` 时，只创建一个新飞书群和一个新 Codex 会话；A 群继续绑定原会话。
- 新建群名与新会话名使用 `[飞书-Codex]` 前缀。

启动或更新后台服务：

```bash
cc-connect daemon install
cc-connect daemon status
```
