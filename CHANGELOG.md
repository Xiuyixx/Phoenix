# Changelog

## 0.1.13 - 2026-11-25

- 临时错误攒满 3 次触发的熔断最多暂停 1 小时，不再停到次日零点；硬错误仍停到次日零点。

## 0.1.12 - 2026-11-24

- 通知正文每行冒号后都加一个空格，和账号行统一，冒号和内容不再错位。
- 通知标题现在加粗显示。

## 0.1.11 - 2026-11-24

- 通知里的 5H、7D 两行现在和账号、模型、状态、时间对齐：额度行的标签改用全角字符 ５Ｈ、７Ｄ，和两个汉字的标签一样宽，冒号和内容不再错位。
- 通知里的邮箱不再和“账号：”连成一个蓝色链接：邮箱实体改成带 mailto: 地址的链接，账号行在冒号后加一个空格，即使客户端不认实体、自己识别链接，也只会把邮箱本身染蓝。

## 0.1.10 - 2026-11-24

- 点火失败的原因不再直接铺在状态列里：0.1.9 把完整报错文字放在状态栏下方，几行同时失败时表格全是长报错；现在状态栏只显示“上次点火失败”/“失败 N 次”/“暂停至 …”这样的简短状态，完整原因改放悬停提示里，遮住账号名时不显示带账号名的提示。手动点火失败仍会记下来（0.1.9 的后端记录不变）。

## 0.1.9 - 2026-11-24

- 手动点火失败现在会记下来：之前手动点“立即点火”失败后，面板上没有任何痕迹（状态仍显示正常），事后查不到失败原因；现在失败原因记入 last_error，面板状态栏显示“上次点火失败”并把原因放在悬停提示里，自动重试、熔断与凭证状态不受影响。

## 0.1.8 - 2026-11-24

- 立即操作表里“立即重置”与“立即点火”两个按钮之间加 8px 间距：之前两个按钮之间没有任何间隔，贴在一起容易点错。

## 0.1.7 - 2026-11-24

- 无名账号的默认备注改成完整邮箱：之前取邮箱 @ 前最后两个字符，两个账号都叫 xiuyixx 时都显示 `GPT#xx`，分不出来；现在同一服务有多个账号时直接显示 `GPT#xiuyixx@icloud.com` 这样的完整邮箱。填了名称的账号不受影响；没有邮箱时仍用序号兜底；MCP 输出仍按原有规则打码，不返回完整邮箱。

## 0.1.6 - 2026-11-24

- 手动“立即重置”按钮加确认：重置会消耗一张真实 reset credit，误触不可撤销；现在点之前先确认，与点火按钮一致（新增 `reset_confirm` 中英文词条）。
- 设置页“通知已关闭”提示计入 Bark：之前只看 Telegram 与 Webhook，只配 Bark 的用户会看到错误的关闭提示；提示文案与无用词条 `notify_off_telegram` 一并清理。
- Telegram 发送增加 `ok` 字段校验：HTTP 200 但 `ok:false` 时不再当作发送成功，错误里带上 Bot API 返回的 description。
- 页面测试的 I18N 键唯一性检查改为剥离字符串字面量后全量扫描，之前只覆盖约一半的键，多键行的重复测不到；补停用通知空值/单值断言与 Did Codex Reset 错误路径测试（畸形 JSON、网络错误、null 项、page/pageSize）。
- 文档修正：`docs/architecture.md` 的新版本检查段落之前写后台循环每 6 小时读、页面打开时调用，实际自动检查默认关闭、只有手动 `POST /check-update` 触发；`reset_min_interval` 标签改为“重置要求的最短剩余时间”；英文测试推送文案统一为 Telegram。

## 0.1.5 - 2026-11-24

- 修复面板倒计时归零文案：I18N 里 `reset_now` 键重复，倒计时归零时显示的是“立即重置”；重置按钮改用新键 `reset_action`，倒计时归零恢复显示“已重置”/“reset”，与 Lamplighter 一致；页面测试新增 I18N 键唯一性断言。
- 中文账号停用通知恢复错误码与状态：状态行后追加 `（403 · TOS_VIOLATION）` 形式的信息，与 Lamplighter 一致；英文文案与事件 Detail 本就包含该信息。
- Did Codex Reset 拉取地址改为包级变量，新增 httptest 端到端测试覆盖拉取与解析（成功、非 200、异常 payload）。
- README 与开发文档里的版本号更新到 0.1.5；修正一处从 Lamplighter 抄来的注释版本号。

## 0.1.4 - 2026-11-24

- 新增仓库根目录 `registry.json`，为上架 CLIProxyAPI 官方插件商店做准备：schema_version 1，github-release 安装方式，含英文描述、作者、仓库、许可证与 zh-CN 本地化描述。
- 发布工作流在 release job 里生成 `checksums.txt`（三个平台 zip 的 sha256sum 格式，裸文件名），随 release 资产一起发布；商店安装时 CPA 按 `checksums.txt` 校验 zip，之前只有每平台一个 `.sha256` 文件，商店安装会失败。

## 0.1.3 - 2026-11-24

- README 优化：版本号全部更新到 0.1.3；结构改为简介 → 快速安装 → 主要功能 → 配置说明 → 安全与运行边界 → 故障排查 → 参考与致谢 → 许可证；轮询协议、点火调度、重置流程等技术细节只留摘要并指向 docs/architecture.md，不再与 docs 重复；版本与发布流程移到 docs/development.md；顶部增加 Release / License / Go 徽章与目录。

## 0.1.2 - 2026-11-24

- 通知标题改回单个表情：中文 Phoenix 样式通知标题前面的三组表情全部改为一个（例如“🟢 🟢 🟢 GPT 额度状态通知”→“🟢 GPT 额度状态通知”），标题其余文字、标点、空格不变；英文文案不动。

## 0.1.1 - 2026-11-23

- 修复 Telegram 通知里邮箱链接范围过大：发送时对每个邮箱地址加显式 "email" 实体（offset/length 按 UTF-16 码元算），只有邮箱本身可点击，"账号："前缀不再被客户端的自动识别拉进链接；没有邮箱的消息不发 entities 字段。Bark/Webhook 的发送路径与消息正文不动。
- Phoenix 风格通知的标题与模型行改用不带账号后缀的干净标签：额度状态、重置提醒、冷却提醒、账号异常/停用/限流、点火熔断的中文标题与模型行里，标签一律用服务本身的标题（"GPT"、"Gemini"、"Claude / GPT"），不再用带 "#名字"/"#邮箱" 后缀的组标签；账号识别只保留在"账号："行。英文文案不动。

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
