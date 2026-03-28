# GoIoT 接口文档（登录 / 即时数据 / 历史数据）

## 1. 基本信息

- 服务地址：`http://127.0.0.1:8081`
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
curl -sS -X POST 'http://127.0.0.1:8081/login' \
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
curl -sS 'http://127.0.0.1:8081/device/realtime?device_id=jlk|dyf20a|DYF20A|930813&device_type=DYF20A' \
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
curl -sS 'http://127.0.0.1:8081/device/history/simpolist?device_id=jlk|dyf20a|DYF20A|930813&page=1&page_size=20' \
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
