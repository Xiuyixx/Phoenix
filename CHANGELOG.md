# Changelog

## 0.0.5 - 2026-11-23

- 修复 Phoenix 在 CLIProxyAPI 8.1.1 上启动即被拒绝的问题：注册信息改用类型化的 `pluginapi.Metadata`，不再用 snake_case 的 map 键（`github_repository` / `config_fields`），宿主按 Go 字段名解码时这两个字段会丢成空值，导致 `validPlugin` 判定失败并报 `plugin phoenix returned invalid metadata or no capabilities`。
- 增加回归测试 `TestRegistrationMetadataParses`，确保注册 JSON 能被宿主正确解析出名称、版本、作者、仓库地址和 capabilities。

## 0.0.4 - 2026-11-22

- 重新整理并扩充 README，补充功能边界、安装路径、平台匹配、配置、验证、升级和故障排查说明。
- 修正 Release 资产名称、SHA256 校验文件、当前支持平台和版本发布命令的文档描述。
- 明确当前版本中自动点火与主动额度查询的接入边界，避免安装后产生错误预期。

## 0.0.3 - 2026-11-22

- 修复 Release 矩阵：移除当前账户无法稳定调度的 macOS runner，避免 Linux/Windows 已成功却无法发布 GitHub Release。
- 当前 Release 自动构建 Linux amd64、Linux arm64 和 Windows amd64，并生成 SHA256 校验文件。

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
