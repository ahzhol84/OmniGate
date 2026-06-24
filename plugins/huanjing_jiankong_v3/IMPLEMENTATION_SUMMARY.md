# 綜合環境監控云平台 3.0 插件实现总结

## 项目完成情况

✅ **已完成所有核心功能实现**

### 1. 插件文件结构

```
/plugins/huanjing_jiankong_v3/
├── worker.go              # 主体逻辑：初始化、轮询、token管理
├── sendcommand.go         # 下行命令：继电器控制
├── README.md              # 使用文档
└── IMPLEMENTATION_SUMMARY.md  # 本文件
```

### 2. 核心功能实现

#### 正向隧道（上行数据采集）
- ✅ 定时轮询获取实时数据
- ✅ 自动登录与 JWT token 管理
- ✅ Token 过期自动刷新（提前5分钟刷新）
- ✅ 支持多设备批量查询
- ✅ 数据标准化与投递到主通道

#### 反向隧道（下行命令执行）
- ✅ 继电器控制命令识别与认领
- ✅ 参数校验与错误处理
- ✅ 调用平台 /api/relay/control 接口
- ✅ 返回标准化的命令回复

### 3. 配置管理

#### 支持的配置参数
| 参数 | 类型 | 说明 | 默认值 |
|-----|------|------|--------|
| base_url | string | 平台基础 URL | http://www.0531yun.com |
| login_name | string | 登录用户名 | 必需 |
| password | string | 登录密码 | 必需 |
| compony_name | string | 公司名称标记 | 綜合環境監控 |
| poll_interval_m | int | 轮询间隔（分钟） | 5 |
| command_timeout | int | 命令超时（秒） | 10 |
| error_retry_max | int | 错误重试次数 | 3 |
| devices | array | 设备列表 | 必需 |

#### 设备配置示例
```yaml
devices:
  - device_addr: 21120446      # 设备地址
    device_name: "21120446"    # 设备名称
    node_id: 2                 # 节点ID
    register_id: 5             # 寄存器ID
    factor_name: "光照"        # 因子名称（用于数据过滤）
    unit: "lux"                # 单位
```

### 4. API 对接说明

#### 登录接口
```
GET /api/getToken?loginName={user}&password={pass}
响应: {
  "code": 1000,
  "data": {
    "token": "JWT token",
    "expiration": 1780572328 (毫秒时间戳)
  }
}
```

#### 实时数据接口
```
GET /api/data/getRealTimeData?deviceAddrs=21120446,21151922
Header: authorization: <JWT token>
响应: {
  "code": 1000,
  "data": [{
    "deviceAddr": 21120446,
    "deviceName": "xxx",
    "deviceStatus": "normal/offline/alarm",
    "dataItem": [{
      "nodeId": 2,
      "registerItem": [{
        "registerId": 5,
        "registerName": "光照",
        "data": "原始值字符串",          // ← 最终存储的数据来源
        "value": 1234.56,              // 实际数值
        "unit": "lux",
        "alarmLevel": 0,
        "alarmInfo": ""
      }]
    }],
    "timeStamp": 1717554000000 (毫秒时间戳)
  }]
}
```

#### 继电器控制接口
```
POST /api/relay/control
Header: 
  authorization: <JWT token>
  Content-Type: application/json
Body: {
  "deviceAddr": 21120446,
  "relayNo": 1,
  "status": 1    // 0=关, 1=开
}
响应: {
  "code": 1000,
  "data": true,
  "message": "操作成功"
}
```

### 5. 数据处理流程

#### 上行数据流
```
1. 轮询计时器触发
   ↓
2. 获取/刷新 JWT token
   ↓
3. 构建设备地址列表
   ↓
4. 调用 getRealTimeData API
   ↓
5. 解析响应数据
   ↓
6. 按配置过滤因子（只保存声明的因子）
   ↓
7. 标准化为 DeviceData 对象
   - DeviceID: "21120446"
   - UniqueID: "huanjing_v3|{loginName}|{deviceAddr}"
   - DeviceType: "HUANJING_JIANKONG_V3"
   - DataType: "HUANJING_REALTIME"
   - Payload: 包含 device_addr/data/value/alarm_level 等字段
   ↓
8. 投递到主通道（写库+广播）
```

#### 下行命令流
```
1. 接收 /device/command 请求
   ↓
2. 判断 method/identifier 是否为 "relayControl"
   ↓
3. 提取参数：device_addr/relay_no/status
   ↓
4. 若 token 即将过期，刷新 token
   ↓
5. 调用 /api/relay/control POST
   ↓
6. 返回 CommandReply：
   - code: 0 (成功) 或 -1 (失败)
   - message: 结果说明
   - data: 命令执行详情
```

### 6. 关键设计特点

#### Token 管理
- 使用 sync.RWMutex 保护并发访问
- 多个配置项可独立管理 token
- 自动在距离过期 5 分钟时触发刷新
- 支持后续扩展到密钥管理服务

#### 数据过滤
- 按 device_addr + factor_name 双重匹配
- 只存储配置声明的因子，其他无关数据直接丢弃
- 确保数据库存储的是有效的、被关注的数据

#### 错误处理
- 轮询失败记录日志但不中断，下次继续尝试
- 命令执行失败返回详细错误信息
- 网络超时设置为 30 秒（可配置）

#### 并发设计
- 每个配置项启动独立的 goroutine 轮询
- 通过 context 实现优雅关闭
- 使用带缓冲的通道避免阻塞

### 7. 使用建议

#### 启用插件
```yaml
# config.yaml
- name: "huanjing_jiankong_v3"
  enabled: true  # 改为 true
  configs:
    - base_url: "..."
      login_name: "..."
      # ... 其他配置
```

#### 监控日志
```bash
# 查看轮询启动日志
grep "HUANJING" logs/*.log

# 查看 token 获取
grep "token acquired" logs/*.log

# 查看数据投递
grep "data sent" logs/*.log
```

#### 调试命令格式
```json
POST /device/command
{
  "request_id": "cmd_001",
  "method": "relayControl",
  "identifier": "relayControl",
  "params": {
    "device_addr": 21120446,
    "relay_no": 1,
    "status": 1
  }
}
```

### 8. 后续扩展方向

#### 短期（优先级高）
- [ ] 添加告警机制（轮询失败告警、数据异常告警）
- [ ] 实现断路器模式防止级联故障
- [ ] 增加 Redis 缓存层

#### 中期（优先级中）
- [ ] 支持查询历史数据接口
- [ ] 支持告警配置下发
- [ ] 支持设备分组管理
- [ ] 支持 HTTPS 和证书校验

#### 长期（优先级低）
- [ ] 从密钥管理服务读取凭证
- [ ] 支持多租户隔离
- [ ] 性能优化（批量 API、连接池等）

### 9. 编译与测试

#### 编译验证
```bash
cd /home/ahzhol/Desktop/agntWorkSpace/iot-middleware
go build ./plugins/huanjing_jiankong_v3

# 应输出：✅ 编译成功（无错误）
```

#### 代码风格
- 遵循 Go 官方规范（go fmt）
- 所有函数和导出类型都有注释
- 错误处理使用标准 Go 模式

### 10. 文件清单

| 文件名 | 行数 | 说明 |
|--------|------|------|
| worker.go | ~450 | 主体逻辑、轮询、token管理 |
| sendcommand.go | ~165 | 下行命令处理 |
| README.md | ~250 | 用户文档 |
| IMPLEMENTATION_SUMMARY.md | 本文件 | 实现总结 |

## 验收清单

- ✅ 代码编译无错误
- ✅ 支持多设备轮询
- ✅ Token 自动管理与刷新
- ✅ 下行命令正确处理
- ✅ 数据标准化与投递
- ✅ 配置文件已更新
- ✅ 文档完整
- ✅ 代码注释详细

## 联系与支持

如遇到问题或需要扩展功能，请参考：
1. README.md - 详细的功能文档
2. worker.go - 核心代码注释
3. sendcommand.go - 命令处理注释
4. config.yaml - 配置示例

---

**更新时间**：2026年6月5日  
**版本**：v1.0.0  
**状态**：✅ 已完成
