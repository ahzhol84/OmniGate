# huanjing_jiankong_v3（綜合環境監控云平台 3.0 集成插件）

这个插件用于接入**綜合環境監控云平台 3.0**，支持以下功能：

1. **正向隧道**：定时轮询平台获取设备实时数据，写入统一的 `DeviceData` 通道。
2. **反向隧道**：实现继电器开关控制命令（下行反控）。

## 文件说明

- **worker.go**
  - `Init`: 解析并校验 YAML 配置，初始化 HTTP 客户端。
  - `Start`: 为每个配置项启动独立的轮询 goroutine。
  - `getOrRefreshToken`: 自动登录获取 JWT token，支持过期刷新。
  - `pollWorker`: 单配置项的轮询工作循环。
  - `getRealTimeData`: 调用平台 API 获取实时数据。
  - `processRealTimeData`: 解析平台响应，标准化为 `DeviceData`，投递到主通道。

- **sendcommand.go**
  - `SendCommand`: 实现反向隧道，认领 `relayControl` 命令。
  - `controlRelay`: 调用平台的继电器控制接口。

- **README.md**: 本文件。

## 配置示例

在 `config.yaml` 的 `plugins` 部分添加：

```yaml
- name: "huanjing_jiankong_v3"
  enabled: true
  configs:
    - base_url: "http://www.0531yun.com"
      login_name: "h250714jlk"
      password: "h250714jlk"
      compony_name: "測試公司"
      poll_interval_m: 5            # 轮询间隔（分钟）
      command_timeout: 10             # 命令超时（秒）
      error_retry_max: 3              # 错误重试次数
      devices:
        - device_addr: 21120446
          device_name: "21120446"
          node_id: 2
          register_id: 5
          factor_name: "光照"
          unit: "lux"
        - device_addr: 21151922
          device_name: "21151922"
          node_id: 2
          register_id: 5
          factor_name: "光照"
          unit: "lux"
```

## 配置字段说明

| 字段 | 说明 | 默认值 |
|------|------|--------|
| `base_url` | 平台基础 URL | `http://www.0531yun.com` |
| `login_name` | 登录用户名 | 必需 |
| `password` | 登录密码 | 必需 |
| `compony_name` | 公司名称（用于数据标记） | `綜合環境監控` |
| `poll_interval_m` | 轮询间隔（分钟，0表示禁用） | 5 |
| `command_timeout` | 下行命令超时（秒） | 10 |
| `error_retry_max` | 错误重试次数 | 3 |
| `devices` | 要监控的设备及因子列表 | 必需 |

## 设备配置

每个设备需要指定以下信息，用于定位平台中的数据：

```json
{
  "device_addr": 21120446,    // 设备地址（必需）
  "device_name": "21120446",  // 设备名称
  "node_id": 2,               // 节点ID
  "register_id": 5,           // 寄存器ID
  "factor_name": "光照",      // 因子名称（用于因子过滤）
  "unit": "lux"               // 单位
}
```

## 工作流程

### 正向（上行）

```
轮询 (间隔5分钟)
    ↓
获取/刷新 JWT token
    ↓
调用 /api/data/getRealTimeData?deviceAddrs=xxx
    ↓
解析响应，按配置过滤因子
    ↓
标准化为 DeviceData
    ↓
投递到主通道 (写库+广播)
```

**Token 管理**：
- 自动在登录时缓存 token
- 距离过期时间 < 5 分钟时自动刷新
- 支持多个配置项分别管理 token

### 反向（下行）

```
/device/command 收到请求
    ↓
解析 Method/Identifier 为 "relayControl"
    ↓
提取 device_addr, relay_no, status
    ↓
获取 token（如需刷新则自动刷新）
    ↓
调用 /api/relay/control POST
    ↓
返回执行结果 (CommandReply)
```

**命令格式**：

```json
{
  "method": "relayControl",
  "identifier": "relayControl",
  "params": {
    "device_addr": 21120446,
    "relay_no": 1,
    "status": 1  // 0=关, 1=开
  }
}
```

## 生产化建议

1. **错误处理增强**：
   - 目前简单打印日志，可添加告警机制
   - 实现断路器模式防止级联故障

2. **性能优化**：
   - 支持批量查询多个设备而不是逐个调用
   - 添加 Redis 缓存减少平台 API 调用

3. **监控指标**：
   - 统计轮询成功率、平均响应时间
   - 监控 token 刷新频率、命令执行延迟

4. **安全加固**：
   - 支持 HTTPS 和证书校验
   - 敏感信息（密码）考虑从密钥管理服务读取

5. **扩展功能**：
   - 支持查询历史数据接口
   - 支持告警配置下发
   - 支持设备分组管理
