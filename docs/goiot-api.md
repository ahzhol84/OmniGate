# GoIoT 接口文档（登录 / 即时数据 / 历史数据 / 下行控制）

## 1. 基本信息

- 服务地址：`http://47.94.14.70:8081`
- 鉴权方式：`X-Token` 请求头
- Content-Type：`application/json`

> 说明：以下接口均基于当前项目实现（`generic_http_listener` 插件）。

---

## 2. 登录接口

### 2.1 接口信息

- 方法：`POST`
- 路径：`/login`
- 是否鉴权：否

### 2.2 请求体

```json
{
  "credential": "string"
}
```

### 2.3 credential 生成规则

`credential = Base64( HMAC-SHA256(password, auth_key) )`

- HMAC Key：`auth_key`
- HMAC Message：`password`

### 2.4 请求示例

```bash
curl -sS -X POST 'http://47.94.14.70:8081/login' \
  -H 'Content-Type: application/json' \
  -d '{"credential":"a3pjRHw1fbvQNNiwSjFnT029p4y4gCjnd7V85W/751M="}'
```

### 2.5 成功响应示例

```json
{
  "code": 0,
  "msg": "ok",
  "token": "6sUWtPcMM7WkbLdeGbK1ZAhej6T4BH6W",
  "expires_in": 43200
}
```

### 2.6 失败响应

- `400`：无效 JSON
- `401`：无效凭证
- `405`：方法不允许
- `500`：内部错误

---

## 3. 设备即时数据查询

### 3.1 接口信息

- 方法：`GET`
- 路径：`/device/realtime`
- 是否鉴权：是（`X-Token`）

### 3.2 Query 参数

- `device_id`：设备唯一索引（必填）
- `device_type`：设备类型（选填，直连实时请求时使用）

### 3.3 请求示例

```bash
curl -sS 'http://47.94.14.70:8081/device/realtime?device_id=jlk|dyf20a|DYF20A|930813&device_type=DYF20A' \
  -H 'X-Token: 你的token'
```

### 3.4 处理逻辑

1. 若命中“可直连实时请求”条件（配置了 `realtime_url` 且 `device_type` 匹配），则：
   - 直接请求上游实时接口
   - 写入 Redis 热缓存（key：`goiot:realtime:{device_id}`）
   - 同时投递到采集通道异步写库
   - 直接返回实时数据
2. 否则：
   - 优先查 Redis 热缓存
   - Redis 未命中再查 `device_data` 最新一条（按 `timestamp DESC, id DESC`）

### 3.5 成功响应示例

```json
{
  "device_id": "jlk|dyf20a|DYF20A|930813",
  "unique_id": "jlk|dyf20a|DYF20A|930813",
  "device_type": "DYF20A",
  "source": "db_latest",
  "payload": {
    "temperature": 26,
    "humidity": 20.5,
    "power": 100,
    "signal": "强",
    "last_jingdu": 112.56549,
    "last_weidu": 37.790576,
    "timestamp": "2026-03-23 16:29:15"
  },
  "timestamp": "2026-03-23T16:29:16+08:00"
}
```

`source` 可能值：
- `direct_request`：直连实时设备返回
- `redis`：来自 Redis 热缓存
- `db_latest`：来自数据库最新记录

### 3.6 失败响应

- `400`：缺少 `device_id`
- `401`：缺少 token 或 token 无效/过期
- `404`：无设备数据
- `500`：数据库错误或内部错误

---

## 4. 设备历史数据分页查询（simpolist）

### 4.1 接口信息

- 方法：`GET`
- 路径：`/device/history/simpolist`
- 别名：`/device/history/simplelist`
- 是否鉴权：是（`X-Token`）

### 4.2 Query 参数

- `device_id`：设备唯一索引（必填）
- `page`：页码（选填，默认 `1`）
- `page_size`：每页条数（选填，默认 `20`，最大 `200`）

### 4.3 请求示例

```bash
curl -sS 'http://47.94.14.70:8081/device/history/simpolist?device_id=jlk|dyf20a|DYF20A|930813&page=1&page_size=20' \
  -H 'X-Token: 你的token'
```

### 4.4 处理逻辑

1. 优先查 Redis 分页缓存：
   - key：`goiot:history:simpolist:{device_id}:{page}:{page_size}`
2. 缓存未命中则查 DB：
   - 条件：`unique_id = ? OR device_id = ?`
   - 排序：`timestamp DESC, id DESC`
   - 同时返回 `total`
3. DB 结果写回 Redis（短 TTL）

### 4.5 成功响应示例

```json
{
  "device_id": "jlk|dyf20a|DYF20A|930813",
  "page": 1,
  "page_size": 20,
  "total": 3,
  "source": "db",
  "items": [
    {
      "id": 3780,
      "device_id": "jlk|dyf20a|DYF20A|930813",
      "unique_id": "jlk|dyf20a|DYF20A|930813",
      "device_type": "DYF20A",
      "data_type": "",
      "compony_name": "大昱丰",
      "timestamp": "2026-03-23T16:28:04+08:00",
      "payload": {
        "temperature": 26,
        "humidity": 20.5
      }
    }
  ]
}
```

### 4.6 失败响应

- `400`：缺少 `device_id`
- `401`：缺少 token 或 token 无效/过期
- `500`：查询失败

---

## 5. 鉴权头说明

所有受保护接口都必须带：

```http
X-Token: <login返回的token>
```

未带或失效会返回 `401`。

---

## 6. 配置项（与接口相关）

`config.yaml` 中 `generic_http_listener` 相关字段：

- `listen_addr`：监听地址
- `username` / `password` / `auth_key`：登录鉴权配置
- `device_type`：用于判断是否允许直连实时请求
- `realtime_url`：可选，配置后可走“直连实时设备”分支
- `realtime_ttl_seconds`：即时数据 Redis TTL（默认 120）
- `history_ttl_seconds`：历史分页 Redis TTL（默认 60）

> 建议调用方统一使用 `unique_id` 作为 `device_id` 参数传入。

---

## 7. 设备下行控制（Hub 转发，标准版）

### 7.1 接口信息

- 方法：`POST`
- 路径：`/device/command`
- 是否鉴权：是（`X-Iot-Token`，与 WebSocket 一致）

### 7.2 请求体（统一规范）

> 对外统一使用 `unique_id`，上层调用方无需关心厂商设备号。  
> 兼容字段：`device_id` 仍可传，但仅作为回退。

```json
{
  "unique_id": "jlk|onenet|ONENET|867369079784279",
  "request_id": "req-001",
  "method": "thing.property.set",
  "identifier": "family_numbers",
  "params": {
    "family_numbers": "13068040314,1,18435115041,2"
  }
}
```

字段定义：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `unique_id` | string | 是（推荐） | 业务侧设备唯一标识。Hub 优先使用该字段分发指令。 |
| `device_id` | string | 否 | 兼容旧调用；仅当 `unique_id` 为空时使用。 |
| `request_id` | string | 否 | 请求追踪 ID；建议业务侧传入。 |
| `method` | string | 是 | 指令方法。推荐值：`thing.property.set`、`thing.service.invoke`。 |
| `identifier` | string | 否 | 属性/服务标识。不同插件按自身能力解释。 |
| `params` | object | 是 | 反控参数对象。字段名和数据类型见“7.5 参数目录”。 |

### 7.3 分发规则

1. Hub 计算目标标识：`target = unique_id`（若空则 `device_id`）。
2. 调用插件路由，按插件顺序尝试认领。
3. 第一个认领成功的插件执行下发并返回标准回包。

### 7.4 响应规范

成功（HTTP 200）：

```json
{
  "request_id": "req-001",
  "code": 0,
  "message": "{...参数回显...}",
  "data": {
    "source_id": "jlk|onenet|ONENET|867369079784279",
    "device_id": "867369079784279",
    "resolved_device_from": "device_id_mapping",
    "action": "set_device_property",
    "method": "thing.property.set",
    "identifier": "family_numbers"
  }
}
```

失败：

- `400`：请求体非法 / `unique_id` 与 `device_id` 同时缺失
- `401`：鉴权失败
- `502`：无插件认领该设备，或插件执行失败

### 7.5 参数目录（字段名 + 数据类型）

> 以下为当前已实现插件可直接消费的参数目录。调用方可按设备所属插件选择对应参数。

#### 7.5.1 OneNET：属性下发 `thing.property.set`

| identifier | params 字段 | 类型 | 说明 |
|---|---|---|---|
| `family_numbers` | `family_numbers` | string \| array \| object | 家庭号码；支持 CSV、数组或对象。 |
| `emergency_contact` | `emergency_contact` | object | 紧急联系人。 |
| `function_keys` | `function_keys` | object | 功能按键配置。 |
| `location` | `location` | object | 定位相关配置。 |
| `timer_tasks` | `timer_tasks` | array | 定时任务列表。 |
| `report_interval` | `report_interval` | number \| string | 上报周期。 |

OneNET 示例：

```json
{
  "unique_id": "jlk|onenet|ONENET|867369079784279",
  "request_id": "req-onenet-001",
  "method": "thing.property.set",
  "identifier": "family_numbers",
  "params": {
    "family_numbers": "13068040314,1,18435115041,2"
  }
}
```

#### 7.5.2 OneNET：服务调用 `thing.service.invoke`

| identifier | params 字段 | 类型 | 说明 |
|---|---|---|---|
| `leave_message_add` | 自定义业务字段 | object | 留言新增。 |
| `leave_message_text_add` | 自定义业务字段 | object | 文本留言新增。 |
| `leave_message_clear` | 自定义业务字段 | object | 留言清空。 |

#### 7.5.3 爱牵挂 X8：SOS 联系人下发 `thing.property.set`

| params 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `name` | string | 条件必填 | 联系人姓名（`clear=false` 时必填）。 |
| `num` / `phone` | string | 条件必填 | 联系人号码（`clear=false` 时必填）。 |
| `sos_index` / `index` / `slot` | integer | 否 | 联系人槽位，默认走插件配置。 |
| `dial_flag` | integer | 否 | 拨号标记，默认 `1`。 |
| `network_type` | string | 否 | 网络类型（如 `4g`）。 |
| `clear` | boolean | 否 | 是否清空该槽位联系人。 |

X8 示例：

```json
{
  "unique_id": "jlk|aiqiangua_x8|AIQIANGUA_X8|866117051455595",
  "request_id": "req-x8-001",
  "method": "thing.property.set",
  "identifier": "sos_numbers",
  "params": {
    "name": "张三",
    "num": "13068040314",
    "sos_index": 1,
    "dial_flag": 1,
    "network_type": "4g"
  }
}
```

#### 7.5.4 爱牵挂 X8：设备设置下发 `thing.property.set`

| params 字段 | 类型 | 说明 |
|---|---|---|
| `frequency_location` | integer | 定位频率。 |
| `frequency_step` | integer | 计步频率。 |
| `frequency_heartrate` | integer | 心率频率。 |
| `theshold_heartrate_h` | integer | 心率高阈值。 |
| `theshold_heartrate_l` | integer | 心率低阈值。 |
| `incoming_restriction` | boolean | 来电限制开关。 |
| `sleep_enable` | boolean | 睡眠监测开关。 |
| `heartrate_enable` | boolean | 心率监测开关。 |
| `bloodoxygen_enable` | boolean | 血氧监测开关。 |
| `power_down_enable` | boolean | 关机提醒开关。 |
| ... | integer / boolean / string | 其余字段按插件能力扩展。 |

> 说明：X8 设备设置参数会直接透传到厂商设备编辑接口，建议调用方仅下发已评审字段。

---

## 8. WS-Hub 数据重播（按 device_data.id）

### 8.1 接口信息

- 方法：`POST`
- 路径：`/device/replay`
- 是否鉴权：是（`X-Iot-Token`，与 WebSocket 一致）

### 8.2 请求体

```json
{
  "id": 8093
}
```

字段说明：
- `id`：`device_data` 表主键 ID（也兼容 `record_id` / `data_id`）

### 8.3 请求示例

```bash
curl -sS -X POST 'http://47.94.14.70:8088/device/replay' \
  -H 'Content-Type: application/json' \
  -H 'X-Iot-Token: your_ws_token' \
  -d '{"id":8093}'
```

### 8.4 成功响应示例

```json
{
  "code": 0,
  "message": "ok",
  "id": 8093,
  "device_id": "866117051453095",
  "unique_id": "306756143402082944",
  "data_type": "SLEEP"
}
```

### 8.5 失败响应

- `400`：请求体非法 / 缺少 `id`
- `401`：鉴权失败
- `404`：记录不存在
- `500`：数据库不可用或查询失败
