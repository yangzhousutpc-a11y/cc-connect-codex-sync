# macOS 真正一条命令安装设计

## 目标

普通 macOS 用户只复制一条命令，就能完成最新 Release 的定位、下载、SHA-256 校验、解压，并进入现有 `setup.sh` 交互向导。飞书凭据、微信扫码和 macOS 权限仍由用户本人确认。

## 公开入口

仓库新增 `install-macos.sh`，README、中文 README 和 `INSTALL.md` 统一展示：

```bash
sh -c "$(curl -fsSL https://raw.githubusercontent.com/yangzhousutpc-a11y/cc-connect-codex-sync/main/install-macos.sh)"
```

保留手动下载、校验和运行 `./setup.sh` 的方式，作为可审计的备选方案；Agent 引导式安装继续作为另一种备选方式。

## 下载与安装流程

`install-macos.sh` 只承担公开安装引导，不复制 `setup.sh` 的配置逻辑：

1. 校验系统为 macOS 12 或更高版本，并确认存在 `curl`、`tar` 和 `shasum`。
2. 通过仓库公开的 `releases/latest` 重定向取得最新稳定标签，不依赖匿名 GitHub API。
3. 校验标签格式，并据此构造唯一的源码安装包和 `.sha256` 下载地址。
4. 使用系统默认临时目录创建本轮专用物理目录。
5. 下载两个文件，执行 `shasum -a 256 -c`，失败立即停止。
6. 确认压缩包只有 `cc-connect-source-install` 顶层目录，解压后确认 `setup.sh` 是普通可执行文件。
7. 调用安装包内的 `./setup.sh`，沿用当前终端的交互输入。
8. 正常结束、失败或中断时，只清理本轮解析后的临时目录。

## 安全边界

- 不使用 `sudo`，不修改 Gatekeeper，不跳过 Codex 审批。
- 只访问 `yangzhousutpc-a11y/cc-connect-codex-sync` 的 GitHub Raw 和 Release。
- 不依赖 GitHub API Token，也不接受非稳定或格式异常的标签。
- 不自行生成校验值，不接受缺失、重复或文件名不匹配的资产。
- 不覆盖配置、会话或登录状态；新装和升级仍完全交给 `setup.sh`。
- 不通过 `/tmp` 的逻辑软链接路径运行安装包，使用 `mktemp -d` 返回的物理路径。

## 错误处理

错误信息明确区分系统不兼容、工具缺失、网络失败、Release 标签异常、校验失败、压缩包结构异常和向导失败。任何前置步骤失败都不启动安装；`setup.sh` 失败时保留用户数据并返回原始退出状态。

## 测试与验收

先新增引导脚本测试并确认在缺少实现时失败，再实现最小脚本。测试覆盖：

- 最新 Release 重定向与两个资产下载；
- SHA-256 成功和失败；
- 非法标签、错误顶层目录、缺失或软链接 `setup.sh`；
- `setup.sh` 调用、退出码传递和临时目录清理；
- README 三处命令保持一致。

完成后运行仓库全量验证和安装器测试，发布 v1.0.2，再从公网复制 README 的单行命令，在隔离目录中验收到 `setup.sh` 的平台选择或人工授权边界，且不改动现有 `~/cc-connect`。
