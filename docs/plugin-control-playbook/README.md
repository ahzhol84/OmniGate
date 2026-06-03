# 插件反控实现指南（给新对话 Agent）

## 1. 目标

本指南用于在新插件中快速实现“反控（下行控制）”能力。
核心目标：

1. 从统一下行入口拿到指令；
2. 仅处理“属于本插件设备”的指令；
3. 调用厂商 API 执行反控；
4. 返回可被 IoT 物模型直接展示/提取的结果。

---

## 2. 统一链路（必须理解）

当前中间件下行链路：

1. Hub 接收 `POST /device/command`；
2. 调用 `command.Dispatch(device_id, command)`；
3. 依次调用各插件 `SendCommand`；
4. 插件若不处理该设备，返回 `ErrCommandNotSupported`；
5. 第一个“认领并处理”的插件返回成功或业务错误。

所以插件实现重点是：**设备归属判定 + API 调用 + 标准回复**。

---

## 3. 新插件要做的最小改动

### 3.1 在插件中实现 ICommandable

在插件 worker 上实现：

- `SendCommand(ctx, deviceID, cmd) (*CommandReply, error)`

要求：

- 非本插件指令：返回 `ErrCommandNotSupported`；
- 本插件执行失败：返回详细错误（不要吞）；
- 本插件执行成功：返回 `CommandReply{code:0}`。

### 3.2 配置项

在插件配置中补齐反控所需字段（按厂商 API 定义）：

- `api_base_url`
- `username` / `password` 或 token
- 业务默认参数（如 network/type/index）

### 3.3 设备映射

必须支持 `unique_id -> vendor_device_id` 解析，建议优先级：

1. 独立映射表 `device_id_mapping`；
2. 回退 `device_data` 最近记录解析；
3. 若传入值本身就是厂商设备号（如 15 位 IMEI），直接使用。

### 3.4 厂商 API 封装

将 API 访问封装成独立函数，例如：

- 登录函数（拿 cookie/token）
- 控制函数（设置参数、触发控制）

不要把 HTTP 细节散落在多个位置。

### 3.5 返回体规范（给 IoT 展示）

成功时建议返回：

- `code = 0`
- `message = 参数回显 JSON 字符串`
- `data = 参数回显对象`

参数回显至少包含：

- `source_id`（原始 unique_id 或输入设备标识）
- `device_id`（最终解析的厂商设备号）
- 本次下发关键参数（如 `name`、`num`、`index` 等）

这样 IoT 侧可直接展示，不依赖固定 `ok`。

---

## 4. 日志要求（强烈建议）

至少保留以下日志点：

1. 收到下行：`req/source_device/method/identifier`
2. 不支持：`unsupported_payload`
3. 映射结果：`source_device -> resolved_device` + 映射来源
4. 厂商调用失败：HTTP 状态码 + body 摘要
5. 厂商调用成功：关键参数回显

日志目的：线上快速定位“映射问题 / 参数问题 / API 问题”。

---

## 5. 数据一致性建议

上报路径中同步维护映射关系（upsert）：

- key: `(plugin_name, unique_id)`
- value: `vendor_device_id`

不要只依赖 `device_data`，否则清理历史数据后会丢反控能力。

---

## 6. 实施清单（可直接复用）

1. 新增/确认插件配置字段；
2. 实现 `SendCommand`；
3. 完成指令参数解析（支持物模型字段）；
4. 完成设备映射解析（映射表优先）；
5. 封装厂商 API 调用；
6. 返回参数回显结构；
7. 增加关键日志；
8. 编译与最小联调（至少一次成功、一次失败路径）。

---

## 7. 联调验收标准

满足以下条件才算完成：

1. IoT 下发后，插件能稳定命中本设备；
2. 厂商侧配置确实变化（用厂商查询接口验证）；
3. IoT 响应中可直接看到本次下发参数回显；
4. 日志能完整追踪一次请求（recv -> mapped -> send ok/failed）。

---

## 8. 给新对话 Agent 的一句话任务模板

请在目标插件中实现反控能力：从统一下行通道接收指令，识别本插件设备并解析 unique_id 到 vendor_device_id，按厂商 API 文档调用控制接口，返回参数回显给 IoT 展示，并补齐映射持久化与关键日志。