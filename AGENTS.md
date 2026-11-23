# Phoenix 开发说明

Phoenix 是 CLIProxyAPI（CPA）的原生插件，用 Go 编写，以 `-buildmode=c-shared`
构建动态库，由 CPA 加载。插件 id 为 `phoenix`，模块为
`github.com/Xiuyixx/Phoenix`，仓库为 https://github.com/Xiuyixx/Phoenix。

## 构建与测试

Go 工具链在 `~/workspace/.tools/go/bin`（Go 1.26.0），构建需要 cgo：

```bash
export PATH=$HOME/workspace/.tools/go/bin:$PATH
go test ./...
go vet ./...
gofmt -l .
node --check internal/web/page.js
node --test internal/web/page_test.cjs
make check-version   # 校验 VERSION、main.go 的 pluginVersion 默认值与 CHANGELOG 三处同步
make build           # 先跑测试与版本校验，再构建 dist/phoenix.so
make release         # 本机构建当前平台的 phoenix_<版本>_<系统>_<架构>.zip 与 checksums.txt
```

`main.go` 的版本变量是 `pluginVersion`，构建时用
`-ldflags "-X main.pluginVersion=<版本>"` 注入；默认值、`VERSION` 文件与
`CHANGELOG.md` 的条目必须保持一致。

## 约定

- 版本号只改三处：`VERSION`、`main.go` 的 `pluginVersion` 默认值、
  `CHANGELOG.md` 的 `## <版本> - <日期>` 条目。
- 代码注释只写当前状态（做什么、为什么），不写变更经过。
- README 与 `docs/` 同样只写当前状态；命令、路径与默认值对照代码核实。
- 修改页面时保持 HTML、CSS、JS 分文件；文字插入 DOM 时使用 `textContent`。
  新增或修改页面文字时，同时修改 `page.js` 中 `I18N` 的中文和英文词条；
  新增或修改推送文字时，同时修改 `internal/notify/text.go` 的两列。
- 修改宿主回调、RPC 字段或注册内容前，对照 CPA 源码中的
  `sdk/pluginabi`、`sdk/pluginapi` 和 `internal/pluginhost`。
- 后台循环中的 goroutine 必须有 `recover`；HTTP 请求设置超时。
- Phoenix 不带 logo：不要在页面、通知图标默认值或仓库里加回 logo 文件。
- 示例邮箱一律用 `user@example.com`，不要写真实邮箱、密钥或服务器地址。
