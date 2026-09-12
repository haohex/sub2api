# PR 发布自动化

统一入口是 `.github/workflows/release.yml`，控制器为 `tools/release/reconcile.py`。
只执行默认分支的控制器；候选源码在无写权限、无发布凭据的独立 runner 编译。

## 生命周期

1. 扫描目标为 main 的同仓库非草稿 PR，或自动化启用后已经合并的 PR。外部 Fork 仅在合并后纳入。
2. 为实际源码提交分配 `v<基础版本>-hao.<序号>`。基础版本来自该提交的 VERSION，序号同时检查远程 tag 和草稿 Release，不能复用已占用版本。
3. 草稿 Release 保留 PR、SHA、完整 tree、构建模式及重试状态。复用 CI 和 Security Scan，在确定的源码 SHA 上执行 shell、test、frontend、golangci-lint、backend-security、frontend-security；任何失败均不能发布。
4. 构建前端、五种平台安装包和 linux/amd64、linux/arm64 OCI 镜像；`SIMPLE_RELEASE=true` 时只构建 amd64 镜像。写入的 VERSION 仅存在于构建工作区。
5. 上传 Actions 构建 bundle（90 天），在发布副作用前冻结 run ID 和每个文件的 SHA-256。后续重试下载同一 bundle，不能以新构建覆盖旧版本。过期且未完成的 bundle 标记废弃，自动分配新版本。
6. 发布版本专属 GHCR 镜像和附件，Release 标为 prerelease。构建期间 PR 有新提交时，旧版本可保留为历史预发布，但不提升旧源码。
7. PR 合并后读取真正的 merge_commit_sha。无论 merge、squash 还是 rebase，按完整 tree 判断内容一致性；相同则提升原候选，不同则自动新建版本并重新测试构建。tag 永不移动。
8. 按基础版本、合并时间和版本序号选定正式渠道，阻止延迟完成的旧 PR 回退代码；如果较新合并结果的候选序号已经落后，分配新的更高序号。正式渠道从冻结的镜像 digest 同步，不依赖可变镜像标签。
9. 正式阶段才写入 GHCR latest/major/minor、可选 Docker Hub 版本和渠道标签、默认分支 VERSION、GitHub Latest。各 API 不支持跨服务事务，可能短暂部分完成；每次扫描补齐，不因 Release 已正式发布而跳过。

发布事件、CI 完成事件、手动运行和每 15 分钟 schedule 都扫描相同的持久状态。每轮只构建一个候选，按最近尝试时间轮转，避免失败 PR 阻塞其他 PR。GitHub 可能延迟 cron 或替换排队中的 run；不能保证精确 15 分钟，但不依赖单个事件的存活。

首次激活边界是控制器首次加入 Git 历史的提交时间，避免把所有历史已合并 PR 重新发布。未合并的现有 PR 会被纳入。未携带控制器元数据的历史 Release 不自动认领、替换产物或重写渠道；历史误发布需要单独核对并修正。

## 必要设置与部署

- 将本次工作流、脚本及约定一起合入 main；仅在功能分支中存在不会启用 schedule。网页手动合并就是首次激活方式，无需再手动点发布。
- Actions 必须开启。仓库当前已经开启且允许所有 actions；工作流显式申请所需权限，默认只读权限可以保留。
- 上游同步创建 PR 需要 Settings → Actions → General → Workflow permissions → **Allow GitHub Actions to create and approve pull requests**。当前该选项未开启。它允许创建 PR，不代表自动化会批准或合并 PR。
- 可选仓库 secret `RELEASE_TOKEN`：当 GitHub 拒绝 GITHUB_TOKEN 为修改 workflow 的候选创建 tag/Release，或者分支保护阻止 VERSION 回写时需要配置。建议使用仅限本仓库的专用凭据，Contents: read/write、Pull requests: read/write、Actions: read、Workflows: read/write；使用 GitHub App 时也要处理 installation token 自动续期。不要把令牌明文写进仓库或聊天。
- 开启 main 分支保护时，VERSION 自动回写身份必须具有适当的 bypass 权限，否则发布会持续报告 VERSION 同步失败。该身份只用于受信任控制器，构建任务仍只使用只读 GITHUB_TOKEN。不要为普通贡献者放开 bypass。
- GHCR 使用 GITHUB_TOKEN；若已有 package 权限未继承仓库权限，在 package 的 Manage Actions access 中为本仓库授予写入权限。

可选设置：仓库变量 `SIMPLE_RELEASE=true`（默认 false）；Docker Hub 的 `DOCKERHUB_USERNAME`、`DOCKERHUB_TOKEN` 两个 secrets。未配置 Docker Hub 时只有 GHCR，候选不会更新 Docker Hub。更改构建模式只影响新分配的候选，已有候选模式冻结。

配置 `TELEGRAM_BOT_TOKEN`、`TELEGRAM_CHAT_ID` 时，完整模式仅在正式渠道全部同步成功后通知一次；简化模式和预发布不通知。通知成功后持久化标记，失败会重试；若发送成功后状态保存失败，可能重复通知（Telegram 不提供该场景的幂等键）。Docker Hub 描述更新不属于版本发布事务，统一流程不再在每次发版时覆盖描述。

## 恢复与检查

- 普通中断、网络故障、合并早于构建完成：等待下一轮扫描，也可手动运行 Release（无需 tag 参数）。不要删除已占用的 tag 或重传不同附件。
- 构建失败：修正对应 PR 或测试问题后会自动构建最新源码；旧 PR 的重试不会阻塞其他候选。
- 已正式发布但渠道不完整：重跑会再次校验 PR/tag 并补齐渠道和 VERSION；修正凭据后无需重建。
- 关闭但不合并：候选保持预发布；不自动删除用户可能已下载的版本。
- 一项构建产物被移动、篡改或已有附件摘要不符：停止该版本发布并明确报错，不覆盖以掩盖来源差异。
- 自动化测试：`python3 -m unittest discover -s tools/release -p 'test_*.py' -v`；工作流使用 actionlint 验证。首次线上完整构建还要核验下载附件、GHCR 两种架构、PR 合并前后状态以及正式渠道一致性。

## 上游同步

KlN 仍以正式 Release tag 建立单个审查 PR。Wei-Shaw 同步保留本地 rebase、定向测试和身份漂移检查，但只推送 `sync/wei-release/<tag>` 审查分支，不再强推 klno/main 或直接发布。产品 main 不再作为可随意重建的 cron 宿主。
