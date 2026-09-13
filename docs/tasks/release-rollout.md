# 合并后发布上线验收

关联 Issue #13。实现见 `docs/conventions/release-lifecycle.md`。

已完成：发布状态机和 workflow、main 保护、merge commit 限制、本地 36 项测试与 actionlint，真实 GitHub 只读 `.18` 恢复演练。

待维护者手动 merge 本次 PR 后：

1. 确认 Release 合并事件或下一次补偿扫描启动；新入口只在 main 生效。
2. `.18` 首次恢复必须固定 PR #11 / `0f4136790c2bbb70b44e6ea48d9a5312f7efc702`。
3. 新合并 PR 应分配 `v0.2.4-hao.19`（若期间有其他预约则顺延）。同 PR 重试不能占新号。
4. 两个版本均下载五个平台压缩包和 checksums，校验摘要与可执行文件版本。
5. 匿名拉取 GHCR，确认版本标签含 amd64/arm64，latest 指向新 PR 产物；补发 `.18` 晚完成也不得回退 latest。
6. Docker Hub 暂不配置；未来设置两个 secrets 后，补偿扫描自动补齐版本及渠道。

不由自动化替维护者合并 PR，不为启动 workflow 直接推 main 或移动产品 tag。
