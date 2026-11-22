# Changelog

## 0.0.8 - 2026-11-23

- 按 Lamplighter 全面复刻面板与功能（与 Phoenix 的区别只有四处：操作卡片为“立即操作”、点火表每行在立即点火前加一个独立的“立即重置”按钮、设置多一块重置相关设置、设置多一块 Telegram 设置）。
- 双窗口：额度解析与面板同时支持 5 小时窗口与 7 天窗口（之前只认周窗口）；凭证状态统一按 `auth_index` 键入，主动轮询替换整组、被动记录只更新已确认的窗口，不再出现同一凭证两张卡。
- 额度历史图表：采样持久化到插件数据目录的 JSONL 历史文件（默认保留 40 天），新增 `GET /history`（按范围分桶），面板加“额度变化”总览色带与详情曲线（含匀速参考线、预测线、重置标记、低阈值线、点火标记、数据表），支持窗口/范围/排序过滤。
- 配置持久化：设置保存改为页面调 CPA 管理接口 `PATCH /v0/management/plugins/phoenix/config`（每次带随机 `config_token`），CPA 写盘并重新下发后页面确认生效；不再是重启丢失的内存保存。通知基线、点火目标状态、凭证问题等运行时状态持久化到数据目录的 `state.json`（0600，原子写）。
- 提醒接线：阈值（默认 50/20/10，须从高到低）逐档通知一次，额度恢复通知分窗口模式（5h 默认用完后、7d 默认每次），重置前提醒（5h 提前 1 小时、7d 提前 1 天），账号错误通知受 `notify_errors` 控制，stale 冷却提示，凭证问题分级（停用/认证/限流，认证类需 3 次连续失败且跨 30 分钟才定性）并暂停自动点火。
- 点火加固：新增 `targets[]` 与点火目标表（当前重置/最近成功/模型/状态/操作）；调度按每日窗口与 5 小时窗口重置时间驱动，点火后多次复查确认出现固定未来 5 小时重置才算成功，瞬时失败退避重试、硬错误或多次失败熔断至次日开始、短 CPA 冷却等待不计失败；模型可从 CPA `/v1/models` 读取候选（最新 luna 优先），但已配置的点火模型始终优先，不会被覆盖。
- 面板重做：通知横幅（配置/列表/通知/模型错误）、账号卡片每窗口行（倒计时、来源 chip、新鲜度、冷却行、套餐行、每卡刷新）、30 秒自动刷新、同步 `POST /refresh`（返回新状态而非旧状态）、事件列表（推送标记、5 小时重置过滤、展开更多）、设置分区补齐（提醒规则、点火扩展参数、高级）、密钥脱敏（Bot Token 与模型列表 API key 只返回是否已设置，留空不修改）。手动重置与点火彻底独立：手动重置不再自动跟一点火，只有自动重置成功后才按延迟自动点火。
- 管理接口新增 `GET /history`；状态 payload 扩展（`accounts/targets/events/errors/last_poll/next_poll/utc_offset_seconds/config_applied_token`）；轮询失败按账号记录并展示，不再静默。
- 新增回归测试 22 个（共 30 个，含 `-race`）：双窗口解析、auth_index 统一、历史分桶、状态脱敏、config_token、阈值逐档通知、恢复判定、点火调度/确认/熔断、模型候选排序、凭证问题分级等。

## 0.0.7 - 2026-11-23

- 管理接口需要 CPA 管理密钥：页面按 Lamplighter 的做法，从管理中心 `localStorage`（`cli-proxy-auth`）读取已记住的密钥，取不到时弹出密钥输入框（只保存在当前标签页 `sessionStorage`），401 时提示重新输入；之前页面调管理接口不带密钥，会直接 401 导致状态刷不出来。

## 0.0.6 - 2026-11-23

- 修复管理页面空白：`handleManagement` 全部路由改回类型化的 `pluginapi.ManagementResponse`（`StatusCode` / `Headers` / `Body`），不再把 HTML 页或 JSON 包进 `okEnvelope` 当 result。之前宿主解码时报 `json: cannot unmarshal string into Go value of type pluginapi.ManagementResponse`，`/v0/resource/plugins/phoenix/page` 返回 502，页面空白；同时 `/status` 等接口的 Body 也为空，页面状态刷不出来。做法与 Lamplighter 的 `jsonResponse` 一致。
- 补齐此前注册了但没有实现的 `POST /refresh` 路由；管理接口未知路由返回 404、未初始化返回 503，而不是外层错误 envelope。
- 额度监控落地：主动轮询按 `poll_interval_seconds`（默认 300 秒，最小 60 秒）枚举 Codex 凭证并查询上游 `/wham/usage`，按窗口时长（约 604800 秒）识别周窗口，填充剩余百分比与重置时间；被动接收 CPA usage 记录，成功记为正常、`usage_limit_reached` 的 429 记为周窗口耗尽并记录重置时间。页面新增“最近事件”列表。
- 自动点火落地：点火改为经宿主 `host.model.execute` 发送最小 Codex 请求（`openai-response` 协议、`Reply with only OK` 提示、不推理、不用工具），模型默认 `gpt-5-codex`，可在设置里改。重置成功后按 `grace_seconds` 自动点火；每日开始—结束时段内每个凭证最多自动点火一次。
- 重置加固（参考 mrchi）：同一凭证同时只跑一个重置流程，已定论后 5 分钟内不再触发；credit 列表最多重试 3 次、401/403 立即停；挑卡跳过已过期 credit、无过期时间的卡排最后；幂等 ID 改用标准 UUIDv4，且 consume 发出后绝不重试；account ID 增加 `id_token` JWT 回落链，`email_` / `local_` 开头的占位 ID 不再作为 `ChatGPT-Account-ID` 发出；`resets_at` 可作为 `resets_in_seconds` 的兜底。
- 配置解析补全：此前 `parseYAML` 会静默丢掉 `enabled`、`alerts.*`、`poll_interval_seconds`、`reset.credentials`（支持 `- item` 列表与内联两种写法）和 `ignition.model`，现已全部支持。设置页保存改为按字段合并，不会再把页面不展示的 `reset.credentials` 清空；设置页新增重置凭证白名单与点火模型输入框。
- Telegram 推送：账号停用文本（`disabled` / `deactivated` / `suspended` / 停用）会单独识别为“账号已停用”通知；发送失败会写 CPA 日志，测试推送失败时页面会明确报错而不是只显示“已提交”。
- 界面：操作卡片改名为“立即操作”（内含立即重置、立即点火两个按钮，顺序不变，重置在前）；操作顺序与按钮文案不变。
- 增加回归测试 `TestManagementPageReturnsTypedResponse`、`TestManagementStatusReturnsTypedResponse`、`TestManagementRefreshRoute`、`TestSettingsMergePreservesCredentials`、`TestParseYAMLCompletesConfig`、`TestParseCodexUsage`、`TestWeeklyExhaustedResetsAt`。

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
