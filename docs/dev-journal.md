# 开发日志

## 2026-09-12：PR #8 合并 KlN v0.2.4-klno.4

- 将主线 `7bcff29ef` 合入上游同步分支，解决 Messages 桥、账号编辑弹窗和指纹验收器的冲突；重复功能按上游实现收敛。
- 时区统一采用上游 `codex_wire_timezone`：仅在设备指纹与实验收敛同时启用时投影明确分类的环境内容，日期按消息 `create_time` 换算；自动解析复用额度刷新，按代理标识隔离缓存。
- 删除旧 `codex_request_timezone` 解析器、请求钩子、UI 开关及专属测试，保留上游时区/历史日期/WS 回归与完整探针。存量旧键不再生效；固定时区使用 `codex_wire_timezone`，未配置时由上游自动解析。旧 alpha/search 搜索位置改写随旧实现移除。
- 保留本地 5 小时额度、OAuth 额度跟随重置、fork 更新源及 PR 发布生命周期；WS 两条路径各保留一次周额度观察。
- 验证：待完成本轮前后端检查后记录结果。

## 2026-09-12：修复上线后的上游识别与草稿恢复

- 实际阻塞：KlN 同步用 jq 的 `// true` 把布尔 false 替换为 true，正式 Release 被误拒绝；首次 Release 构建使用了草稿 API 返回的 `untagged-*`，上传和后续扫描均失败。
- 修复：显式判断 draft/prerelease 均为 false；持久化 canonical tag，PATCH 总是携带 tag_name，构建不使用草稿别名；附件按 Release ID 上传。旧草稿只在标题候选版本与实际 tag SHA 匹配后恢复；不可恢复的草稿隔离告警，已发布记录异常仍拒绝推进正式渠道。
- 验证：31 项回归测试及 actionlint 通过，覆盖实际 untagged 草稿形状、PATCH 别名响应、旧草稿错误 SHA、附件上传 ID 和上游布尔判定。

## 2026-09-12：PR 预发布与合并后发布自动化

- 背景：PR #5 未合并即由 tag/手动触发正式 Release；重复发布被取消后，PR 上出现两个取消状态。
- 变更：统一默认分支控制器，测试/构建与发布权限隔离；PR 候选预发布，合并 tree 校验，产物冻结、补偿扫描、版本及正式渠道防回退，完整/简化构建均隔离稳定标签。
- 上游同步：移除自动强推 main 和绕过 PR 的产品发布；改为审查 PR。
- 配置、权限、恢复及首次线上验证说明见 `docs/conventions/release-lifecycle.md`。
- 当前分支原先没有 `docs/dev-journal.md`、`docs/tasks/` 或 `docs/conventions/codex-outbound-identity.md`。本次建立发布约定及日志，不臆造缺失的身份协议约定。
- 验证：24 项状态机/打包/API 契约测试、actionlint、真实 GitHub API 只读检查通过；Docker Buildx 双架构 OCI 导出及 skopeo 保持摘要复制通过。补充原生 tag 令牌及旧 workflow 防回退测试。完整应用的首次线上构建/发布需本次改动进入 main 后验证。
