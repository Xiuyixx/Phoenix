# Phoenix

Phoenix 是一个面向 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生插件，围绕 Codex、Claude 与 Antigravity 订阅凭证提供额度状态、账号错误通知、自动点火以及管理页面操作入口。Kimi 与 Grok 在服务列表中标记为不支持，不参与轮询与点火。

> 当前版本：`0.1.0`。本版本是从 Lamplighter 干净复刻后系统改名为 Phoenix 的底本，并已移植 Phoenix 差异功能：Telegram 推送、reset credit 自动/手动重置、Phoenix 通知文案、模型 key 经插件路由校验（`POST /check-models-key`）与点火模型 fallback。

## 主要功能

### 额度与账号状态

Phoenix 通过两条路径收集额度数据：主动轮询按 `poll_interval_seconds`（默认 300 秒）枚举凭证并按服务查询上游额度——Codex 查 `/wham/usage`（按窗口时长识别 5 小时窗口与周窗口）、Claude 查 `api.anthropic.com/api/oauth/usage`（5 小时、7 天窗口，另有 Opus/Sonnet/OAuth Apps 等独立窗口与 Fable 窗口）、Antigravity 查 `daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary`（按模型分 Gemini 与 Claude / GPT 两个独立额度组）；被动接收 CPA 的 usage 回调，成功记为正常、`usage_limit_reached` 的 429 记为对应窗口耗尽并记录重置时间（Claude 被动头 `Anthropic-Ratelimit-Unified-*` 也会解析）。页面新增“最近事件”列表，账号异常、重置与点火结果都会记录在内。

额度采样会持久化到插件数据目录下的历史文件（默认保留 40 天），管理页面用“额度变化”图表展示：总览按采样点着色（满/高/中/低/空五档），可点开单账号详情曲线（含匀速参考线、预测线与重置标记），支持 5 小时 / 7 天窗口与时间范围切换。

额度数据是否完整取决于 CPA 版本、凭证类型以及上游响应中是否包含可解析的额度字段。没有收到有效额度数据时，页面会显示等待状态，不会把缺失数据当成 100% 或 0% 使用量。

设置页的“账号”区支持拖动排序、填写名称与权重，卡片与图表的顺序跟随账号顺序；名称取自 CPA 凭证备注的第一行。

### 通知

通知按 Bark → Telegram → webhook 的顺序扇出，同时发送到所有已配置的渠道；三个渠道都没配时推送关闭。当前通知场景包括：

- 账号返回 `401` 或 `403`，通常表示凭证失效、账号未授权或账号被停用。
- 账号返回 `429`，通常表示限流或额度耗尽。
- 额度降到提醒阈值（默认剩余 20% / 10% / 5% 各通知一次），额度恢复、重置前提醒。
- 自动重置成功。
- 点火失败或熔断。
- 测试推送。

通知文案为 Phoenix 定稿：中文通知用三组表情标题（🟢 Normal / 🟡 Warning / 🔴 Exhausted）与账号 / 模型 / 5H / 7D / 状态 / 时间逐行字段，账号用邮箱标识；英文通知保持 Lamplighter 原文案。

Bark 请求为 `GET {bark_url}/{标题}/{正文}?group&level&icon&url`，默认分组为 `CPA`，默认图标为空（Phoenix 不带 logo，可用 `bark_icon` 自行设置）。webhook 支持 `url`、`method`、`headers`、`body`、`success_json`，请求体为空时发送内置 JSON，占位符为 `{{title}} {{body}} {{text}} {{url}} {{priority}} {{kind}} {{label}} {{time}}`。

#### Telegram 推送

设置 Telegram Bot Token 和接收 ID 后，Phoenix 可以向一个或多个接收者发送提醒。多个接收 ID 使用英文分号分隔，例如：

```text
123456789;987654321;-1001234567890
```

Bot Token 只用于调用 Telegram Bot API，不写入 Git 仓库。配置文件和管理接口中的 Token 应按敏感信息保护；管理接口返回的配置中 Bot Token 一律脱敏（只返回是否已设置），页面留空表示不修改。Bot Token 为空或未启用时该渠道不启用；Token 已填但未填接收 ID 时，设置页会报“Telegram 设置无效”，通知横幅中显示原因。

Did Codex Reset 信号默认关闭；开启后按 `codex_reset_updates.poll_seconds`（默认 300 秒）轮询 https://didcodexreset.com/openapi/v1/records，去重后经已配置的通知渠道（含 Telegram）转发；发送失败的记录仍标记为已见，不再每轮重复播报。

### 自动点火

点火通过宿主的模型执行回调发送一次最小请求（只要求回复 `OK`，不推理、不用工具），从而开始一个新的 5 小时窗口；点火后会多次复查，确认出现固定的未来 5 小时重置才算成功，否则报未确认并按退避重试，多次失败后熔断到次日开始。自动点火在每日开始—结束（加宽限分钟）时段内按 5 小时窗口的重置时间调度，默认 7–22 点、结束后宽限 30 分钟、失败重试间隔 300 秒、熔断前瞬时失败上限 3 次。

点火按服务区分：Codex 用 `openai-response` 协议（最新 Luna 优先）、Claude 用 `claude` 协议（最新 Haiku 优先，其次 Sonnet）、Antigravity 用 `openai` 协议（Gemini 组最新 Flash 优先，Claude / GPT 组 Haiku > Sonnet > Opus > GPT-OSS）；Antigravity 自动点火默认关闭，开启后默认只点 Gemini 组。管理页面的“立即操作”卡片是一张点火目标表，每行对应一个额度组。Codex 行有两个独立按钮，顺序固定为“立即重置”在前、“立即点火”在后；其他服务行只有“立即点火”。立即重置只走重置流程，立即点火只走点火流程，两者互不触发。

点火模型按插件当前配置显示；设置了模型列表 API key 后，插件会从 CPA 的 `/v1/models` 读取候选模型，但已配置的模型始终优先。已配模型被拒时按列表候选 fallback，Codex 最后兜底试 `gpt-5-codex`（仅当它在模型列表中时）；连续被拒两次即停止点火。模型列表 API key 的校验走插件路由 `POST /check-models-key`（插件直连 CPA），页面不直接请求 CPA 的 `/v1/models`。

### 自动重置（仅 Codex）

自动重置只针对 Codex 订阅渠道的周用量窗口。触发条件为：

1. 请求失败状态为 `429`。
2. 错误类型为 `usage_limit_reached`。
3. 凭证属于 Codex。
4. 窗口是周窗口，通常由 `limit_window_minutes: 10080` 表示；缺少显式窗口字段时，根据剩余时间进行保守判断。
5. 剩余恢复时间达到配置的最低秒数（默认 `86400`），用于排除短时 5 小时窗口。
6. 凭证在 Phoenix 的纳入列表中，或当前配置允许自动处理全部凭证。
7. 上游返回至少一张可用的 reset credit。

命中后，Phoenix 会：

1. 通过 CPA host 回调读取凭证。
2. 提取 access token 和 ChatGPT account ID。
3. 查询 `rate-limit-reset-credits`（失败最多重试 3 次）。
4. 选择可用且最早到期的 credit（无到期时间的 credit 视为永不过期）。
5. 生成一次性的幂等请求 ID。
6. 调用 credit consume 接口（只发一次，`Content-Type: application/json`）。
7. 只有上游返回 `reset` 或 `already_redeemed` 时才视为 credit 操作成功。
8. 调用 CPA 的 `host.routing.reset_cooldown` 清除本地凭证冷却。
9. 记录日志并发送通知；点火开关开启时，在点火宽限（`ignition.grace_seconds`）后自动点火一次。

同一凭证 5 分钟内只做一次重置决策，重复的 429 不会消耗第二张 credit。消费 credit 后如果清除 CPA 冷却失败，Phoenix 会记录警告并通知“重置未完全成功”，不会重复消费 credit。网络错误、凭证缺失、没有可用 credit 或上游返回其他结果时，不会伪造成功状态。

手动重置（“立即重置”按钮或 `POST /v0/management/phoenix/reset`）只走重置流程，不自动点火；自动重置成功后仍会按配置的延迟自动点火一次。

### MCP

独立 MCP HTTP 服务默认关闭。开启后在 `mcp_listen`（默认 `127.0.0.1:8318`）监听，密钥为空时页面在同一次保存中生成 `phxmcp_` 前缀的随机密钥。客户端配置示例：

```bash
claude mcp add --transport http phoenix http://127.0.0.1:8318/mcp --header "Authorization: Bearer <key>"
```

## 兼容性与构建平台

Phoenix 使用 CLIProxyAPI v8 插件 SDK，并通过 c-shared 动态库加载，当前依赖的 SDK 版本为 v8.0.36。

当前 GitHub Release 提供：

- Linux amd64：`linux/amd64`，文件扩展名 `.so`。
- Linux arm64：`linux/arm64`，文件扩展名 `.so`。
- Windows amd64：`windows/amd64`，文件扩展名 `.dll`。

macOS 可以在具备 Go、CGO 和对应编译工具链的机器上自行构建；构建结果必须使用 CPA 所在系统对应的动态库格式和架构。

## 从 GitHub Release 安装

以下步骤适用于已经运行 CLIProxyAPI 的服务器。请先确认 CPA 使用的系统和 CPU 架构：

```bash
uname -s
uname -m
```

架构对应关系：

- `x86_64` → `amd64`。
- `aarch64` 或 `arm64` → `arm64`。

### 1. 下载正确的 Release 包

打开 [Phoenix Releases](https://github.com/Xiuyixx/Phoenix/releases)，选择目标版本，例如 `v0.1.0`，下载与 CPA 运行平台匹配的压缩包和对应 `.sha256` 文件。

Linux amd64 示例：

```bash
VERSION=0.1.0
mkdir -p /tmp/phoenix-$VERSION
cd /tmp/phoenix-$VERSION
curl -fL -o phoenix.zip "https://github.com/Xiuyixx/Phoenix/releases/download/v$VERSION/phoenix_${VERSION}_linux_amd64.zip"
curl -fL -o phoenix.zip.sha256 "https://github.com/Xiuyixx/Phoenix/releases/download/v$VERSION/phoenix_${VERSION}_linux_amd64.sha256"
sha256sum -c phoenix.zip.sha256
unzip -l phoenix.zip
unzip -o phoenix.zip
```

校验通过后，压缩包中应至少包含：

- `phoenix.so` 或 `phoenix.dll`。
- `VERSION`。

Windows 使用 PowerShell 校验：

```powershell
$version = "0.1.0"
$file = "phoenix_${version}_windows_amd64.zip"
(Get-FileHash $file -Algorithm SHA256).Hash.ToLower()
Get-Content "${file}.sha256"
```

两边显示的 SHA256 值必须一致。

### 2. 确认 CPA 插件目录

CLIProxyAPI 的默认插件目录通常是 CPA 工作目录下的 `plugins`。如果配置文件显式设置了插件目录，应以配置文件为准。插件目录按运行系统和架构分层：

```text
<plugins-dir>/linux/amd64/phoenix.so
<plugins-dir>/linux/arm64/phoenix.so
<plugins-dir>/windows/amd64/phoenix.dll
<plugins-dir>/darwin/amd64/phoenix.dylib
<plugins-dir>/darwin/arm64/phoenix.dylib
```

当前 Release 只提供前三种资产。不要把 Linux `.so` 放到 Windows 或 macOS 目录，也不要把 `arm64` 文件放到 `amd64` 目录。

如果 CPA 使用的是默认插件目录，可按下面方式安装 Linux amd64：

```bash
CPA_DIR=/path/to/CLIProxyAPI
mkdir -p "$CPA_DIR/plugins/linux/amd64"
install -m 0755 phoenix.so "$CPA_DIR/plugins/linux/amd64/phoenix.so"
```

把 `/path/to/CLIProxyAPI` 替换成真实 CPA 目录。不要直接覆盖正在运行的插件文件；推荐先停止 CPA，替换文件后再启动。

### 3. 启用插件

在 CPA 的 `config.yaml` 中确认插件总开关已开启，并在 `plugins.configs` 下添加 Phoenix 配置：

```yaml
plugins:
  enabled: true
  configs:
    phoenix:
      enabled: true
      ignition:
        enabled: true
        start_hour: 7
        end_hour: 22
        end_grace_minutes: 30
```

建议先使用以下最小配置确认插件能被 CPA 加载：

```yaml
plugins:
  enabled: true
  configs:
    phoenix:
      enabled: true
```

确认加载成功后，再通过管理页面配置通知和自动点火。这样可以把“插件加载问题”和“上游通知/点火问题”分开排查。

### 4. 重启并检查加载结果

修改配置后重启 CPA。不同部署方式的命令不同，例如：

```bash
# Docker Compose
 docker compose restart

# systemd，服务名称以实际部署为准
sudo systemctl restart cliproxyapi
```

重启后检查：

- CPA 日志中出现 Phoenix 注册或加载信息。
- 管理中心的插件菜单出现 `Phoenix`。
- 打开 Phoenix 页面后状态显示“运行中”。
- 页面可以读取配置并保存设置。
- 使用“测试推送”确认已配置的通知渠道能收到消息。

不要直接假设服务名称、配置路径或容器名称。先用实际部署中的 `systemctl list-units`、`docker compose ps` 和 CPA 启动参数确认。

## 从源码构建

### Linux 本机构建

要求：

- Go 版本满足 `go.mod`。
- GCC 或其他 CGO 编译器可用。
- CPA 目标平台与构建目标一致。

```bash
git clone https://github.com/Xiuyixx/Phoenix.git
cd Phoenix
go test ./...
make check-version
make build
```

Linux amd64 的产物为：

```text
dist/phoenix.so
```

手动安装时，将它复制为 CPA 插件目录中的 `phoenix.so`。

### 本地 Release 包

```bash
make release VERSION=$(cat VERSION)
unzip -l dist/phoenix_$(cat VERSION)_linux_amd64.zip
sha256sum -c dist/checksums.txt
```

`make release` 使用当前主机的 `GOOS/GOARCH`，适合在本机生成测试包。跨平台正式包由 GitHub Actions 按 runner 原生 shell 构建，Windows 使用 PowerShell 的 `Compress-Archive` 和 `Get-FileHash`。

## 配置说明

| 配置 | 作用 |
| --- | --- |
| `enabled` | Phoenix 总开关。关闭后插件保留注册能力，但不执行主动功能。 |
| `bark_url` / `bark_group` / `bark_icon` | Bark 推送地址（到 device key 为止）、通知分组（默认 `CPA`）、通知图标（默认空）。 |
| `webhook` | 自定义 webhook：`url`、`method`、`headers`、`body`、`success_json`；`url` 为空时不启用。 |
| `notice_threshold` / `low_threshold` / `critical_threshold` | 提醒阈值（剩余百分比），默认 `20` / `10` / `5`，须从高到低。旧配置的 `alerts.thresholds` 数组仅在三个新阈值均未写时映射。 |
| `telegram` | Telegram 推送：`bot_token`、`chat_ids`（多个用分号分隔）、`enabled`；`bot_token` 为空时不启用。 |
| `reset` | reset credit 重置：`enabled`（默认开启）、`credentials`（凭证序号列表或逗号分隔文本，留空为全部 Codex 凭证）、`weekly_only`（默认仅每周窗口）、`minimum_reset_seconds`（默认 `86400`）。 |
| `recovery_notify` | 额度恢复通知模式，分 5 小时 / 7 天窗口：`off` / `all` / `after_exhausted`；默认 5 小时 `after_exhausted`、7 天 `all`。 |
| `reset_reminder` | 重置前提醒模式，分 5 小时 / 7 天窗口：`off` / `all` / `has_remaining`；默认 5 小时 `off`、7 天 `has_remaining`。 |
| `poll_interval_seconds` | 主动额度查询间隔（秒），默认 `300`，最小 `60`。 |
| `request_timeout_seconds` | 上游请求超时（秒），默认 `20`。 |
| `passive_skip_seconds` / `passive_skip_max_minutes` | 整点前有被动数据时跳过主动查询的秒数（默认 `60`）与同一账号最多连续跳过分钟数（默认 `30`）。 |
| `cpa_base_url` | CPA 地址，用于读取模型列表，默认 `http://127.0.0.1:8317`，请按 CPA 实际监听地址填写。 |
| `models_api_key` | 从 CPA 读取模型列表用的 API key（请用专用 key，不要填管理密钥）。 |
| `mcp_enabled` / `mcp_listen` / `mcp_key` | 独立 MCP 服务开关、监听地址（默认 `127.0.0.1:8318`）、Bearer 密钥（`phxmcp_` 前缀）。 |
| `ignition.enabled` | 自动点火开关，默认开启。 |
| `ignition.model` | 旧配置写法，等价于 `providers.codex.model`；仅当后者未写时映射，已配值优先（用户已配的 `gpt-6-luna` 不会丢失）。 |
| `ignition.start_hour` / `ignition.end_hour` | 自动点火每日开始 / 结束小时，默认 `7` / `22`。 |
| `ignition.end_grace_minutes` | 每日结束后的宽限分钟，默认 `30`。 |
| `ignition.grace_seconds` | 额度窗口重置后等待的秒数，默认 `3`。 |
| `ignition.failure_retry_seconds` | 点火瞬时失败后的重试间隔（秒），默认 `300`。 |
| `ignition.max_transient_failures` | 点火熔断前的瞬时失败上限，默认 `3`。 |
| `providers.<服务>.monitor` / `.ignite` / `.model` / `.groups` | 每服务的监控开关、自动点火开关、点火模型与点火额度组。Codex 与 Claude 默认点火开启，Antigravity 默认关闭、开启后默认只点 `Gemini` 组；Kimi 与 Grok 不点火。 |
| `codex_reset_updates` | Did Codex Reset 转发：`enabled`（默认关闭）、`poll_seconds`（默认 `300`）、`notify_current_pending`。 |
| `history_retention_days` | 额度历史保留天数，默认 `40`。 |
| `data_dir` | 状态和历史目录，默认插件目录下的 `data/phoenix`。 |
| `account_weights` / `account_order` | 账号权重与账号顺序（设置页拖动排序后保存）。 |

设置页面可以修改通知、提醒规则、点火与高级字段。保存时页面调用 CPA 的管理接口 `PATCH /v0/management/plugins/phoenix/config`，每次带一个随机 `config_token`；CPA 合并后写入 `config.yaml` 并重新下发给插件，页面等到自己的 token 被回显才显示“已生效”，重启后配置仍然有效。管理接口返回的模型列表 API key 与 MCP 密钥一律脱敏，页面留空表示不修改。

## 安全与运行边界

- 不要把 Bot Token、access token、完整凭证 JSON 或 CPA 管理密钥提交到 Git。
- 自动重置会消耗真实 reset credit。建议先使用测试账号，并保持 `weekly_only: true`。
- reset credit 接口是上游非官方接口，接口字段或策略变化可能导致流程失败，需要根据 CPA 日志处理。
- 点火会发送真实模型请求并开始新的 5 小时窗口，首次部署建议先把 `ignition.enabled` 设为 `false`，确认额度、被动数据、页面和通知正常后再开启。
- 卸载或升级前应停止 CPA，替换对应平台的插件文件，再启动并观察日志。

## 故障排查

### 管理中心没有 Phoenix

按顺序检查：

1. 插件文件扩展名是否与 CPA 平台一致。
2. 文件是否放在 `<plugins-dir>/<goos>/<goarch>/` 目录。
3. CPA 是否设置了 `plugins.enabled: true`。
4. 配置键是否为 `plugins.configs.phoenix`。
5. 当前 CPA 版本是否支持 v8 插件 ABI。
6. CPA 启动日志中是否有动态库加载错误。

### 页面显示运行中但没有账号数据

这通常表示插件已加载，但 CPA 尚未产生可识别的 usage 记录，或上游响应没有可解析的额度字段。先通过 CPA 发起一次对应服务的请求，再查看 Phoenix 页面和 CPA 日志。

### 设置保存后没有生效

页面保存后会等 CPA 回显 `config_token` 才显示“已生效”。如果一直显示“未能验证”，检查 CPA 版本是否支持 `PATCH /v0/management/plugins/<id>/config`，以及 `config.yaml` 是否可写。

### 没有收到通知

检查 Bark 地址、Telegram Bot Token 与接收 ID、webhook 地址与 `success_json` 是否正确、服务器是否可以访问通知服务。先点击“测试推送”，再检查 CPA 日志中的 HTTP 错误。测试推送按渠道分别回显结果；Telegram 通道失败时页面横幅会显示原因。

### GitHub Release 构建失败

先打开对应 Actions 运行记录。当前工作流中：

- Linux 使用 Bash、`zip` 和 `sha256sum`。
- Windows 使用 PowerShell、`Compress-Archive` 和 `Get-FileHash`。
- Linux amd64、Linux arm64、Windows amd64 成功后才会创建 Release。

构建警告不会自动等同于失败，应以每个 job 的 `conclusion` 和最终 Release 资产为准。

## 版本与发布流程

版本号统一保存在 `VERSION`，并同步到 `main.go` 和 `CHANGELOG.md`。当前版本为 `0.1.0`。

后续每次功能、修复或文档变更都递增版本号，例如从 `0.1.0` 到 `0.1.1`，完成验证后执行：

```bash
make check-version VERSION=0.1.1
go test ./...
git add .
git commit -m "..."
git push origin master
git tag -a v0.1.1 -m "Phoenix v0.1.1"
git push origin v0.1.1
```

推送版本 tag 后，GitHub Actions 会自动构建并发布：

- `phoenix_<version>_linux_amd64.zip`
- `phoenix_<version>_linux_arm64.zip`
- `phoenix_<version>_windows_amd64.zip`
- 每个平台对应的 `.sha256` 校验文件

GitHub Actions 不会发布当前工作流未构建的平台。发布完成后，应在 [Releases](https://github.com/Xiuyixx/Phoenix/releases) 中检查 tag、资产列表和校验文件。

## 参考与致谢

Phoenix 0.1.0 从 [PosvdM/cpa-plugin-lamplighter](https://github.com/PosvdM/cpa-plugin-lamplighter) 干净复刻而来，并系统改名为 Phoenix：额度管理页面、额度卡片、历史展示、自动点火概念、通知组织方式和 CLIProxyAPI 原生插件结构均以该项目为底本。reset credit 流程参考了 [mrchi/cpa-plugin-codex-auto-reset](https://github.com/mrchi/cpa-plugin-codex-auto-reset) 与 [JefferyZhang2019/cpa-plugin-codex-auto-reset](https://github.com/JefferyZhang2019/cpa-plugin-codex-auto-reset)（周用量窗口判定、credit 查询/消费、`host.routing.reset_cooldown` 回调思路）。感谢 Lamplighter 作者以及 CLIProxyAPI 社区提供的开源实现和经验。Phoenix 与 Lamplighter 互相独立，具体许可证、版权和第三方代码归属以各项目仓库为准。

## 许可证

本项目按 GNU AGPLv3 授权，见 `LICENSE`。使用或再分发 Phoenix 前，请同时遵守 CLIProxyAPI 以及所使用第三方依赖的许可证要求。
