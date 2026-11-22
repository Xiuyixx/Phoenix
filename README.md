# Phoenix

Phoenix 是一个面向 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生插件，围绕 Codex 订阅凭证提供额度状态、账号错误通知、Telegram 推送、reset credit 自动重置以及管理页面操作入口。

项目名称取自“重生”：当周用量窗口耗尽且账号仍有 reset credit 时，Phoenix 可以消耗一张 credit，清除 CLIProxyAPI 对该凭证的本地冷却，让凭证恢复可调度状态。

> 当前版本：`0.0.6`。项目仍在持续开发中，安装到生产 CPA 前建议先使用测试凭证验证上游 reset credit 接口和 CPA 版本兼容性。

## 主要功能

### 额度与账号状态

Phoenix 通过两条路径收集额度数据：主动轮询按 `poll_interval_seconds`（默认 300 秒）枚举 Codex 凭证并查询上游 `/wham/usage`，按窗口时长识别周窗口；被动接收 CPA 的 usage 回调，成功记为正常、`usage_limit_reached` 的 429 记为周窗口耗尽并记录重置时间。页面新增“最近事件”列表，账号异常、重置与点火结果都会记录在内。

额度数据是否完整取决于 CPA 版本、凭证类型以及上游响应中是否包含可解析的额度字段。没有收到有效额度数据时，页面会显示等待状态，不会把缺失数据当成 100% 或 0% 使用量。

### Telegram 推送

设置 Telegram Bot Token 和 Chat ID 后，Phoenix 可以向一个或多个接收者发送提醒。多个 Chat ID 使用英文分号分隔，例如：

```text
123456789;987654321;-1001234567890
```

当前通知场景包括：

- 账号返回 `401` 或 `403`，通常表示凭证失效、账号未授权或账号被停用。
- 账号返回 `429`，通常表示限流或额度耗尽。
- 自动重置成功。
- 测试推送。

Bot Token 只用于调用 Telegram Bot API，不写入 Git 仓库。配置文件和管理接口中的 Token 应按敏感信息保护。

### 自动重置

自动重置只针对 Codex 订阅渠道的周用量窗口。触发条件为：

1. 请求失败状态为 `429`。
2. 错误类型为 `usage_limit_reached`。
3. 凭证属于 Codex。
4. 窗口是周窗口，通常由 `limit_window_minutes: 10080` 表示；缺少显式窗口字段时，根据剩余时间进行保守判断。
5. 剩余恢复时间达到配置的最低秒数，默认用于排除短时 5 小时窗口。
6. 凭证在 Phoenix 的纳入列表中，或当前配置允许自动处理全部凭证。
7. 上游返回至少一张可用的 reset credit。

命中后，Phoenix 会：

1. 通过 CPA host 回调读取凭证。
2. 提取 access token 和 ChatGPT account ID。
3. 查询 `rate-limit-reset-credits`。
4. 选择可用且最早到期的 credit。
5. 生成一次性的幂等请求 ID。
6. 调用 credit consume 接口。
7. 只有上游返回 `reset` 或 `already_redeemed` 时才视为 credit 操作成功。
8. 调用 CPA 的 `host.routing.reset_cooldown` 清除本地凭证冷却。
9. 记录日志并发送 Telegram 通知。

消费 credit 后如果清除 CPA 冷却失败，Phoenix 会记录警告，不会重复消费 credit。网络错误、凭证缺失、没有可用 credit 或上游返回其他结果时，不会伪造成功状态。

### 手动重置与点火入口

管理页面的“立即操作”卡片内操作顺序固定为：

1. `立即重置`
2. `立即点火`

这样可以先处理周额度和本地冷却，再执行后续点火操作。按钮请求会被记录到 CPA 日志，页面会反馈请求已提交。

点火通过宿主的模型执行回调发送一次最小 Codex 请求（只要求回复 `OK`，不推理、不用工具），从而开始一个新的 5 小时窗口。自动点火在每日开始—结束时段内每个凭证最多执行一次，重置成功后也会按配置的延迟自动点火一次。点火模型默认 `gpt-5-codex`，可在设置页修改。

## 兼容性与构建平台

Phoenix 使用 CLIProxyAPI v8 插件 SDK，并通过 c-shared 动态库加载。

当前 GitHub Release 已验证并发布：

- Linux amd64：`linux/amd64`，文件扩展名 `.so`。
- Linux arm64：`linux/arm64`，文件扩展名 `.so`。
- Windows amd64：`windows/amd64`，文件扩展名 `.dll`。

当前 Release 工作流没有发布 macOS 构建包，因为 GitHub 账户的 macOS runner 调度不稳定。macOS 可以在具备 Go、CGO 和对应编译工具链的机器上自行构建；构建结果必须使用 CPA 所在系统对应的动态库格式和架构。

建议使用 CLIProxyAPI v8.0.12 或更高版本，因为自动重置清除冷却依赖 `host.routing.reset_cooldown` 回调；本项目当前依赖并验证过的 SDK 版本为 v8.0.36。最终兼容性仍以实际 CPA 版本、运行平台和插件 ABI 为准。

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

打开 [Phoenix Releases](https://github.com/Xiuyixx/Phoenix/releases)，选择目标版本，例如 `v0.0.6`，下载与 CPA 运行平台匹配的压缩包和对应 `.sha256` 文件。

Linux amd64 示例：

```bash
VERSION=0.0.6
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
$version = "0.0.6"
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

当前 Release 只提供前三种已验证资产。不要把 Linux `.so` 放到 Windows 或 macOS 目录，也不要把 `arm64` 文件放到 `amd64` 目录。

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
      telegram:
        enabled: true
        bot_token: "<BOT_TOKEN>"
        chat_ids: "123456789;987654321"
      reset:
        enabled: true
        weekly_only: true
        minimum_reset_seconds: 86400
      ignition:
        enabled: true
        start_hour: 7
        end_hour: 22
        grace_seconds: 3
```

建议先使用以下最小配置确认插件能被 CPA 加载：

```yaml
plugins:
  enabled: true
  configs:
    phoenix:
      enabled: true
```

确认加载成功后，再通过管理页面配置 Telegram 和自动重置。这样可以把“插件加载问题”和“上游通知/重置接口问题”分开排查。

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
- 使用“测试推送”确认 Telegram 收件人能收到消息。

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
| `telegram.enabled` | 是否启用 Telegram 通知。 |
| `telegram.bot_token` | Telegram Bot Token。 |
| `telegram.chat_ids` | 收件人 Chat ID，多个值用英文分号分隔。 |
| `reset.enabled` | 是否允许自动和手动重置流程。 |
| `reset.weekly_only` | 只处理周用量窗口，建议开启。 |
| `reset.minimum_reset_seconds` | 触发自动重置所需的最少剩余秒数，默认 `86400`。 |
| `reset.credentials` | 纳入重置的凭证（auth_index 或 auth_id），多个值用分号或 YAML 列表表示，留空为全部 Codex 凭证。 |
| `ignition.enabled` | 自动点火开关。 |
| `ignition.start_hour` | 自动点火每日开始小时。 |
| `ignition.end_hour` | 自动点火每日结束小时。 |
| `ignition.grace_seconds` | 额度窗口重置后等待的秒数。 |
| `ignition.model` | 点火使用的模型，默认 `gpt-5-codex`。 |
| `poll_interval_seconds` | 主动额度查询间隔（秒），默认 `300`，最小 `60`。 |

设置页面可以修改 Telegram、重置和点火相关字段，保存时按字段合并，不会清空页面不展示的配置。页面保存立即生效，但只保存在内存里，CPA 重启后会恢复为 `config.yaml` 中的配置；需要长期生效的配置请写进 `config.yaml`。修改后应重新读取页面状态确认保存结果，并查看 CPA 日志确认插件已经应用配置。

## 安全与运行边界

- 不要把 Bot Token、access token、完整凭证 JSON 或 CPA 管理密钥提交到 Git。
- 自动重置会消耗真实 reset credit。建议先使用测试账号，并保持 `weekly_only: true`。
- Phoenix 不会重放触发自动重置的原始请求。
- credit consume 已发送后，即使网络响应异常，也不能安全地自动重复消费；流程会停止并记录日志。
- 清除 CPA 本地冷却失败时，Phoenix 会报告失败，不会继续消耗第二张 credit。
- reset credit 接口是上游非官方接口，接口字段或策略变化可能导致流程失败，需要根据 CPA 日志处理。
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

这通常表示插件已加载，但 CPA 尚未产生可识别的 usage 记录，或上游响应没有可解析的额度字段。先通过 CPA 发起一次 Codex 请求，再查看 Phoenix 页面和 CPA 日志。

### Telegram 没有收到消息

检查 Bot Token 是否完整、Chat ID 是否正确、多个 ID 是否使用英文分号、服务器是否可以访问 `https://api.telegram.org`。先点击“测试推送”，再检查 CPA 日志中的 HTTP 错误。

### 自动重置没有执行

检查：

- `reset.enabled` 是否为 `true`。
- `weekly_only` 是否配置正确。
- 触发账号是否被纳入 `reset.credentials`，如果配置了白名单。
- 错误体是否包含 `usage_limit_reached`。
- 触发窗口是否确实是周窗口。
- access token 是否仍然有效。
- 上游是否返回可用 reset credit。
- CPA 是否支持 `host.routing.reset_cooldown`。

### GitHub Release 构建失败

先打开对应 Actions 运行记录。当前工作流中：

- Linux 使用 Bash、`zip` 和 `sha256sum`。
- Windows 使用 PowerShell、`Compress-Archive` 和 `Get-FileHash`。
- Linux amd64、Linux arm64、Windows amd64 成功后才会创建 Release。

构建警告不会自动等同于失败，应以每个 job 的 `conclusion` 和最终 Release 资产为准。

## 版本与发布流程

版本号统一保存在 `VERSION`，并同步到 `main.go` 和 `CHANGELOG.md`。当前版本为 `0.0.6`。

后续每次功能、修复或文档变更都递增版本号，例如从 `0.0.6` 到 `0.0.7`，完成验证后执行：

```bash
make check-version VERSION=0.0.6
go test ./...
git add .
git commit -m "..."
git push origin master
git tag -a v0.0.6 -m "Phoenix v0.0.6"
git push origin v0.0.6
```

推送版本 tag 后，GitHub Actions 会自动构建并发布：

- `phoenix_<version>_linux_amd64.zip`
- `phoenix_<version>_linux_arm64.zip`
- `phoenix_<version>_windows_amd64.zip`
- 每个平台对应的 `.sha256` 校验文件

GitHub Actions 不会发布当前工作流未构建的平台。发布完成后，应在 [Releases](https://github.com/Xiuyixx/Phoenix/releases) 中检查 tag、资产列表和校验文件。

## 参考与致谢

Phoenix 为独立重写实现，未将下列项目作为运行时依赖。设计、CLIProxyAPI 插件结构、管理页面和 Codex reset credit 流程研究参考了：

- [PosvdM/cpa-plugin-lamplighter](https://github.com/PosvdM/cpa-plugin-lamplighter)：额度管理页面、额度卡片、历史展示、自动点火概念、通知组织方式和 CLIProxyAPI 原生插件结构。
- [mrchi/cpa-plugin-codex-auto-reset](https://github.com/mrchi/cpa-plugin-codex-auto-reset)：周用量窗口判定、认证与凭证边界、reset credit 查询/消费、幂等请求以及 `host.routing.reset_cooldown` 回调思路。
- [JefferyZhang2019/cpa-plugin-codex-auto-reset](https://github.com/JefferyZhang2019/cpa-plugin-codex-auto-reset)：reset credit 状态机、额度读取、重置确认和管理操作流程的实现思路。

感谢上述项目作者以及 CLIProxyAPI 社区提供的开源实现和经验。Phoenix 与这些项目互相独立，具体许可证、版权和第三方代码归属以各参考项目仓库为准。

## 许可证

Phoenix 当前仓库以源码和发布包为主要交付形式。使用或再分发 Phoenix 前，请同时遵守 CLIProxyAPI、参考项目以及所使用第三方依赖的许可证要求。
