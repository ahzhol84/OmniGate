# shanghai_xikali（上海希卡利 AMQP 消费插件）

## 概述

本插件通过 **阿里云物联网平台 AMQP 服务端订阅** 消费上海希卡利（ShangHai XiKaLi）防跌倒雷达设备的上报数据。

支持的设备型号（基于测试数据）：
- **L4LTE** 系列防跌倒雷达
  - ProductKey: `i0m773yyR3D`
  - 设备示例: `44074268`, `44074269`

## 架构

```
阿里云 IoT AMQP Broker
        │
        ▼  amqps:// (TLS)
┌───────────────────┐
│  AMQP Consumer    │  ← 自动重连、退避
├───────────────────┤
│  Message Parser   │  ← 解析阿里云物模型 JSON
├───────────────────┤
│  DeviceData       │  → out 通道（主程序写入 DB + WS 广播）
└───────────────────┘
```

## 数据流

### 正向隧道（上行）

1. AMQP 接收消息（Topic: `/{productKey}/{deviceID}/user/update`）
2. 解析阿里云物模型 JSON：
   ```json
   {
     "id": "xkl_fall",
     "version": "1.0.0",
     "items": {
       "Height":       {"value": 2.8},
       "Fall_flag":    {"value": 0},
       "People_flag":  {"value": 0},
       "Nickname":     {"value": "L4LTE_S01B01N284"},
       "DeviceID":     {"value": "44074268"}
     },
     "method": "thing.event.property.post"
   }
   ```
3. 拍平 items 结构：
   ```json
   {
     "Height": 2.8,
     "Fall_flag": 0,
     "People_flag": 0,
     "Nickname": "L4LTE_S01B01N284",
     "DeviceID": "44074268",
     "_topic": "/i0m773yyR3D/44074268/user/update",
     "_method": "thing.event.property.post"
   }
   ```
4. 输出 `DeviceData` 到主通道

### 反向隧道（下行）

暂为模拟实现，二开需对接阿里云物联网平台 **OpenAPI**：
- `SetDeviceProperty`：设置设备属性
- `InvokeThingService`：调用设备服务

## 配置项

在 `config.yaml` 中添加：

```yaml
plugins:
  - name: "shanghai_xikali"
    enabled: true
    configs:
      - host: "iot-010a0clt.amqp.iothub.aliyuncs.com"
        port: 5671
        iot_instance_id: "iot-010a0clt"
        consumer_group_id: "lGIkek58uHzjqRaDUif6000110"
        client_id: "go-amqp-xikali-001"
        sign_method: "hmacsha1"
        access_key: "${ALIBABA_CLOUD_ACCESS_KEY_ID}"
        access_secret: "${ALIBABA_CLOUD_ACCESS_KEY_SECRET}"
        link_credit: 200
        reconnect_max_wait: 20
        insecure_skip_verify: true
        compony_name: "上海希卡利"
        device_type: "SHANGHAI_XIKALI"
        data_type: "ALIYUN_AMQP"
        unique_prefix: "xkl"
```

> **安全建议**：`access_key` 和 `access_secret` 建议通过环境变量 `ALIBABA_CLOUD_ACCESS_KEY_ID` / `ALIBABA_CLOUD_ACCESS_KEY_SECRET` 注入。

## 文件说明

| 文件 | 说明 |
|---|---|
| `worker.go` | AMQP 连接管理、消息接收、数据解析、设备注册 |
| `sendcommand.go` | 下行反控接口（模拟 + TODO） |
| `README.md` | 本文件 |

## 从教学版改成生产版

1. **下行反控**：在 `sendcommand.go` 中将 `executeAliYunCommand` 中的模拟返回替换为真实阿里云 OpenAPI 调用
2. **配置管理**：通过 `channel` 参数传入 `access_key`/`access_secret`
3. **ProductKey 提取**：在 `extractProductKeyFromTopicHistory` 中可以从 DB 历史记录或 device_id_mapping 表中提取
4. **消息去重**：根据 `messageId` 做幂等处理
5. **监控告警**：添加断连 / 积压告警

## 注意事项

- 阿里云 AMQP 服务端订阅**一个消费组只能创建一个 Receiver Link**
- 建议通过配置多个 `consumer_group_id` 实现多个消费端
- `InsecureSkipVerify` 在正式环境建议设置为 `false`
- 依赖的 `pack.ag/amqp` 包已通过 `go mod vendor` 或 `go get` 管理
