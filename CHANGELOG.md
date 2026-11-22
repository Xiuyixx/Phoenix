# Changelog

## 0.0.2 - 2026-11-22

- 修复 GitHub Actions Release 的 Windows 打包方式，改用 PowerShell 原生压缩和 SHA256 校验。
- 移除当前 GitHub 账户无法稳定调度的 macOS arm64 runner，避免矩阵任务卡住 Release。
- 统一 Release 构建的跨平台 shell 处理。
- 进一步避免 macOS runner 阻塞公开 Release；Linux 与 Windows 构建先行发布。

## 0.0.1 - 2026-11-22

- 创建 Phoenix CLIProxyAPI 原生插件项目。
- 增加管理页面、Telegram 配置、额度状态和“立即重置 / 立即点火”操作入口。
- 增加参考项目致谢与部署说明。
- 增加跨平台 Release 构建与 checksums 发布流程。
