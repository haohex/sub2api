# 开发日志

## 2026-09-16：解决 KlN v0.2.5-klno.2 同步冲突（PR #21）

- 将最新 main 合入同步分支，保留上游 Release 与 main 的祖先关系；16 个显式冲突按新版应用实现与本仓库发布约定分别处理。
- 保留上游 CPR、图片端点分流、WebSocket 常驻读循环及执行作用域测试；移除自动合并意外重复添加的 CreateCodexBackendReqClient。
- 发布工作流、打包配置、安装渠道及 AGENTS 保留 main；保留 CLA 删除与 gRPC v1.83.2 安全修复。VERSION 为 0.2.5，与源 tag 基础版本一致。
- 验证：45 项发布回归、基础版本检查、actionlint 1.7.12、安装脚本语法与 GitHub token 回归通过；后端定向回归及 PR 必需检查继续执行。
- 身份协议约定文件仍缺失，本次按两侧现有实现解决重叠，不引入新的协议约定。

## 2026-09-16：修复 PR #22 的后端安全检查

- 既有 gRPC v1.82.1 被漏洞扫描检出 GO-2026-6348 和 GO-2026-6443；与 CLA 删除无关。官方漏洞库显示 1.83 分支需要 v1.83.2 才同时覆盖两项修复。
- 升级 gRPC 至 v1.83.2，使用 Go 模块工具调整必需的传递依赖并整理 go.sum；保留原有安全检查门槛。
- 验证：Go 1.27 容器中 govulncheck v1.8.0 扫描可达漏洞为 0；插件相关服务回归测试通过，git diff --check 通过。完整回归由 PR CI 执行。

## 2026-09-16：停用并移除 Fork CLA 流程（关联 PR #21）

- 原因：KlN Release 同步 PR 携带多位上游作者的提交，CLA Assistant 将他们视为本 Fork 的待签署贡献者，批量提及并引发不必要的签署回复。
- 已在线停用 CLA Assistant；检查时没有未完成运行，main 的必需检查不包含 CLA，仓库没有 CLA 专用 Actions secret 或 variable。
- 删除 CLA 工作流及协议文档，一并移除签名检查、评论触发重查和合并后锁定讨论逻辑；贡献约定明确后续同步保留删除，不重新引入该流程。LICENSE 和版权声明不变。
- 已删除远端 `cla-signatures` 分支；API 回读确认分支不存在、工作流状态为 `disabled_manually`。历史 PR 评论不修改。
- 验证：`git diff --check` 通过；全仓 CLA 引用仅剩开发日志及停用约定。本次只删除工作流和文档，不涉及应用代码，未运行应用测试。
- `docs/conventions/codex-outbound-identity.md` 仍缺失，本次仅清理贡献自动化，不涉及协议代码。

## 2026-09-15：KlN 同步分支自动合入 main（关联 PR #18、Issue #13）

- 根因：从上游 tag 建分支且已有同步 PR 时直接跳过，与 main 的 strict 更新要求组合后，每次都需要手动 merge main。
- 新同步分支创建前自动合入 main；已有同步 PR 每次轮询更新其分支，保留人工提交，不强推。重试接受包含源 tag 的已有分支，避免首次 push 成功但 PR 创建失败后无法恢复。
- 合并冲突撤销并警告，原分支继续保留供人工解决；其他错误失败退出。使用切换分支前保存的受信任合并脚本。自动提交关联发布流程 Issue #13，最终 PR 仍手动合并。
- 验证：39 项发布回归通过，新增真实 Git 测试覆盖合并祖先、幂等、后续 main 更新、冲突恢复及非冲突错误；actionlint 1.7.12、bash -n 和 git diff --check 通过。身份协议约定仍缺失，本次不涉及协议修改。
## 2026-09-15：修复 PR #18 发布被历史重复版本阻塞

- 线上运行 `34954036763` 由 PR #18 合并触发，却补发 PR #17 为 `.25`；`.23`、`.24` 正文的 CRLF 导致旧正则无法识别已有状态。
- 状态标记兼容 LF/CRLF 混用，损坏标记显式报错；同一 PR 多条记录逐一核对 merge SHA/tree 后保留并继续原预约，不再因重复数量阻塞后续 PR，不移动 tag 或覆盖冻结产物。
- 更新发布约定，覆盖重复记录恢复和稳定渠道按合并时间排序；身份协议约定文件仍缺失，本次仅修改发布控制器。
- 验证：42 项发布测试通过，新增换行解析、损坏标记、重复版本后继续分配、源码冲突、冻结 bundle 复用及 Latest 排序回归。actionlint、git diff --check 通过；用本次排查抓取的 `.23/.24/.25` 线上正文验证，均正确识别为 PR #17。

## 2026-09-13：恢复合并 PR 后的完整发布（Issue #13）

- 根因：`4feaf5457` 删除 Release workflow，`.18` 仅创建 tag/Release，没有构建安装包或推送镜像；旧约定与实际流程矛盾。
- 改为仅已合并 PR 发布：固定 merge SHA 与完整 tree，在草稿预约一次 hao 版本；无未合并预发布、不直接回写 main VERSION。KlN 同步和自有修改共用门禁，基础版本升级重置序号，冲突保留旧 VERSION 在合并前及发布时均拒绝。
- 恢复完整五平台安装包、checksums 和 GHCR 双架构 OCI；取消简化模式。保留冻结摘要、找回中断前 artifact、原版本重试和稳定渠道防回退；公开 Release 前复核所有附件和镜像。Docker Hub 配齐双 secrets 后启用，用户已选择先用 GHCR。
- 新增第一父链激活边界及真实 Git 回归，避免重发历史 PR；首次生效自动按固定 SHA `0f4136790` / PR #11 认领空 `.18`，提供手动 recover_tag 恢复入口。
- main 七项必需检查、strict 更新、PR 要求、管理员保护和禁止强推/删除已通过 GitHub API 配置并回读；只允许 merge commit，不启用自动合并。配置模板纳入仓库。Wei-Shaw 同步改为仅手动，日常保留 KlN 单一轮询来源。
- 验证：36 项发布/打包/真实 Git 激活测试通过，actionlint 1.7.12 及 git diff --check 通过；使用真实 GitHub GET API 和本地模拟写入演练，准确选择 `.18` 原 SHA，无远端变更。
- 首次线上验收待本 PR 手动合并：检查 `.18` 补发、本 PR 新版本、六附件 checksum、GHCR 双架构与 latest。跟进见 `docs/tasks/release-rollout.md`。身份协议约定文件仍缺失，本次未修改应用协议。

## 2026-09-12：修复 OpenAI OAuth 周额度 reset_at 秒级漂移（Issue #9）

- 参考 `LuckyKuang/sub2api-plus` 的 `v0.2.4+custom.002`，将周重置判定收敛为：上一窗口必须已结束，候选 `reset_at` 至少前进半个 7 天窗口；同一原始值允许重试以补齐遗漏的分组基线。
- 移除服务层进程内时间去重，统一由数据库事务判定窗口，避免多实例、重启和重复观测造成状态分叉；保留原始 `reset_at` 字段，不新增迁移。
- WebSocket 与 HTTP 快照仅接受明确的 10,080 分钟周窗口，补充秒级漂移、倒退、跨午夜、非周窗口及月度清零回归覆盖。
- 验证：完整 backend unit/integration、聚焦 OAuth 重置集成测试及 `golangci-lint v2.13.0` 均通过。

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

## 2026-09-13：上游同步改为仅创建审查 PR

- 按维护流程调整：KlN 正式 Release 由定时任务检测并创建唯一同步分支和 PR；不再自动创建 draft/pre-release、构建候选或推进正式渠道。
- 移除自动发布 workflow；同步 PR 合并后由维护者手动创建 GitHub Release。
