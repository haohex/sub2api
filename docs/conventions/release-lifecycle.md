# 合并 PR 后发布

产品仓库是 `haohex/sub2api`，默认分支是 `main`。唯一产品发布入口为
`.github/workflows/release.yml`，控制器为 `tools/release/reconcile.py`。
上游同步和自有修改共用此入口。只有维护者手动合并 PR 才产生新的产品版本，
不要手动创建产品 tag 或空 Release，也不要从 tag push 触发另一套发布。

## 两条开发路径

- KlN：每小时第 23 分钟检查 `KlN-4096/sub2api` 最新正式 Release，以其 tag
  建立 `sync/kln-release/<tag>` 审查 PR，创建前自动 merge 最新 main。
  同一时间只保留一个同步 PR；已有 PR 时每轮尝试将最新 main 合入该分支，
  保留人工提交，只做普通 push。无更新时不生成提交；冲突时撤销本次 merge，
  保留原分支并输出警告，由维护者在同步 PR 处理冲突，保留本仓库工作流、
  发布脚本和约定，并核对 VERSION 的基础版本。合并脚本使用切换同步分支前
  保存的 main 版本。自动更新后重新运行检查，最终仍由维护者手动合并 PR。
- 自有修改：从 main 创建功能或修复分支，提交 PR。包括工作流修改在内，
  所有改动都经必需检查后手动合并。

统一使用 GitHub **Create a merge commit**。上游同步依赖祖先关系判断是否已包含
某个 Release，不能用 squash/rebase 合并破坏这一关系。解决冲突时也应把 main
merge 到同步分支，提交冲突解决结果后重新检查。

Wei-Shaw 的 `sync-upstream.yml` 保留为独立的手动维护工具；其审查 PR 如果合入
main，也遵循相同发布门禁。日常 KlN 同步不依赖它，不自动混入第二条上游补丁线。

## main 保护与检查

main 必须通过 PR 修改；管理员同样受保护，不给发布机器人直接推送的例外。
禁止强推和删除，不要求线性历史，不开启自动合并。保护配置保存在
`tools/release/main-protection.json`，可用 GitHub API 重放。个人维护可将必需批准数设为 0，
但仍必须手动合并 PR，不能绕过检查。

必需状态来自 GitHub Actions（App ID 15368）：`release-automation`、`shell`、
`test`、`frontend`、`golangci-lint`、`backend-security`、`frontend-security`。
合并前要求分支与 main 同步。`release-automation` 同时检查发布入口仍存在，
避免上游同步再次删除发布工作流而 CI 假通过；同步 PR 的基础版本也在合并前校验。

合并后发布再次调用 reusable CI/Security workflow：六项应用检查在固定的合并 SHA
上执行；发布控制器的回归测试在受信任控制器 SHA 上执行。这允许用当前恢复工具
检查没有新发布控制器的历史源码，例如 `.18`，同时仍完整检查其应用源码。

## 版本与源码身份

PR 未合并时不创建 Release、不占版本号。合并后固定 PR 号、`merge_commit_sha`、
完整 tree 和合并时间，后续不跟随 main 移动，也不复用仅 tree 相同的其他提交。

基础版本来自该合并 SHA 的 `backend/cmd/server/VERSION`。源码 VERSION 只维护基础
版本，如 `0.2.4`；构建工作区将它替换为 `0.2.4-hao.19`，不提交回 main。
同步 PR 的分支 tag 必须与合并源码基础版本一致，冲突解决保留了旧基础版本时拒绝发版。

- 同一基础版本：检查所有远程 tag、Release 和草稿中的 canonical tag，取最大 N + 1。
- `v0.2.4-klno.5` 仍使用 `v0.2.4-hao.N` 递增。
- 首次合入 `v0.2.5-klno.1` 使用 `v0.2.5-hao.1`；已有该基础版本时继续递增。
- 同一合并 PR 正常只分配一个版本；构建/发布重试不重新分配，不移动已存在的 tag。
- Release 正文中的状态标记接受 LF、CRLF 和两者混用；存在标记但格式损坏时按异常状态处理，
  不把它当作未受管的历史 Release。
- 历史解析故障已为同一 PR 分配多个版本时，逐一核对其 SHA、tree 与真实合并结果；
  全部一致则保留所有预约和产物，未完成版本按原 bundle 重试，已就绪版本不再构建。
  任一源码身份不一致仍拒绝推进；正式渠道继续按合并时间及版本号选择，不按补发时间回退。

先用草稿 Release 持久化版本预约。检查和构建成功后才创建公开 tag，避免仅有 tag
而无构建结果。GitHub 草稿可能显示 `untagged-*`，控制器始终保存和使用 canonical tag。

## 构建、发布和权限

1. 合并事件触发；每 15 分钟补偿扫描及手动运行共用状态机。每轮处理一个 PR，
   优先按合并时间分配新版本，失败重试轮转；全仓串行且不取消运行中的发布。
2. 使用默认分支受信任控制器；源码 runner 没有发布凭据且只有只读权限。
   两个 reusable workflow 全部成功后，才构建前端、五个平台安装包和双架构 OCI 镜像。
3. Actions artifact 保存 bundle（90 天）。计划阶段记录 build run；即使上传 artifact
   后、发布前中断，后续也能找回原包。发布前冻结文件 SHA-256、bundle run 和镜像 digest。
4. 独立发布任务核对身份、完整文件集合、checksums、OCI 平台和摘要，写入版本专属 GHCR
   标签并上传附件。已有同版本内容只能相同，绝不覆盖不同内容。
5. 补齐可选 Docker Hub 的每个版本标签；复核 tag、附件摘要和镜像平台后，按基础版本、
   合并时间和版本号选择稳定渠道。延迟完成的老版本可以发布，但不能回退 latest。
6. 同步 GHCR/Docker Hub 的 latest、major、minor 后才把新草稿正式公开并设置 GitHub Latest。
   服务之间没有事务，网络中断可能留下部分更新；下轮继续补齐，不因 Release 已正式而跳过。

完整发布必须包含：Linux amd64/arm64、macOS amd64/arm64、Windows amd64 五个压缩包，
以及 `checksums.txt`。GHCR 地址为 `ghcr.io/haohex/sub2api`，版本标签不含前导 v，
例如 `0.2.4-hao.19`；镜像必须同时包含 linux/amd64、linux/arm64。
旧 `SIMPLE_RELEASE` 变量不再生效，不能静默省略下载附件或 arm64。

tag 优先用原生 GITHUB_TOKEN 创建，避免触发旧源码的 tag-push 发布工作流。
仅当候选和受信任控制器的 workflows tree 完全一致时，才允许因权限问题回退
到 RELEASE_TOKEN。恢复已有 tag 不创建、不移动 tag。

## GitHub 设置

- Actions 开启即可，默认权限可保持只读，工作流逐 job 申请权限。
- `RELEASE_TOKEN` 用于自动同步 PR 和必要的 Release 写操作。使用本仓库专用 PAT
  或 GitHub App 凭据，具备 Contents/PR 写权限、Actions 读权限，修改 workflows 时
  还需对应权限。同步明确要求此凭据，避免退回 GITHUB_TOKEN 后 PR 检查需额外批准或未触发。
- GHCR 使用 job 的 GITHUB_TOKEN。已有 package 须允许本仓库 Actions 写入；面向公开
  安装的 package 应设为 public，并做匿名拉取验收。
- Docker Hub 为可选镜像副本：设置 `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`
  两个 secrets 后发布到 `<username>/sub2api`。必须同时设置或同时不设置，缺一个会报错。
  未配置时完整发布仍包含 GHCR 双架构镜像及所有安装包。
- 不需要 VERSION 写回权限、分支 bypass 或 Telegram 通知凭据。

## 激活与恢复

新流程必须经本次 PR 手动 merge 到 main 才生效。首次激活边界是
`tools/release/merged-only.json` 首次进入 main 第一父链的提交时间，包含激活 PR 本身，
不为更早的历史合并批量发版。schema 1 的旧预发布记录不再续建或自动提升。

普通失败重跑 Release（不填输入）或等待扫描即可。没有冻结发布副作用的失败构建
可以在原预约版本重建；已冻结 bundle 若过期/丢失则报错，需要恢复原始 bundle，
不能用重建或递增版本掩盖同版本来源问题。

首次激活会按 `merged-only.json` 中审计过的仓库、tag、SHA 和 PR 自动认领空的 `.18`，
补发成功后继续发布新的合并结果；若已有附件则不自动接管。

其他空历史 Release 可用 `workflow_dispatch` 的 `recover_tag` 显式认领：

```bash
gh workflow run release.yml --repo haohex/sub2api --ref main \
  -f recover_tag=v0.2.4-hao.18
```

仅接受已有 tag + Release，tag 必须恰好对应一个已合并 PR 的真实 merge SHA，
基础版本一致且尚无附件。已有受管状态会继续重试；已受旧控制器管理的历史版本
不自动接管。首次认领后的恢复不受激活日期限制，失败后定时扫描继续完成。
`.18` 固定使用 `0f4136790c2bbb70b44e6ea48d9a5312f7efc702`（PR #11），不得换成新 main。
如果已有版本镜像摘要与本次构建不同，停止并核对已有产物，不能覆盖。

最终验收：下载全部六个附件并运行 checksum 验证；检查二进制版本与 tag 一致；
匿名拉取 GHCR 两种架构；核验版本标签与 latest 的 digest；配置 Docker Hub 时同样核验。
本地验证命令：`python3 -m unittest discover -s tools/release -p 'test_*.py' -v`，以及 `actionlint`。
