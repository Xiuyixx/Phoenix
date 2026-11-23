# Changelog

## 0.0.16 - 2026-11-23

- 修复 Telegram Bot Token 保存失败：保存时页面把 Bot Token 当成模型列表 API key 拿去 CPA 的 /v1/models 校验（check-models-key），Telegram 的 token 永远通不过，保存被拒并显示“ CPA 不接受这个 key”，token 从未存进 config.yaml；测试推送能通过是因为它用的是已加载的旧 token。修复：只有 models_api_key 才走管理密钥拒绝与 /v1/models 校验，Bot Token 直接保存（用测试推送验证）。另修一个同类问题：保存 Telegram 分组内任意字段时，空的 Bot Token 输入框会被当成 "" 一起提交，把已保存的 token 清掉——现在空 Bot Token 与模型 key 一样视为“保持不变”，不再随保存发出。测试推送成功后会清除残留的“telegram send failed”横幅，不必等下一次后台通知。

## 0.0.15 - 2026-11-23

- 模型列表 API key 输入框按原版显示掩码：已保存的 key 现在回填真实值，密码框渲染成圆点（此前输入框恒为空，只靠“已设置”提示）。同时删掉“已设置”提示及其中英文 i18n 键；无 key 时的横幅提醒不变。保存流程不动（改动才触发校验、留空不改动、管理密钥拒绝、Telegram bot_token 仍留空）。
- 复刻 Did Codex Reset 并真的能跑：新增 `codex_reset_updates` 配置（默认关闭、轮询 300 秒、首次开启推送当前待生效排期），轮询 https://didcodexreset.com/openapi/v1/records（最新 10 条），按原版逻辑去重（首次运行只发当前待生效排期、已见不重发、被取代的排期不发、排期变更重发，48 小时内有效记录、最多记 100 条），新信号经 Telegram 推送（不用 Bark/webhook），中英文通知文案与时间格式照 Phoenix 现有做法，历史链接附在正文末尾。设置页在模型 key 与额度图表之间新增 Did Codex Reset 设置块。失败只记 warn 日志，不阻塞额度轮询。

## 0.0.14 - 2026-11-23

- 修复模型列表 API key 填了也显示“未设置”：key 其实早已存进 config.yaml（13:14 的校验与 PATCH 均成功，文件里顶层 models_api_key 已有 20 位值），但插件一直读不到。根因在手写 parseYAML：CPA 下发配置前会用 yaml.Marshal 重排成 2 空格缩进（顶层键在 0 列），而解析器遇到顶层键值行时不重置 section——排在 reset: 之后的顶层 models_api_key 被误认成 reset.models_api_key 悄悄丢掉；config_token 同样丢失，所以保存也永远无法确认已应用。修复：顶层键值行（0 列）先把 section 清空再分发。新增回归测试 TestParseYAMLDeliveredShape（旧解析器复现失败），并用服务器上真实 phoenix 配置块按 CPA 下发形态验证通过（key 20 位、config_token 16 位均读到）。部署后无需重新填 key，文件里已有的 key 会直接生效。

## 0.0.13 - 2026-11-23

- 修复刷新页面后全部已保存配置“丢失”：根因是 `status()` 把 `targetEntry()` 的 `nil` 结果直接追加进 targets——没有 5h 窗口的组（测试服务器上 Antigravity 的两个组只有 7d 窗口）会返回 nil，于是 status 里出现 `null` 目标；页面 `targetStatus(t)` 在 `t.blocked_window` 上抛错，启动时的 `Promise.all` 被拒，`fillSettings()` 从未执行，所有设置项都停在空白默认值上，看起来像配置丢了。config.yaml 里的配置其实一直在，从未丢失。修复：后端跳过 nil 的 target（只有带 5h 窗口的组才进目标列表）；页面跳过 null 目标且 `targetStatus` 做空值保护；启动时先填设置再渲染，今后即使渲染再出问题也不会把表单清空。新增回归测试 `TestStatusSkipsTargetsWithoutFiveHourWindow`（共 58 个测试，旧代码上复现失败、新代码通过）。

## 0.0.12 - 2026-11-23

- 修复模型列表 API key 在手机上始终保存失败：服务器日志证明失败发生在浏览器本地、没有任何校验或保存请求发到 CPA——保存时输入框里的值是浏览器自动填入的 CPA 管理密钥（与管理中心同源，iOS Safari 会把已保存的管理密钥填进密码框，粘贴后失焦时还可能被还原），页面按设计直接拒绝管理密钥。`autocomplete="one-time-code"` 拦不住这种自动填充，改用 `autocomplete="new-password"`（模型列表 API key 与 Telegram Bot Token 两个密码框均改），并加 `autocapitalize/autocorrect/spellcheck` 关闭。另修一个连带问题：窄屏下保存失败只显示紧凑文案“✕ 保存失败”，具体的拒绝原因被 CSS 藏起来了，用户看不到到底为什么失败；现把原因拼进紧凑文案，手机上也能看到（例如“✕ 保存失败：这是 CPA 管理密钥，未保存。请填写 access.api-keys 中的 key”）。

## 0.0.11 - 2026-11-23

- 多服务支持：额度轮询与自动点火从 Codex-only 扩展到 Claude 与 Antigravity（按 Lamplighter 的 provider 实现改写）。Claude 查 `api.anthropic.com/api/oauth/usage`（5 小时、7 天窗口，Opus/Sonnet/OAuth Apps/Cowork 各为独立额度组，Fable 由 `limits[]` 的 weekly_scoped 项解析），Antigravity 查 `daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary`（3 个 URL 依次回退，Hub 版本缓存 3 小时），按模型分 Gemini 与 Claude / GPT 两个独立额度组；Kimi 与 Grok 在服务列表中标记为不支持，不参与轮询与点火。点火按服务区分协议与候选模型：Codex `openai-response`（最新 luna 优先）、Claude `claude` 协议（最新 Haiku 优先，其次 Sonnet）、Antigravity `openai` 协议（Gemini 组最新 Flash 优先，Claude / GPT 组 Haiku > Sonnet > Opus > GPT-OSS）；Antigravity 自动点火默认关闭，开启后默认只点 Gemini 组。已配置的点火模型始终优先（Codex 的 `gpt-6-luna` 永不重置），每服务可配开关、模型与额度组，`PUT /settings` 的 `providers` 按服务合并，不会互相清空。
- 额度组模型：账号下除主组外可有额外额度组（Claude 额外窗口、Antigravity 各模型组），`state.json` 的基线与目标改以组 key（`codex:<idx>:codex:main` / `claude:<idx>:<窗口>` / `antigravity:<idx>:<slug>`）键入，旧的裸 `auth_index` 键在加载时迁移；历史图表按组分系列，额外组标注为“账号 · 来源”；提醒（阈值/恢复）按组窗口评估，重置提醒仍只针对 Codex。点火确认改为按服务重新查询该凭证额度，直到观测到固定的未来 5 小时重置。
- 修复模型列表 API key 保存失败：页面此前用 `fetch("/v1/models")` 直接向页面源校验，请求经 nginx 代理到 CPA 时 Authorization 头被丢掉，CPA 永远回 401，正确 key 也存不下去。现改走插件路由 `POST /check-models-key`（插件直连 CPA 校验），返回 `{ok}` / `{refused}`（401/403）/ `{reason}`（结论不明时允许保存）；`status.config` 的 `models_api_key` 恒为 `""`，新增 `models_api_key_set` 布尔；设置页 key 输入框已设置时保持为空并显示“已设置”，留空保存不发送该键（`PUT /settings` 中 `models_api_key` 缺席或 `""` = 不改动，`null` = 清空）。
- 新增回归测试 15 个（共 57 个，含 `-race`）：服务归一化与别名（含 xai/Grok 注册 ID）、Claude/Antigravity 额度解析与被动头、各服务点火候选排序、配置模型优先级、providers 合并与 YAML groups 解析、组点火开关与 Antigravity 默认关闭、`/reset` 拒绝非 Codex、状态键迁移、`/check-models-key` 三种返回、历史额外组系列、configFields 声明、PUT 换 key 后模型缓存预热。Grok 注册 ID 按 Lamplighter 为 `xai`（`grok` 为别名），Fable 窗口 ID 为 `seven-day-fable`；`PUT /settings` 换 key 后后台预热模型列表，`next_models` 及时更新；`configFields` 补齐 `models_api_key` / `cpa_base_url` / `providers` / 阈值等声明。
- 已知限制：Claude 与 Antigravity 只有单元测试，没有真机验证——测试服务器上没有 Claude 账号，且 Codex 凭证文件已丢失，无法做端到端验证。

## 0.0.10 - 2026-11-23

- 面板按 Lamplighter 完全复刻：page.html/page.css/page.js 以 Lamplighter 原文件为底本逐字复制（628 行 CSS、暖灰纸面、三主题、1200px 容器、图表/筛选/排序/刷新/横幅/事件/设置分区、i18n 与语言/主题跟随、config_token 保存流），只做品牌与 Phoenix 差异手术；logo 用用户提供的图（web/logo.jpg，白底圆角小框 30×30 原样使用，base64 内嵌进插件，无远程请求）。
- 点火模型与点火目标完全按 Lamplighter：模型来自服务表 `providers.codex.model`（占位“自动：最新的 Luna”，`next_models` 追加），已配置的 gpt-6-luna 优先且永不重置；目标为 7 列表格，`POST /ignite {target: "codex:<idx>:codex:main"}`。
- 重置按 Lamplighter 的呈现适配：图表重置标记、重置提醒、重置列与 Phoenix reset credit 机制对接；动作接口统一 `{ok, status}`，`/reset` 同步返回，`/ignite` 接受 `{target}`。
- 后端对齐 Lamplighter 的 status/history 形状：`groups[]` 层、顶层错误/语言/时区/providers/next_models/group_order、history 桶/points/resets 字段；PUT `/settings` 接受顶层键与 `providers.codex.model`，`reset.credentials` 字符串先归一化为数组再合并。
- 修复 `status()` 持锁调用 `cachedNextModel()` 导致的死锁（拆为 `cachedNextModelLocked()`）；修复 PUT 回退路径丢字符串 `reset.credentials` 的问题。
- 已知限制（宿主侧，未修）：CPA 8.1.1 的 Codex 凭证已迁入 kv.db，插件宿主 AuthList/AuthGet 只认文件凭证，主动轮询与取 token 重置对 kv 凭证不可见；面板数据来自持久化状态 + 被动 usage 填充，点火走 host.model.execute 不受影响。

## 0.0.9 - 2026-11-23

- 修复轮询在测试服务器上取不到任何额度的问题：轮询、重置与点火回退用 `Provider == "codex"` 精确匹配过滤凭证，宿主在该环境下返回的条目 `Provider` 为空、只有 `Type` 是 codex，导致全部凭证被静默跳过——无账号、无历史、无报错。现按 Lamplighter 的 `loadCreds` 做法归一化（`Provider` 优先、`Type` 兜底、转小写并去空格），并在有凭证但无一匹配为 Codex 时把原因写进 `listErr`（面板通知横幅展示）与 CPA 日志，不再静默空面板。
- 增加回归测试 `TestIsCodexEntry`、`TestPollOnceMatchesTypeOnlyCodex`、`TestPollOnceNoCodexCredentialIsLoud`。

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
