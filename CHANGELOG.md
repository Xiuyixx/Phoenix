# Changelog

## 0.1.0 - 2026-11-23

- 0.1.0：从 Lamplighter 干净复刻，命名全面清理。
- 模块与插件 id 改为 `github.com/Xiuyixx/Phoenix` / `phoenix`，插件注册元数据（名称 Phoenix、作者 Xiuyixx、仓库 https://github.com/Xiuyixx/Phoenix）与 release 检查地址同步改为 Xiuyixx/Phoenix；SDK 升到 CLIProxyAPI/v8 v8.0.36。
- 版本变量改为 `pluginVersion`，由 `-X main.pluginVersion=...` 注入，默认值、`VERSION` 文件与本 CHANGELOG 三处同步校验（`make check-version`）。
- MCP 密钥前缀由 `llmcp_` 改为 `phxmcp_`；存储键（localStorage / sessionStorage）、插件数据目录（`plugins/data/phoenix`）、配置键与页面文案中的旧名称一并改为 Phoenix。
- 去掉 logo：页眉 logo 图片与相关 CSS、页面 JS 的 logo 镜像回退逻辑已删除，`assets/logo.png` 已删除；Bark 默认图标为空，不再指向旧 logo 地址。
- 构建与发布脚手架改为 Phoenix 版：`Makefile`（`make build` / `make check-version` / `make release`）与 `.github/workflows/release.yml`（Linux amd64/arm64、Windows amd64，产物 `phoenix_<版本>_<系统>_<架构>.zip`）；与 Makefile 重复的 `scripts/build.sh` 与 `scripts/ci-build.sh` 已删除，Lamplighter 专属的 `.github/ISSUE_TEMPLATE/new-service.yml` 与 `README_EN.md` 已删除。
- 底本改名完成后，再移植 Phoenix 差异功能（见下）。
- 移植 Phoenix 差异（源自已验证的 v0.0.20 树，按新底本适配）：
  - 自动/手动重置全套（`internal/engine/reset.go`）：周窗口耗尽且持有 reset credit 时自动消耗最早过期的一张并清除 CPA 本地冷却，宽限后自动点火；手动重置（`POST /reset`、每行“立即重置”按钮）不点火；5 分钟防重复，consume 只发一次（`Content-Type: application/json`）；credit 列表解析失败重试、无到期时间视为永不过期。
  - Telegram 通知渠道（`internal/notify/telegram.go`）：Bot Token + 分号分隔的多接收 ID，扇出顺序 Bark → Telegram → Webhook；三个渠道都没配才关闭通知；设置无效记 `lastNotifyErr` 进面板横幅；测试推送分渠道回显。
  - Webhook 与多 provider（codex/claude/antigravity/kimi/xai）及账号管理（拖动排序、名称、权重）按底本保留并经测试验证。
  - v0.0.18 通知文案（`internal/notify/phoenix.go`）：中文三组表情标题与账号（邮箱）/ 模型 / 5H / 7D / 状态 / 时间逐行字段，13 种示例逐字断言；触发与去重逻辑不动底本。
  - Did Codex Reset 经 `resetFeedSender` 适配器走通知扇出，发送失败仍标记已见，不再每轮重复播报。
  - 点火 fallback 链（`internal/providers/provider.go` 的 `Candidates`）：已配模型 → 列表候选 → `gpt-5-codex` 兜底（仅当它在列表中时），被拒两次即停；宿主 `model.execute` 错误详情（code/message/HTTP 状态）透传到面板。
  - 新增 `POST /check-models-key` 路由，页面经插件校验模型 key，不再直连 CPA `/v1/models`；Bot Token 保存修复（仅 `models_api_key` 走管理密钥拒绝与 checkKey、空 token 视为保持不变、密码框 `autocomplete="new-password"`、保存失败带具体原因）。
  - 默认值：提醒阈值 `20` / `10` / `5`，恢复模式 5 小时 `after_exhausted` / 7 天 `all`，重置提醒 5 小时 `off` / 7 天 `has_remaining`，额度图表默认不勾选；Release 自动检查默认关闭，手动 `POST /check-update` 仍可达。
  - 配置兼容：旧键只读映射（`alerts.thresholds` 仅当三个新阈值均未写时映射；`ignition.model` 仅当 `providers.codex.model` 未写时映射，已配的 `gpt-6-luna` 优先；`providers` 嵌套映射直读）；`parseYAML` 顶层键在 section 之后仍可读（回归测试用服务器真实配置形状验证）；`loadCreds` 跳过空 `auth_index`；无 5 小时窗口的组不进目标列表（后端过滤 + 页面空值保护）；Status 不下发 `config_token`，`models_api_key` 恒为空另给 `models_api_key_set`。
  - 面板 5 处 Phoenix 差异：立即操作卡名、每行立即重置按钮、重置设置区、Telegram 设置区、Webhook 设置区；无 logo；其余照底本。
