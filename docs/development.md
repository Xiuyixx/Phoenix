# 开发

## 环境

- Go 1.26（与 CPA v8 一致）。
- 构建动态库需要 cgo 和目标平台的 C 编译器。Linux 版本在 `golang:1.26-bookworm` 中构建，与 CPA 官方镜像（Debian 12）使用相同的 glibc；macOS 版本用 Xcode 的 clang，Windows 版本用 MinGW-w64 的 gcc。
- 运行测试不需要 cgo。`internal/` 下的包都是纯 Go，可以在 Windows、macOS 或 Linux 上测试；只有根目录的 `main.go` 需要 cgo。

## 测试

```bash
go vet ./internal/...
go test ./internal/...
go test -race ./internal/...
node --check internal/web/page.js
node --test internal/web/page_test.cjs
```

测试使用假的宿主和 HTTP 服务器，不会发送真实请求。

推送到 `main` 和 PR 时，GitHub Actions（`.github/workflows/ci.yml`）在 Linux 上检查 `gofmt -l .` 的输出为空，并运行 vet、普通测试和 Node.js 语法检查；race 检查用于本地验证。只改动 Markdown 或 `docs/` 的提交不触发。同一分支的新提交会取消仍在运行的旧任务。

测试覆盖：

- 各服务（Codex、Claude、Antigravity、Kimi、Grok）的主动额度解析、被动响应头解析和窗口周期，窗口种类判断，以及 Kimi 的接口域名选择；
- 网络出口选择，包括无效 `proxy_url` 不退回直连；
- 点火时间、滑动重置判断、错误分类和失败保护；
- 候选模型顺序和模型列表读取；
- 点火请求无输出 token 上限、Claude/Responses SSE 分片和宿主 Chat JSON chunk 解析、首次可见输出计时、阻塞读取期间关闭上游、自然结束及真实错误与 deadline 同时就绪时的确定性终止仲裁、取消/停止/watchdog panic 的所有权和关闭错误优先级、单次关闭及 watchdog 退出、主动断流不换模型且仍须确认窗口，以及日志中的 Unicode 截断、宿主 usage 来源、转换后补零或缺少推理细分时的保守完整性标记、Claude/Codex 原生最终 usage 和敏感字段过滤；
- 额度提醒、按窗口的恢复通知和重置提醒、套餐变化后重新记录基线并保留已发送的重置提醒、Bark 与 Telegram 请求格式、Phoenix 通知文案（13 种示例逐字断言），通知中日期时间的格式（带星期），以及 Did Codex Reset 的去重、过滤和排期时间，判断排期更新用的内容不带星期；
- webhook 的占位符转义、内置请求体、`success_json`、设置检查和错误中的密钥隐藏，多渠道发送时只要一个渠道成功就算送达，以及没有渠道时关闭推送、只有无效的 webhook 或 Telegram 时按失败重试；reset credit 全链模拟（查列表→选最早过期可用 credit→redeem→清冷却→宽限后自动点火；手动重置不点火；5 分钟防重复；consume 只发一次），以及模型 key 经插件路由校验；
- 引擎：账号后缀和名称、被动数据跳过规则、被动数据不新增窗口、早于当前读数的被动数据被丢弃且不重复提醒、重置前后的查询、点火确认、换模型重试、Kimi 别名账号的识别、Kimi 和 Grok 不点火、暂停通知、历史采样、历史接口的窗口筛选、分段长度和范围开始前的最后一个样本、状态接口的服务列表和新版本、新版本检查默认关闭（手动触发）及其 1 分钟限流和不等待进行中的读取，账号套餐的识别和每天一次的 Claude 账号资料请求，以及额度窗口增减后（包括重启后）重读套餐、读取失败时暂缓额度提醒（从未读到套餐的账号不暂缓）、连续失败 1 小时后恢复额度提醒并在 15 分钟后再读套餐、套餐变化的通知及其重试、通知在 1 小时后放弃和送达前变回原套餐时取消、不受影响的其他账号；
- 版本号比较，以及读取最新 Release 时的成功、限流、无效响应和非数字标签；
- 配置中 `account_weights` 的校验，Codex、Claude 和 Antigravity 的套餐名称和默认权重；
- 插件 RPC：注册、管理接口、页面、状态中不含密钥；
- 实例锁互斥，以及从 CPA 配置读取插件目录。

## 构建

```bash
make check-version
make build
```

产物为 `dist/phoenix.so`。`make build` 先跑测试与版本校验，再用
`CGO_ENABLED=1 go build -trimpath -buildmode=c-shared` 构建。本地 Release 包用
`make release`，产物为 `dist/phoenix_<版本>_<系统>_<架构>.zip` 与
`dist/checksums.txt`，zip 根目录只有 `phoenix.so`。

## 发布

推送 `v*.*.*` 标签后，GitHub Actions（`.github/workflows/release.yml`）：

1. 按标签解析版本号，并校验它与 `VERSION` 和 `CHANGELOG.md` 的条目一致；
2. 在三个平台上构建：Linux amd64、Linux arm64 在 Ubuntu runner 上构建，
   Windows amd64 在 Windows runner 上构建（PowerShell）；
3. 每个平台打包为 `phoenix_<版本>_<系统>_<架构>.zip`，zip 内含
   `phoenix.so`（或 `phoenix.dll`）与 `VERSION`，并生成对应的 `.sha256` 文件；
4. 三个平台构建成功后创建 GitHub Release，说明为自动生成的提交列表。

在 Actions 页面手动运行 `release` 时，输入不带 `v` 的版本号，只构建并上传
Actions 产物，不创建 Release。

版本号来自标签（`v0.1.0` 对应 `0.1.0`），通过
`-ldflags "-X main.pluginVersion=..."` 写入插件，并显示在管理中心和页面上。
`VERSION`、`main.go` 的 `pluginVersion` 默认值与 `CHANGELOG.md` 的条目必须
保持一致，`make check-version` 校验这三处。

## 修改约定

- 修改宿主回调、RPC 字段或注册内容前，对照 CPA 源码中的 `sdk/pluginabi`、`sdk/pluginapi` 和 `internal/pluginhost`。`go.mod` 中的 CPA 版本与线上最低支持版本保持一致。
- 升级 `go.mod` 中的 CPA 版本时，对照 CPA 的 `internal/runtime/executor` 更新 `internal/providers` 中 Codex 和 Claude 的 User-Agent 常量。插件无法在运行时读取 CPA 编译进去的常量。
- 修改额度请求的地址或请求头时，对照 CPA 管理中心的 `src/utils/quota/constants.ts`，并更新 [架构文档](./architecture.md) 中的表格。
- 修改轮询、跳过规则、点火时间或失败保护时，同步更新测试、README 和架构文档。
- 后台循环中的 goroutine 必须有 `recover`，取消监视器必须在调用结束前退出。HTTP 请求设置超时；模型启动阶段的取消能力受 CPA SDK 限制，见[流式执行与取消](./architecture.md#流式执行与取消)，不能用遗留后台请求的超时返回替代真实取消。
- 修改页面时保持 HTML、CSS、JS 分文件；文字插入 DOM 时使用 `textContent`。新增或修改页面文字时，同时修改 `page.js` 中 `I18N` 的中文和英文词条；新增或修改推送文字时，同时修改 `internal/notify/text.go` 的两列。增加一种语言时，两处各加一份。图表颜色只表示剩余额度的高低，使用与额度条相同的三档变量。
- 用户可见的行为、配置和限制写进 README，实现细节写进 `docs/`。

## 真实环境验证

点火会发送真实模型请求并开始新的 5 小时窗口。在生产 CPA 上首次部署时，先把 `ignition.enabled` 设为 `false`，确认额度、被动数据、页面和通知正常后再开启。
