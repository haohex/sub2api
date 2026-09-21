# AGENTS

本仓库是 Wei-Shaw/sub2api 的 fork；`klno` 分支只维护 docs/conventions/ 里描述的补丁。

- 写码前读 `docs/conventions/codex-outbound-identity.md`；续接工作先看 `docs/dev-journal.md` 最新条目和 `docs/tasks/`。
- 非平凡改动后回写 journal；产生新约定就改写 conventions（不是追加）。
- 创建或修改 Git commit、Pull Request 标题或正文时，必须使用 `chinese-commit-conventions` skill；type 保留英文，scope、subject 和 body 使用中文，并按该规范关联 Issue。
- 补丁必须小、可 rebase；协议事实以 openai/codex 源码为准。
- Git 远程约定：origin 指向个人仓库 haohex/sub2api，upstream 指向原始上游 Wei-Shaw/sub2api，klno 指向直接父 Fork KlN-4096/sub2api。
- 日常更新先执行 git fetch --prune upstream klno origin。Wei-Shaw 的根上游更新通过 upstream 获取；KlN 的补丁更新以其正式 Release tag 为准，当前补丁线是 klno 分支，不把 klno/main 当作 Release 内容快照。
- KlN Release 同步由独立的 GitHub Actions 轮询完成：以最新正式 Release tag 创建 sync/kln-release/<tag> 分支并向 main 提交一个 PR。同一时间只保留一个 KlN 同步 PR；冲突留在 PR 中等待人工解决和审查，不自动合并或强制推送。
- 本仓库自有发布标签使用 v<基础版本>-hao.<序号>，例如 v0.2.4-hao.8。历史的 -klno.N 标签保留但不重命名，后续不再创建新的 -klno.N 标签；KlN 的源 Release tag 仍保持其原名。序号在同一基础版本内递增，基础版本变化时从 .1 开始。

## 发布生命周期约定

- 实现及配置以 `docs/conventions/release-lifecycle.md` 为准。只有已手动合入 main 的 PR 才自动发版；未合并 PR 只检查、不预发布、不占产品版本号。
- main 必须通过 PR 修改，七项必需检查通过且分支同步后统一使用 Create a merge commit；禁止直接推送、强推、删除和机器人绕过。上游同步保留提交祖先关系，不使用 squash/rebase 合并。
- 发布固定实际 merge SHA、完整 tree、PR 和产物摘要。基础版本来自合并源码 VERSION，同步 PR 还须匹配 KlN tag；同基础版本递增 hao.N，新基础版本从 .1 开始。
- 六项应用 CI/安全检查必须在实际构建 SHA 上通过，发布控制器回归在受信任控制器 SHA 上执行。构建任务只读，发布凭据只交给不执行候选代码的独立任务。
- 完整发布包含五平台安装包、checksums 和 GHCR 双架构镜像；Docker Hub 配置双 secrets 后启用。禁止用简化模式省略产物。稳定渠道成功后正式公开 Release，失败可补齐。
- VERSION 只在构建工作区注入完整发布版本，不直接回写 main。已发布 tag 不移动、已有版本产物不覆盖。相同 PR 重试沿用版本，冻结 bundle 丢失时停止，不擅自重建或换号。
- 合并事件、每 15 分钟补偿和手动恢复共用串行状态机，不取消运行中的发布。旧版本不得回退稳定渠道；只显式认领符合校验的空历史 Release。
- 上游同步只能创建审查 PR，不得强推 main、直接打产品 tag 或替维护者合并。首次启用需要将发布 workflow、脚本和约定通过 PR 合入 main。

## 本地 Paseo workspace 工具链与共享缓存

- 工具链和缓存安装在创建 workspace 的 Paseo daemon 所在机器；如果客户端连接的是远程或 Docker daemon，应在对应 daemon 环境重复配置，不要只在当前终端安装。
- 本机 WSL 已安装 Go `1.27.0`、Node `24.21.0` 和 pnpm `9.15.9`。Go 通过用户目录安装，命令由 `~/.local/bin` 提供；不要使用系统级 `/usr/local` 安装覆盖它们。
- Go 与 pnpm 的跨 workspace 缓存固定在 workspace 之外：
  - `GOCACHE=~/.cache/sub2api/go-build`
  - `GOMODCACHE=~/.cache/sub2api/go-mod`
  - `GOPATH=~/.cache/sub2api/go`
  - `GOBIN=~/.local/bin`
  - pnpm store：`~/.cache/sub2api/pnpm-store`
- 新 workspace 第一次准备依赖时执行：

  ```bash
  go -C backend mod download
  pnpm --dir frontend install --frozen-lockfile --prefer-offline
  ```

- 后续 workspace 会复用 Go 模块/编译缓存和 pnpm store；`frontend/node_modules`、`frontend/dist` 以及 Vite/TypeScript 的工作目录仍必须各 workspace 独立，不要跨 workspace 软链接或复制 `node_modules`。
- `frontend/pnpm-lock.yaml` 变化后仍须重新执行 frozen install；不要改用 `npm install`，也不要随意执行 `pnpm store prune` 清理共享缓存。
- 检查当前 daemon 和工具链：

  ```bash
  paseo daemon status --json
  go version
  pnpm --version
  go env GOCACHE GOMODCACHE GOPATH GOBIN
  pnpm store path
  ```

- 如果需要让 Paseo 创建 workspace 后自动准备依赖，可在基础分支提交 `paseo.json`，加入以下配置；缓存环境变量应继续由 daemon 用户环境提供，不要写入 workspace 临时目录：

  ```json
  {
    "worktree": {
      "setup": [
        "go -C backend mod download",
        "pnpm --dir frontend install --frozen-lockfile --prefer-offline"
      ]
    }
  }
  ```
