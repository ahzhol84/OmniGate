# teaching_device_scaffold（教学插件脚手架）

这个插件是“教学用最小脚手架”，用于演示一个新插件应具备的完整骨架：

1. 正向隧道：HTTP 收数 -> 标准化 `DeviceData` -> 写入主通道。
2. 反向隧道：实现 `SendCommand` -> 设备认领 -> 命令回包。
3. 配置驱动：通过 `config.yaml` 的 `plugins` 配置管理启停。

## 文件说明

- `worker.go`
  - `Init`: 解析配置 + 默认值 + 冲突校验。
  - `Start`: 启动 HTTP 入口，接收上行并写入 `out`。
- `sendcommand.go`
  - `SendCommand`: 演示反控认领与回包模板。

## 如何从教学版改成生产版

1. 在 `sendcommand.go` 中把 echo 逻辑替换成真实厂商 API 调用。
2. 按厂商协议扩展 `ConfigItem` 字段（账号、密钥、base_url、超时等）。
3. 根据业务增加字段清洗、重试、幂等与监控日志。
