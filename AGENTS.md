# AGENTS

本仓库是 Wei-Shaw/sub2api 的 fork；`klno` 分支只维护 docs/conventions/ 里描述的补丁。

- 写码前读 `docs/conventions/codex-outbound-identity.md`；续接工作先看 `docs/dev-journal.md` 最新条目和 `docs/tasks/`。
- 非平凡改动后回写 journal；产生新约定就改写 conventions（不是追加）。
- 创建或修改 Git commit、Pull Request 标题或正文时，必须使用 `chinese-commit-conventions` skill；type 保留英文，scope、subject 和 body 使用中文，并按该规范关联 Issue。
- 补丁必须小、可 rebase；协议事实以 openai/codex 源码为准。
- Git 远程约定：origin 指向个人仓库 luohao830/sub2api，upstream 指向原始上游 Wei-Shaw/sub2api，klno 指向直接父 Fork KlN-4096/sub2api。
- 日常更新先执行 git fetch --prune upstream klno origin。Wei-Shaw 的根上游更新通过 upstream 获取；KlN 的补丁更新以其正式 Release tag 为准，当前补丁线是 klno 分支，不把 klno/main 当作 Release 内容快照。
- KlN Release 同步由独立的 GitHub Actions 轮询完成：以最新正式 Release tag 创建 sync/kln-release/<tag> 分支并向 main 提交一个 PR。同一时间只保留一个 KlN 同步 PR；冲突留在 PR 中等待人工解决和审查，不自动合并或强制推送。
- 本仓库自有发布标签使用 v<基础版本>-hao.<序号>，例如 v0.2.4-hao.8。历史的 -klno.N 标签保留但不重命名，后续不再创建新的 -klno.N 标签；KlN 的源 Release tag 仍保持其原名。序号在同一基础版本内递增，基础版本变化时从 .1 开始。

## 发布生命周期约定

- 实现及配置以 `docs/conventions/release-lifecycle.md` 为准。统一 Release workflow 在默认分支运行，按 PR、源码 SHA、完整 tree 和构建产物摘要管理发布；禁止移动已发布 tag、替换已发布版本产物或从 tag push 绕过发布门槛。
- 同仓库非草稿 PR 自动测试并预发布；外部 Fork PR 在合并后才构建发布。六项既有 CI/安全检查必须在实际构建提交上通过；编译任务只读，发布凭据只交给不执行候选代码的独立任务。
- 合并结果与候选 tree 一致时提升原候选；有差异时自动分配新的 `-hao.N` 并从合并结果重新测试、构建。关闭但未合并不提升，旧候选不覆盖较新合并结果；序号不足以继续递增发布时重新分配版本。
- 预发布只写版本专属 GHCR 标签和下载附件；正式发布阶段才同步 Docker Hub、GHCR 的 latest/major/minor、GitHub Latest 和默认分支 VERSION。正式状态不代表所有后续步骤已完成，必须可重试补齐。
- 合并事件与每 15 分钟补偿扫描共用逻辑，覆盖手动合并、构建晚完成、GITHUB_TOKEN 事件抑制和运行中断。全仓发布串行且不取消运行中的任务；构建失败轮转重试，冻结的产物在有效期内复用，过期则分配新版本。
- 上游同步只能创建审查 PR，不得强推 main、重建 main 或直接打产品标签发布。自动化不替维护者合并产品 PR；首次生效需要把工作流与脚本合入 main，并满足部署文档中的 Actions/Token 权限要求。
