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

插件会将原始请求和标准字段写入 `payload`，示例：

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

---

## 6. 联调建议

- 先用统一入口 `receive_path` 跑通全部推送。
- 若对方坚持“每类数据一个 URL”，直接使用 `path_prefix` 下的分路径。
- 你方接口响应务必在 5 秒内返回 `200`，避免对方临时熔断。
- 建议先关闭 `allowed_ips`，联调稳定后再开启白名单。

---

## 7. 代码位置

- 插件实现：`plugins/aiqiangua_x8/worker.go`
- 插件注册：`cmd/server/main.go`（空导入）
- 配置示例：`config.yaml`
