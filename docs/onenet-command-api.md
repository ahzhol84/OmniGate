# OneNET 反控 API 文档（基于 Go Hub）

## 1. 适用范围

本文档面向“非 IoT 模块”调用 Go Hub 下行反控接口，使用 OneNET 插件能力进行设备控制。

- 目标：调用方只需要知道设备 `unique_id` 和反控参数
- 无需关心：厂商 `device_name`、映射表、OneNET 鉴权签名细节

---

## 2. 接口信息

- 方法：`POST`
- 地址：`http://47.94.14.70:8088/device/command`
- 鉴权头：`X-Iot-Token: <token>`（按配置开启）
- Content-Type：`application/json`

### 2.1 当前环境真实 token 状态

基于当前配置文件 `iot-middleware/config.yaml`：

- `realtime.auth_header = "X-Iot-Token"`
- `realtime.auth_token = ""`

结论：当前环境 **未启用 token 校验**，即“当前可用 token”为 **空字符串**（可不传 `X-Iot-Token`）。

> 说明：后续如果配置了非空 `auth_token`，调用方必须携带正确的 `X-Iot-Token`。

---

## 3. 统一请求体（标准）

```json
{
  "unique_id": "jlk|onenet|ONENET|867369079784279",
  "request_id": "req-onenet-property-family-numbers-001",
  "method": "thing.property.set",
  "identifier": "family_numbers",
  "params": {
    "family_numbers": "13068040314,1,18435115041,2"
  }
}
```

### 3.1 字段定义

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `unique_id` | string | 是（推荐） | 业务设备唯一标识。Hub 优先用该字段路由。 |
| `device_id` | string | 否 | 兼容旧调用；仅当 `unique_id` 为空时才使用。 |
| `request_id` | string | 否 | 请求追踪 ID（仅用于链路追踪，不决定反控功能）。建议传。 |
| `method` | string | 是 | `thing.property.set` 或 `thing.service.invoke`。 |
| `identifier` | string | 否 | 属性/服务标识。 |
| `params` | object | 是 | 反控参数对象。 |

`request_id` 命名建议：`req-<渠道>-<method>-<identifier>-<序号>`，例如：`req-onenet-property-report-interval-001`。

### 3.2 路由规则（调用方可忽略实现细节）

1. Go Hub 取目标 ID：优先 `unique_id`，否则 `device_id`。
2. OneNET 插件识别设备归属（channel 标识、历史数据特征等）。
3. 插件将 `unique_id` 解析为 OneNET `device_name`（映射表优先，历史数据回退）。
4. 插件调用 OneNET 开放接口并返回标准回包。

---

## 4. 返回结果

### 4.1 成功响应（HTTP 200）

```json
{
  "request_id": "req-onenet-property-family-numbers-001",
  "code": 0,
  "message": "{...JSON回显字符串...}",
  "data": {
    "plugin": "onenet_http_push_listener",
    "action": "set_device_property",
    "source_device_id": "jlk|onenet|ONENET|867369079784279",
    "vendor_device_name": "867369079784279",
    "resolved_device_from": "device_id_mapping",
    "product_id": "5POpr2bk2g",
    "endpoint": "https://iot-api.heclouds.com/thingmodel/set-device-property",
    "request": {
      "product_id": "5POpr2bk2g",
      "device_name": "867369079784279",
      "params": {
        "family_numbers": [
          { "phone": "13068040314", "type": 1 },
          { "phone": "18435115041", "type": 2 }
        ]
      }
    },
    "response": {
      "request_id": "req-onenet-property-family-numbers-001",
      "msg": "succ"
    }
  }
}
```


### 4.2 失败响应

- `400`：请求体非法，或 `unique_id` / `device_id` 同时缺失
- `401`：鉴权失败
- `502`：无插件认领或插件执行失败

典型业务错误（`code=-1`）：

- `onenet command config missing: cmiot_user_id/cmiot_access_key`
- `onenet command config missing: cmiot_product_id`
- `service identifier required`
- `onenet api failed code=... msg=...`

---

## 5. 参数目录（字段名 + 数据类型）

### 5.0 全量能力矩阵（当前插件代码实装）

| method | identifier | OneNET 实际接口 | 说明 |
|---|---|---|---|
| `thing.property.set` | `family_numbers` | `/thingmodel/set-device-property` | 家庭联系人 |
| `thing.property.set` | `emergency_contact` | `/thingmodel/set-device-property` | 紧急联系人 |
| `thing.property.set` | `function_keys` | `/thingmodel/set-device-property` | 功能键设置 |
| `thing.property.set` | `location` | `/thingmodel/set-device-property` | 位置信息设置 |
| `thing.property.set` | `timer_tasks` | `/thingmodel/set-device-property` | 定时任务 |
| `thing.property.set` | `report_interval` | `/thingmodel/set-device-property` | 上报间隔 |
| `thing.property.set` | *(空)* | `/thingmodel/set-device-property` | 批量属性下发（自动过滤允许字段） |
| `thing.service.invoke` | `leave_message_add` | `/thingmodel/call-service` | 留言新增 |
| `thing.service.invoke` | `leave_message_text_add` | `/thingmodel/call-service` | 文本留言新增 |
| `thing.service.invoke` | `leave_message_clear` | `/thingmodel/call-service` | 留言清空 |

## 5.1 属性下发 `thing.property.set`

### 可用 identifier

- `family_numbers`
- `emergency_contact`
- `function_keys`
- `location`
- `timer_tasks`
- `report_interval`

### 各 identifier 参数定义

#### 1) `family_numbers`

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `family_numbers` | string \| array \| object | 是 | 联系人集合。插件会规范化为数组对象。 |

支持格式示例：

- CSV 字符串：`"13068040314,1,18435115041,2"`
- JSON 数组字符串：`"[{\"phone\":\"13068040314\",\"type\":1}]"`
- 对象数组：`[{"phone":"13068040314","type":1}]`

#### 2) `function_keys`

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `function_keys` | array \| string | 是 | 功能键配置，元素结构为 `{function:int, param:int}`。 |

#### 3) `location`

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `location` | object \| string | 是 | 位置信息，最终格式 `{longitude:string, latitude:string}`。 |

也可由 params 中 `longitude/lon + latitude/lat` 自动组装。

#### 4) `report_interval`

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `report_interval` | number \| string | 是 | 上报间隔，插件会转为整数。 |

#### 5) 其他属性

| identifier | params 类型 | 说明 |
|---|---|---|
| `emergency_contact` | object | 紧急联系人 |
| `timer_tasks` | array | 定时任务 |

### 5.1.1 联系人清空规则（`family_numbers` / `emergency_contact`）

结论：这两类手机号都**没有独立 clear 接口**。

必须使用 `thing.property.set`，并且显式传对应字段为空值：

- 清空 `family_numbers`：`params.family_numbers = ""`
- 清空 `emergency_contact`：`params.emergency_contact = ""`

注意：不传该字段不等于清空；不传表示“不修改该字段”。

---

## 5.2 服务调用 `thing.service.invoke`

### 可用 identifier

- `leave_message_add`
- `leave_message_text_add`
- `leave_message_clear`

### 参数

- `params`：object（按业务服务定义透传）

---

## 6. 调用示例

### 6.0 示例字段速查（看示例前先看这段）

| 字段 | 中文说明 | 典型取值 | 备注 |
|---|---|---|---|
| `unique_id` | 设备唯一标识（推荐） | `867369079784279` | Hub 优先按它路由到设备。 |
| `request_id` | 请求追踪号 | `req-onenet-xxx-001` | 建议全局唯一，便于日志检索。 |
| `method` | 操作类型 | `thing.property.set` / `thing.service.invoke` | 属性下发或服务调用。 |
| `identifier` | 属性名或服务名 | `family_numbers` / `leave_message_text_add` | 必须是插件支持的标识。 |
| `params` | 具体业务参数 | 对象 | 按 `identifier` 定义填写。 |

常见服务参数补充：

- `mid`：留言 ID，业务自定义，建议唯一。
- `url`：语音文件地址；纯文字可传空字符串 `""`。
- `format`：语音格式，`0=amr`，`1=mp3`。
- `text`：文字留言内容（仅 `leave_message_text_add` 需要）。

### 6.1 属性下发（`thing.property.set`）

#### 示例 A1：`family_numbers`

用途：设置“亲情联系人号码列表”，设备拨号类功能会用到该列表。

关键字段说明：

- `identifier=family_numbers`：指定要修改“亲情联系人”属性。
- `params.family_numbers`：联系人内容，可用 CSV（号码,类型,号码,类型）。

成功判定：返回 `code=0`，且设备后续上报属性中能看到 `family_numbers` 新值。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-property-family-numbers-001",
    "method": "thing.property.set",
    "identifier": "family_numbers",
    "params": {
      "family_numbers": "13068040314,1,18435115041,2"
    }
  }'
```

#### 示例 A2：`emergency_contact`

用途：设置“紧急联系人”，按键求救或重呼流程优先使用该号码列表。

关键字段说明：

- `identifier=emergency_contact`：指定紧急联系人属性。
- `params.emergency_contact`：紧急号码，通常用逗号分隔字符串。

成功判定：返回 `code=0`，且设备属性上报出现新的 `emergency_contact`。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-property-emergency-contact-001",
    "method": "thing.property.set",
    "identifier": "emergency_contact",
    "params": {
      "emergency_contact": "13068040314,18435115041"
    }
  }'
```

#### 示例 A2-1：清空 `emergency_contact`

用途：清空紧急联系人号码。

关键字段说明：

- `identifier=emergency_contact`
- `params.emergency_contact=""`（空字符串）

成功判定：返回 `code=0`，且设备后续上报中 `emergency_contact` 为空。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-property-emergency-contact-clear-001",
    "method": "thing.property.set",
    "identifier": "emergency_contact",
    "params": {
      "emergency_contact": ""
    }
  }'
```

#### 示例 A1-1：清空 `family_numbers`

用途：清空亲情联系人号码。

关键字段说明：

- `identifier=family_numbers`
- `params.family_numbers=""`（空字符串）

成功判定：返回 `code=0`，且设备后续上报中 `family_numbers` 为空数组/空值。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079608726",
    "request_id": "req-onenet-property-family-numbers-clear-001",
    "method": "thing.property.set",
    "identifier": "family_numbers",
    "params": {
      "family_numbers": ""
    }
  }'
```

#### 示例 A3：`function_keys`

用途：配置设备功能键（左右键）按下后的动作。

关键字段说明：

- `identifier=function_keys`：指定功能键配置属性。
- `params.function_keys`：数组顺序对应按钮顺序，每项含 `function` 与 `param`。

成功判定：返回 `code=0`，并在设备上按键触发对应动作。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-property-function-keys-001",
    "method": "thing.property.set",
    "identifier": "function_keys",
    "params": {
      "function_keys": [
        {"function": 0, "param": 0},
        {"function": 1, "param": 0}
      ]
    }
  }'
```

规则说明（就以上面这组参数）：

- `function_keys` 数组按“按钮顺序”生效：第 1 个元素对应 1 号按钮，第 2 个元素对应 2 号按钮。
- `function` 取值含义：`-1`=无动作，`0`=拨号，`1`=收听语音留言，`2`=挂断电话，`3`=按键打卡，`4`=播报当前时间和温度。
- `param` 仅在 `function=0`（拨号）时生效，表示拨打哪个亲情联系人；其它 `function` 下通常忽略。
- 因此当前示例会得到：
  - 1 号按钮：`{"function":0,"param":0}` → 拨号（第 1 个亲情联系人）。
  - 2 号按钮：`{"function":1,"param":0}` → 收听语音留言。

如果希望 2 号按钮拨打“第二个亲情联系人”，通常可将第二项改为 `{"function":0,"param":1}`（以设备端实际配置解释为准）。

#### 示例 A3-1：两个按钮分别对应两个手机号（独立示例）

用途：一次请求同时完成“联系人配置 + 按键映射”，避免只改一半导致拨号目标错位。

关键字段说明：

- `params.family_numbers`：先定义第 1/2 个亲情号码。
- `params.function_keys`：再把按钮 1、2 分别映射到对应号码索引。

成功判定：返回 `code=0`，按钮 1/2 分别呼叫第 1/2 个亲情号码。

> 这个场景建议“一步下发”联系人和按键映射，避免只改按键不改联系人导致拨号目标不一致。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079608726",
    "request_id": "req-onenet-property-buttons-bind-phones-001",
    "method": "thing.property.set",
    "params": {
      "family_numbers": "13068040314,1,18435115041,2",
      "function_keys": [
        {"function": 0, "param": 0},
        {"function": 0, "param": 1}
      ]
    }
  }'
```

执行结果（按当前规则理解）：

- 1 号按钮：拨打第 1 个亲情联系人（`13068040314`）
- 2 号按钮：拨打第 2 个亲情联系人（`18435115041`）

当前设备实测：`param` 采用 0 基索引（`0`=第 1 个号码，`1`=第 2 个号码）。

#### 示例 A4：`location`

用途：设置设备安装位置经纬度（用于天气播报等能力）。

关键字段说明：

- `identifier=location`：指定位置属性。
- `params.location.longitude/latitude`：经纬度字符串。

成功判定：返回 `code=0`，并在属性上报中看到 `location` 更新。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079608726",
    "request_id": "req-onenet-property-location-001",
    "method": "thing.property.set",
    "identifier": "location",
    "params": {
      "location": {
        "longitude": "112.571321",
        "latitude": "37.793533"
      }
    }
  }'
```

#### 示例 A5：`timer_tasks`

用途：下发定时提醒任务（例如每天固定时间语音提醒）。

关键字段说明：

- `identifier=timer_tasks`：指定定时任务属性。
- `params.timer_tasks[]`：任务列表，常用字段 `week/hour/min/text/url/times`。

成功判定：返回 `code=0`，到达指定时间设备执行提醒。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-property-timer-tasks-001",
    "method": "thing.property.set",
    "identifier": "timer_tasks",
    "params": {
      "timer_tasks": [
        {
          "week": 127,
          "hour": 20,
          "min": 0,
          "disable": 0,
          "type": 0,
          "times": 3,
          "text": "5Lya5oyJ5pe25ZCD6I2v",
          "url": ""
        }
      ]
    }
  }'
```

#### 示例 A6：`report_interval`

用途：调整设备“状态上报周期”（单位分钟）。

关键字段说明：

- `identifier=report_interval`：指定上报周期属性。
- `params.report_interval`：周期值，插件会转为整数。

成功判定：返回 `code=0`，后续心跳/状态上报节奏变化。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-property-report-interval-001",
    "method": "thing.property.set",
    "identifier": "report_interval",
    "params": {
      "report_interval": 300
    }
  }'
```

#### 示例 A7：批量属性下发（identifier 为空）

用途：一次请求同时修改多个属性，减少多次调用。

关键字段说明：

- `identifier` 省略或留空：表示批量属性模式。
- `params` 内仅保留插件支持字段（其余字段会被过滤）。

成功判定：返回 `code=0`，多个属性一并生效。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-property-batch-set-001",
    "method": "thing.property.set",
    "params": {
      "report_interval": 180,
      "location": {"longitude": "112.571321", "latitude": "37.793533"},
      "family_numbers": "13068040314,1,18435115041,2"
    }
  }'
```

### 6.2 服务调用（`thing.service.invoke`）

#### 示例 B1：`leave_message_add`

用途：下发“语音留言”（设备下载并保存音频）。

关键字段说明：

- `identifier=leave_message_add`：调用语音留言服务。
- `params.mid`：留言 ID（建议唯一）。
- `params.url`：语音文件 URL（amr/mp3，建议可公网访问）。
- `params.format`：`0=amr`，`1=mp3`。

成功判定：返回 `code=0`，设备收到并可播放该留言。

注意：该服务不支持 `title/content`，传错字段会报 `request invalid`。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079608726",
    "request_id": "req-onenet-service-leave-message-add-001",
    "method": "thing.service.invoke",
    "identifier": "leave_message_add",
    "params": {
      "mid": "m003",
      "url": "",
      "format": 0
    }
  }'
```

#### 示例 B2：`leave_message_text_add`

用途：下发“文字留言（可选带语音）”，适合纯文本提醒场景。

关键字段说明：

- `identifier=leave_message_text_add`：调用文字留言服务。
- `params.text`：要播报的文本内容。
- `params.url/format`：可选语音文件及格式，纯文字时 `url` 可空。

成功判定：返回 `code=0`，设备会播报文字或提醒有新留言。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-service-leave-message-text-add-001",
    "method": "thing.service.invoke",
    "identifier": "leave_message_text_add",
    "params": {
      "mid": "m001",
      "url": "",
      "format": 0,
      "text": "请按时吃药"
    }
  }'
```

#### 示例 B3：`leave_message_clear`

用途：清空设备上的语音留言。

关键字段说明：

- `identifier=leave_message_clear`：调用留言清空服务。
- `params={}`：该服务无入参，传空对象即可。

成功判定：返回 `code=0`，设备侧留言列表被清空。

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/command' \
  -H 'Content-Type: application/json' \
  -d '{
    "unique_id": "867369079784279",
    "request_id": "req-onenet-service-leave-message-clear-001",
    "method": "thing.service.invoke",
    "identifier": "leave_message_clear",
    "params": {}
  }'
```

> 注意：服务类 `params` 字段由业务定义，插件会透传，不做字段白名单限制。

---

## 7. 对接建议

1. 调用方统一存储并使用 `unique_id`，不要依赖厂商设备号。
2. `request_id` 使用业务全局唯一值，便于日志对账。
3. 对 `code != 0` 做失败重试或人工告警。
4. 首次接入建议先跑“示例 A”，确认映射与权限配置正确。
