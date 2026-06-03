# 爱牵挂 X8 插件设计与接入说明

## 1. 插件定位

`aiqiangua_x8` 插件用于接收爱牵挂 X8 设备推送数据（默认 `application/x-www-form-urlencoded`），并写入中间件统一数据通道。

实现目标：

- 兼容对方“仅配置一套 URL”场景（单入口）。
- 兼容按事件分路径推送场景（同一服务多路径）。
- 自动标准化设备唯一索引，统一入库结构。

---

## 2. 配置项

在 `config.yaml` 增加插件：

```yaml
- name: "aiqiangua_x8"
  enabled: true
  configs:
    - compony_name: "佳莱康"
      listen_addr: ":8090"
      receive_path: "/push/aiqiangua/x8"
      path_prefix: "/push/aiqiangua/x8"
      device_type: "AIQIANGUA_X8"
      # allowed_ips:
      #   - "120.24.56.48"
      #   - "120.24.56.0/24"
```

字段说明：

- `listen_addr`：监听地址。
- `receive_path`：统一接收入口（推荐给爱牵挂配置该地址）。
- `path_prefix`：可选分路径入口前缀，支持如 `/push/aiqiangua/x8/location`。
- `device_type`：写入 `device_data.device_type`。
- `allowed_ips`：可选 IP 白名单，支持单 IP 或 CIDR。
- `api_base_url`：爱牵挂平台地址，默认 `http://api.aiqiangua.com:8888`。
- `username` / `password`：爱牵挂平台登录账号密码（用于下行接口登录）。
- `network_type`：设备网络类型，默认 `4g`。
- `sos_index`：默认紧急联系人槽位（1-10），默认 `1`。

---

## 3. 路由策略

插件同时支持两类路由：

1) 统一入口：

- `POST /push/aiqiangua/x8`
- 可通过 query 传事件类型：`?event=location`

2) 分路径入口：

- `POST /push/aiqiangua/x8/location`
- `POST /push/aiqiangua/x8/sos`
- `POST /push/aiqiangua/x8/heartrate`
- `POST /push/aiqiangua/x8/step`
- `POST /push/aiqiangua/x8/sleep`
- `POST /push/aiqiangua/x8/power`
- `POST /push/aiqiangua/x8/bloodpressure`
- `POST /push/aiqiangua/x8/fall`
- `POST /push/aiqiangua/x8/reply`
- `POST /push/aiqiangua/x8/bloodoxygen`
- `POST /push/aiqiangua/x8/exercise_heartrate`
- `GET/POST /push/aiqiangua/x8/notify`

---

## 4. data_type 映射规则

优先级：

1. query 参数 `event` 或 `type_name`。
2. URL 后缀（如 `/location`）。
3. 表单字段自动推断。

推断结果示例：

- `LOCATION`
- `SOS`
- `HEART_RATE`
- `STEP`
- `SLEEP`
- `POWER`
- `BLOOD_PRESSURE`
- `FALL`
- `ALERT_REPLY`
- `BLOOD_OXYGEN`
- `EXERCISE_HEART_RATE`
- `NOTIFY`
- `UNKNOWN`

---

## 5. 入库 payload 结构

除 `HEART_RATE` 外，插件会将原始请求和标准字段写入 `payload`，示例：

```json
{
  "vendor": "aiqiangua",
  "model": "x8",
  "event_type": "LOCATION",
  "path": "/push/aiqiangua/x8/location",
  "query": "",
  "remote_ip": "120.24.56.48",
  "content_type": "application/x-www-form-urlencoded",
  "form": {
    "imei": "868219000099988",
    "time_begin_str": "20260401153000",
    "lon": "113.123",
    "lat": "23.123",
    "is_track": "true"
  },
  "raw_body": "imei=868219000099988&time_begin_str=20260401153000&lon=113.123&lat=23.123&is_track=true",
  "event_time": "2026-04-01T15:30:00+08:00",
  "received_at": "2026-04-01T15:30:02+08:00"
}
```

`HEART_RATE`（心率）事件按物模型最小字段入库，只保留：

```json
{
  "heartrate": "78",
  "imei": "868219000099988",
  "time_begin": "1711956600"
}
```

---

## 6. 联调建议

- 先用统一入口 `receive_path` 跑通全部推送。
- 若对方坚持“每类数据一个 URL”，直接使用 `path_prefix` 下的分路径。
- 你方接口响应务必在 5 秒内返回 `200`，避免对方临时熔断。
- 建议先关闭 `allowed_ips`，联调稳定后再开启白名单。

---

## 7. 下行指令（设置紧急联系人）

插件已支持通过 Hub 的 `/device/command` 转发到爱牵挂接口：

1. 登录：`POST /api/auth/login`
2. 设置联系人：`POST /api/device/{device_id}/{network_type}/sos_numbers/{index}/`

参数映射：

- `name`：联系人名字（必填）
- `num`：联系人电话（必填）
- `dial_flag`：拨号标记，默认 `1`
- `network_type`：可选，不传时取配置
- `sos_index/index/slot`：可选，不传时取配置

设备标识说明：

- Hub 传入的 `device_id` 可以是厂商设备号（IMEI）或业务侧 `unique_id`。
- 当传入 `unique_id` 时，插件会先从 `device_data` 表按 `unique_id` 反查最近一条记录，解析真实设备号后再调用爱牵挂下行接口。
- 若无法反查到真实设备号，会返回失败并提示 `cannot resolve unique_id`。

清空联系人槽位（删除效果）：

- 爱牵挂当前接口是“覆盖写入”，未提供单独删除接口时，可用清空方式实现删除效果。
- 方式一：`params.clear=true`（或 `remove=true` / `delete=true`）
- 方式二：`identifier=sos_numbers_clear`（或 `sos_numbers_delete`）
- 清空时插件会提交 `name=""`、`num=""`、`dial_flag=0`。

建议下行请求示例（Hub 层）：

```json
{
  "device_id": "866117051455595",
  "request_id": "req-sos-001",
  "method": "property_set",
  "identifier": "sos_numbers",
  "params": {
    "name": "Redemption",
    "num": "13238699393",
    "dial_flag": 1,
    "sos_index": 1,
    "network_type": "4g"
  }
}
```

清空槽位示例：

```json
{
  "device_id": "jlk|aiqiangua_x8|AIQIANGUA_X8|866117051455595",
  "request_id": "req-sos-clear-001",
  "method": "property_set",
  "identifier": "sos_numbers",
  "params": {
    "sos_index": 1,
    "clear": true
  }
}
```

---

## 8. 代码位置

- 插件实现：`plugins/aiqiangua_x8/worker.go`
- 插件注册：`cmd/server/main.go`（空导入）
- 配置示例：`config.yaml`
