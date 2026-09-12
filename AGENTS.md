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
