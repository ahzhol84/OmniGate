# shanghai_xikali（上海希卡利插件）

## 概述

本插件通过 **阿里云物联网平台 AMQP 服务端订阅** 消费上海希卡利（ShangHai XiKaLi）睡眠监测设备的上报数据，同时通过 **希卡立 HTTP API** 实现设备反控（参数设置）与历史数据查询。

支持的设备型号（基于测试数据）：
- **L4LTE** 系列防跌倒雷达
  - ProductKey: `i0m773yyR3D`
  - 设备示例: `44074268`, `44074269`

## 架构

```
                    ┌────────────────────────────────────────┐
阿里云 IoT AMQP    │  希卡立 REST API (59.110.175.77)        │
    Broker         │  :4861(触发) :4877(配置)               │
       │           │  :4879(报告) :4881(对比/床垫)          │
       ▼           │  :4859(纯API)                          │
  amqps://         └──────────┬─────────────────────────────┘
       │                      │
       ▼                      ▼
┌───────────────────────────────────────────────┐
│          shanghai_xikali 插件                   │
│  ┌────────────────┐  ┌────────────────────┐   │
│  │ AMQP Consumer  │  │ SendCommand (反控)  │   │
│  │ (上行数据)      │  │ configset/dailyreport│   │
│  └───────┬────────┘  │ ... 10+ API 接口    │   │
│          │           └────────────────────┘   │
│          ▼                                    │
│  ┌────────────────┐                           │
│  │ DeviceData     │ → out 通道                 │
│  └────────────────┘                           │
└───────────────────────────────────────────────┘
```

## 数据流

### 正向隧道（上行 - AMQP）

1. AMQP 接收消息（Topic: `/{productKey}/{deviceID}/user/update`）
2. 解析阿里云物模型 JSON → 拍平 items → 输出 `DeviceData`

### 反向隧道（下行 - HTTP API）✅ 已实现

通过希卡立 REST API 实现以下能力：

| 类别 | 接口 | HTTP |
|------|------|------|
| 参数设置 | configset (POST :4877) | 设置预警阈值/报告时段 |
| 参数查询 | configget (GET :4877) | 查询设备当前参数 |
| 触发计算 | control_mq (POST :4861) | 触发AMQP即时计算 |
| 睡眠日报 | dailyreport (GET :4859) | 查单日报告 |
| 睡眠分期 | sleepstage (GET :4859) | 查分期数据 |
| 分时数据 | ratedata (GET :4859) | 查心/呼吸率 |
| 聚合报告 | fullreport (GET :4859) | 统一聚合 |
| 预警查询 | warning (GET :4859) | 查预警消息 |
| 床垫参数 | bedinfo (POST :4881) | 设置床宽/厚 |
| 压敏操作 | pressure (POST :4881) | 压敏初始化 |

认证方式：所有 API 使用 MD5 签名（`appkey + timestamp + sign`），已内置实现。

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

> **安全建议**：`access_key` 和 `access_secret` 建议通过环境变量注入。

## 下行反控调用示例

通过 `plugin.SendCommand` 调用：

### 设置预警阈值

```json
{
  "method": "configset",
  "identifier": "",
  "params": {
    "channel": {
      "plugin": "shanghai_xikali",
      "appkey": "your-appkey",
      "secret": "your-secret",
      "vendor_device_id": "123456",
      "db": "xkl_prod",
      "product_key": "i0m773yyR3D"
    },
    "hr_switch": 1,
    "hr_min": 50,
    "hr_max": 120,
    "br_switch": 1,
    "br_min": 10,
    "br_max": 30,
    "offbed_switch": 1,
    "offbed_thre": 30,
    "interval": 5
  }
}
```

### 查询睡眠日报

```json
{
  "method": "dailyreport",
  "params": {
    "date": "2024-1-2",
    "channel": {
      "plugin": "shanghai_xikali",
      "appkey": "your-appkey",
      "secret": "your-secret",
      "vendor_device_id": "123456",
      "db": "xkl_prod"
    }
  }
}
```

## 文件说明

| 文件 | 说明 |
|---|---|
| `worker.go` | AMQP 连接管理、消息接收、数据解析、设备注册 |
| `sendcommand.go` | 下行反控（希卡立真实 HTTP API，10+ 接口） |
| `README.md` | 本文件 |

## 注意事项

- 阿里云 AMQP 服务端订阅**一个消费组只能创建一个 Receiver Link**
- 建议通过配置多个 `consumer_group_id` 实现多个消费端
- `InsecureSkipVerify` 在正式环境建议设置为 `false`
- 「纯 API」系列接口（:4859）的 db 参数必须是 `xkl_prod` 或 `x2_prod`
- configset（:4877）的 n ≥ 3 个参数（含3个必选验证参数）
- control_mq（:4861）仅在北京时间 5:00-13:00 才可调用
