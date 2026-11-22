# Phoenix

Phoenix 是面向 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生插件，专注于额度监控、Telegram 推送、自动点火与 Codex 订阅渠道的 reset credit 自动重置。

## 当前功能

- 管理页面：优先采用 Lamplighter 的额度卡片、额度状态和“额度 / 设置”分栏思路。
- Telegram 推送：填写 Bot Token；多个 Chat ID 使用英文分号 `;` 分隔。
- 账号异常提醒：收到账号停用、认证失败（401/403）或限流/额度错误（429）时立即推送。
- 自动重置：订阅渠道周用量窗口耗尽、账号存在可用 reset credit 且满足设置条件时，选择最早到期的 credit 消耗；成功后清除 CPA 对凭证的本地冷却。
- 手动重置：管理页面中“立即重置”按钮位于“立即点火”按钮之前。
- 自动点火：支持配置启停、每日时间段和重置后延迟。

> 说明：reset credit 接口属于上游非官方接口，生产部署前请先在测试账号验证 CPA 版本、凭证结构和上游接口响应。Phoenix 不会重放原始请求；重置流程失败时保留 CPA 默认冷却行为。

## 安装与构建

```bash
go test ./...
go build -buildmode=c-shared -o phoenix.so .
```

将对应平台的共享库放入 CPA 插件目录，并在 CPA 中启用插件。具体插件目录和 `plugins.enabled` 配置请以所使用的 CLIProxyAPI 版本文档为准。

## 配置示例

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

Bot Token 属于敏感凭据，请勿提交到 Git 仓库。建议通过 CPA 的安全配置方式或部署环境注入。

## 参考与致谢

Phoenix 重新编写实现，未将参考项目作为运行时依赖；设计和兼容性研究参考了：

- [PosvdM/cpa-plugin-lamplighter](https://github.com/PosvdM/cpa-plugin-lamplighter)：管理页面布局、额度监控展示、点火操作与 CLIProxyAPI 原生插件结构。
- [mrchi/cpa-plugin-codex-auto-reset](https://github.com/mrchi/cpa-plugin-codex-auto-reset)：周用量窗口判定、reset credit 选择/消费、幂等处理和 `reset_cooldown` 主机回调思路。
- [JefferyZhang2019/cpa-plugin-codex-auto-reset](https://github.com/JefferyZhang2019/cpa-plugin-codex-auto-reset)：reset credit 状态机、额度读取与管理页面操作流程的实现思路。

感谢上述项目作者和 CLIProxyAPI 社区的工作。Phoenix 与上述项目互相独立，具体许可证和第三方代码归属以各参考项目仓库为准。
