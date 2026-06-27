# OmniGate — IoT 设备数据中台（中间件）

[![Go Version](https://img.shields.io/badge/Go-1.25.6-blue)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **OmniGate** 是一个基于 **Go 语言** 构建的 IoT 设备数据中台，以 **插件化架构** 为核心，支持多厂商、多协议设备的统一接入、标准化处理、实时分发、下行指令和数据持久化。

---

## 📋 目录

- [架构概览](#架构概览)
- [核心数据流](#核心数据流)
- [项目结构](#项目结构)
- [核心模块](#核心模块)
  - [入口与配置（cmd/server）](#入口与配置cmdserver)
  - [统一数据模型（pkg/base）](#统一数据模型pkgbase)
  - [插件注册中心（pkg/plugin）](#插件注册中心pkgplugin)
  - [全局命令路由（pkg/command）](#全局命令路由pkgcommand)
  - [公共基础设施（pkg/common）](#公共基础设施pkgcommon)
  - [实时 WebSocket 枢纽（pkg/realtime）](#实时-websocket-枢纽pkgrealtime)
  - [登录鉴权框架（pkg/auth）](#登录鉴权框架pkgauth)
- [Plugin 插件体系](#plugin-插件体系)
  - [IWorker 接口](#iworker-接口)
  - [ICommandable 接口](#icommandable-接口)
  - [插件注册机制](#插件注册机制)
  - [三种插件模式](#三种插件模式)
- [下行指令（反向隧道）](#下行指令反向隧道)
- [实时数据分发（WebSocket）](#实时数据分发-websocket)
- [数据落库与 Device ID 归一化](#数据落库与-device-id-归一化)
- [快速开始](#快速开始)
- [如何编写新插件](#如何编写新插件)

---

## 架构概览

```
                          ┌──────────────────────────────────────────────────────────────┐
                          │                       OmniGate Server                         │
                          │                                                              │
 config.yaml ──► Config   │   ┌─────────────────────────────────────────────────────┐    │
   (plugins)  ──► Loader  │   │                插件层 (Plugins)                       │    │
                          │   │  ┌──────────┐  ┌──────────┐  ┌──────────┐           │    │
                          │   │  │ Plugin A │  │ Plugin B │  │ Plugin C │  ...       │    │
                          │   │  │ (HTTP)   │  │ (MQTT)   │  │ (Poller) │           │    │
                          │   │  └────┬─────┘  └────┬─────┘  └────┬─────┘           │    │
                          │   └───────┼──────────────┼──────────────┼────────────────┘    │
                          │           │              │              │                       │
                          │           ▼              ▼              ▼                       │
                          │           ┌──────────────────────────────────┐                 │
                          │           │     ingressChan (Channel, 100)   │ ◄── 正向数据流    │
                          │           └──────────────┬───────────────────┘                 │
                          │                          │                                     │
                          │                          ▼                                     │
                          │           ┌──────────────────────────────────┐                 │
                          │           │     数据分发器 (Distributor)      │                 │
                          │           │  ┌──────────┐  ┌─────────────┐  │                 │
                          │           │  │WS Hub    │  │writerChan   │  │                 │
                          │           │  │(实时广播) │  │(持久化通道)  │  │                 │
                          │           │  └──────────┘  └──────┬──────┘  │                 │
                          │           └────────────────────────┼────────┘                 │
                          │                                    │                          │
                          │                                    ▼                          │
                          │                         ┌──────────────────┐                 │
                          │                         │ Data Writer     │                 │
                          │                         │ (MySQL 批量写入) │                 │
                          │                         └────────┬─────────┘                 │
                          │                                  │                           │
                          │                                  ▼                           │
                          │                         ┌──────────────────┐                 │
                          │                         │    MySQL DB      │                 │
                          │                         │ (device_data 表) │                 │
                          │                         └──────────────────┘                 │
                          └──────────────────────────────────────────────────────────────┘
                                          │
                              ┌───────────┴──────────────┐
                              │                          │
                              ▼                          ▼
                    ┌──────────────────┐      ┌──────────────────┐
                    │ 下行指令 HTTP API │      │ WebSocket        │
                    │ /device/command  │      │ /ws/events       │
                    │ POST 统一指令入口  │      │ 实时订阅推送      │
                    └────────┬─────────┘      └──────────────────┘
                             │
                             ▼
                    ┌──────────────────┐
                    │ Command Router   │
                    │ (认领 / 分发)     │
                    └────────┬─────────┘
                             │
               ┌─────────────┼─────────────┐
               │             │             │
               ▼             ▼             ▼
          Plugin A      Plugin B      Plugin C
        SendCommand    SendCommand    SendCommand
```

### 设计理念

| 概念 | 说明 |
|---|---|
| **正向隧道** | 插件从设备侧采集数据 → 标准化为 `DeviceData` → 推入主通道 |
| **反向隧道** | 外部 API 接收控制指令 → Command Router 分发 → 插件转送到设备 |
| **插件即 Worker** | 每个插件实现 `IWorker` 接口，以 goroutine 独立运行 |
| **配置驱动** | `config.yaml` 定义启停与参数，无需硬编码 |

---

## 核心数据流

```
┌────────────┐     ┌────────────┐     ┌─────────────────┐     ┌──────────────┐
│  设备数据    │ ──► │   插件     │ ──► │  ingressChan    │ ──► │  数据分发器    │
│ (HTTP/MQTT) │     │ (IWorker) │     │  (缓冲 100 条)    │     │              │
└────────────┘     └────────────┘     └─────────────────┘     └──────┬───────┘
                                                                     │
                                    ┌────────────────────────────────┼────────────┐
                                    │                                │            │
                                    ▼                                ▼            ▼
                          ┌─────────────────┐              ┌────────────┐ ┌────────────┐
                          │ WebSocket Hub    │              │ writerChan │ │ _(忽略)_   │
                          │ (实时广播)       │              │            │ │            │
                          └─────────────────┘              └──────┬─────┘ └────────────┘
                                                                  │
                                                                  ▼
                                                         ┌─────────────────┐
                                                         │ Data Writer     │
                                                         │ (批量写 MySQL)   │
                                                         │ (50条/30秒刷盘)  │
                                                         └─────────────────┘
```

数据从插件采集到最终落库，经过 **采集 → 标准化 → 分发 → 广播 → 持久化** 五个阶段，**完全解耦**。

---

## 项目结构

```
.
├── cmd/server/                  # 入口
│   ├── main.go                  # Main 函数：组装全局流程
│   └── config.go                # 配置解析（YAML → struct）
├── pkg/                         # 核心库
│   ├── base/
│   │   └── types.go             # DeviceData / IWorker / ICommandable / DeviceCommand / CommandReply
│   ├── plugin/
│   │   └── registry.go          # 全局插件注册中心
│   ├── command/
│   │   └── router.go            # 下行命令路由分发
│   ├── common/
│   │   ├── storage.go           # MySQL/Redis 初始化 + DataWriter
│   │   ├── device_identity.go   # 设备 ID 归一化（FNV-1a 哈希）
│   │   └── device_id_mapping.go # 设备 ID 映射表管理
│   ├── realtime/
│   │   └── hub.go               # WebSocket Hub（实时推送 + 命令 API）
│   └── auth/
│       └── http_auth.go         # 登录/Token 鉴权框架（HMAC-SHA256 + Redis）
├── plugins/                     # 插件目录
│   ├── teaching_device_scaffold/ # 教学脚手架（最小示例）
│   ├── sxb_poller/              # 主动轮询 + 被动推送双模式
│   ├── simple_http_responder/   # 简单 HTTP 服务 + 登录鉴权
│   ├── aiqiangua_x8/            # 爱牵挂 X8 设备
│   ├── ctwing_aep_push_listener/# 电信 AEP 平台推送
│   ├── dyf20a_poller/           # 迪云服设备轮询
│   ├── generic_http_listener/   # 通用 HTTP 推送接收
│   ├── huanjing_jiankong_v3/    # 环境监控 V3
│   ├── likaan_push_listener/    # 理康推送接收
│   ├── onenet_http_push_listener/# OneNET HTTP 推送
│   ├── shanghai_xikali/         # 上海希卡立
│   └── zixing_smart_home/       # 紫兴智能家居
├── docs/                        # 文档
├── config.yaml                  # 主配置文件
├── go.mod / go.sum              # Go 模块
└── build.sh                     # 编译脚本（CGO_ENABLED=0 静态编译）
```

---

## 核心模块

### 入口与配置（cmd/server）

#### `main.go` 启动流程

```go
func main() {
    cfg := loadConfig()               // 1. 读取 config.yaml
    common.InitRedis(cfg.Global.RedisAddr)  // 2. 初始化全局资源
    common.InitDB(cfg.Global.DBDsn)

    ingressChan := make(chan *base.DeviceData, 100)  // 3. 创建数据通道
    writerChan := make(chan *base.DeviceData, 100)

    // 4. 可选：启动 WebSocket Hub
    if wsCfg.Enabled {
        wsHub = realtime.NewHub(...)
        go wsHub.Start(ctx)
    }

    // 5. 数据分发器：ingressChan → (wsHub.Publish + writerChan)
    go func() { /* ... */ }()

    // 6. 数据写入器：writerChan → MySQL 批量写
    go func() { common.StartDataWriter(writerChan) }()

    // 7. 解析插件配置、启动插件
    for name, configs := range pluginConfigs {
        factory := plugin.Plugins[name]
        worker := factory()
        worker.Init(configs)

        if commandable, ok := worker.(base.ICommandable); ok {
            command.RegisterNamed(name, commandable)
        }

        go worker.Start(ctx, ingressChan)
    }

    // 8. 等待 SIGINT/SIGTERM → 优雅退出
    <-sigCh; cancel(); workerWG.Wait()
    close(ingressChan); /* wait for flush */
}
```

**优雅退出机制**：收到中断信号后 → 取消 context → 等待所有 Worker 退出 → 关闭 ingressChan → 等待分发器与写入器完成 → 确保数据已刷盘。

#### `config.go` 配置结构

```yaml
global:
  redis_addr: "127.0.0.1:6379"
  db_dsn: "user:pass@tcp(127.0.0.1:3306)/db_name?charset=utf8mb4"
  websocket:
    enabled: true
    listen_addr: ":8088"
    path: "/ws/events"
    auth_header: "X-Iot-Token"
    auth_token: ""
    event_buffer_size: 1024
    write_timeout_ms: 2000

plugins:
  - name: sxb
    enabled: true
    configs:
      - compony_name: "睡眠科技"
        client_id: "xxx"
        client_secret: "xxx"
        base_url: "https://openapi.sleepthing.com"
        device_id: "DEV001"
        unique_id: "SXB_UNIQUE_001"
        poll_interval: 60
        listen_addr: ":8090"
  - name: simple_http_responder
    enabled: true
    configs:
      - listen_addr: ":8080"
        password: "my_password"
        auth_key: "my_shared_key"
```

---

### 统一数据模型（pkg/base）

```go
type DeviceData struct {
    DeviceID    string          // 归一化后的设备 ID（数字字符串）
    UniqueID    string          // 设备唯一标识（由插件提供或自动哈希生成）
    DeviceType  string          // 设备类型标识
    Timestamp   time.Time       // 数据产生时间
    Payload     json.RawMessage // 原始数据 JSON
    ComponyName string          // 厂商/公司名
    DataType    string          // 数据类型（如 "HEART_RATE", "_NODB" 结尾的跳过落库）
}
```

核心接口：

```go
// IWorker — 所有插件必须实现的接口
type IWorker interface {
    Init(configs []json.RawMessage) error
    Start(ctx context.Context, out chan<- *DeviceData)
}

// ICommandable — 支持下行指令的插件可选实现
type ICommandable interface {
    SendCommand(ctx context.Context, deviceID string, cmd *DeviceCommand) (*CommandReply, error)
}
```

---

### 插件注册中心（pkg/plugin）

```go
var Plugins = make(map[string]func() base.IWorker)

func Register(name string, factory func() base.IWorker) {
    Plugins[name] = factory
}
```

插件在其 `init()` 函数中调用 `plugin.Register()` 完成自注册，main 函数通过 import 触发。

```go
import _ "iot-middleware/plugins/sxb_poller"
// 自动触发 sxb_poller/worker.go 中的 init()
// → plugin.Register("sxb", func() base.IWorker { return &SXBWorker{} })
```

---

### 全局命令路由（pkg/command）

```
POST /device/command { "unique_id":"XXX", "method":"property_set", ... }
         │
         ▼
   command.Dispatch(ctx, deviceID, cmd)
         │
         ├── Plugin A: SendCommand → ErrCommandNotSupported
         ├── Plugin B: SendCommand → ErrCommandNotSupported
         └── Plugin C: SendCommand → (*CommandReply, nil)  ✓
```

- **自动认领**：按注册顺序逐一遍历，直到某个插件返回非 `ErrCommandNotSupported`。
- **定向分发**：`DispatchWithPlugin(ctx, deviceID, cmd, pluginID)` 可指定目标插件。

---

### 公共基础设施（pkg/common）

| 组件 | 功能 |
|---|---|
| `common.InitRedis(addr)` | 初始化全局 Redis 客户端 |
| `common.InitDB(dsn)` | 初始化 MySQL/GORM 连接，自动建表、迁移旧数据 |
| `common.StartDataWriter(ch)` | 批量写入器：50条/30秒刷盘，`_NODB` 结尾的数据类型跳过落库 |
| `common.ResolveDeviceUniqueID(...)` | FNV-1a 哈希归一化 → 19 位数字 ID |
| `common.UpsertDeviceIDMapping(...)` | 维护 `device_id_mapping` 表（插件名→唯一ID→厂商设备ID） |

---

### 实时 WebSocket 枢纽（pkg/realtime）

```
Client ──WebSocket──► /ws/events?device_type=SXB_SLEEP_MONITOR
                         │
                         ▼
                    ┌──────────┐
                    │  Hub     │ (订阅过滤 + 广播)
                    └──────────┘
                         │
                         ▼
                    ┌──────────┐
                    │  Client  │ (匹配 client 收到实时数据)
                    └──────────┘
```

**特性**：
- 支持按 `device_type`、`device_id`、`unique_id`、`data_type` 订阅过滤
- 鉴权头 `X-Iot-Token`（可选）
- `POST /device/command` 下行指令 API
- `POST /device/replay` 历史数据重播

---

### 登录鉴权框架（pkg/auth）

```
Client                     Server (Validator)
  │                           │
  ├── POST /login ──────────► │  HMAC-SHA256(password, auth_key) 对比
  │  { "credential":"..." }   │  Redis 存储 token (12h TTL)
  │ ◄── { "token":"xxx" }    │
  │                           │
  ├── GET /api/xxx ──────────►│  X-Token 校验 → context 注入 username
  │  X-Token: xxx            │
  │ ◄── response             │
```

---

## Plugin 插件体系

### IWorker 接口

所有插件必须实现两个方法：

```go
type IWorker interface {
    // Init 接收多个配置项（每个配置项对应一个设备或端口）
    Init(configs []json.RawMessage) error

    // Start 启动工作协程，支持多个设备/端口的数据采集
    Start(ctx context.Context, out chan<- *base.DeviceData)
}
```

**约定**：
- `Init` 负责解析配置、校验冲突、填充默认值，**不应启动网络服务**
- `Start` 负责启动 HTTP 监听、轮询循环等
- 生产型插件应解析 `configs[]` 中的每个配置项，每个项可对应一个设备或接入端点

### ICommandable 接口

下行指令能力可选实现：

```go
type ICommandable interface {
    SendCommand(ctx context.Context, deviceID string, cmd *DeviceCommand) (*CommandReply, error)
}
```

关键机制：**设备认领**。若设备不属于此插件，返回 `ErrCommandNotSupported`，路由器自动尝试下一个插件。

### 插件注册机制

```go
// 每个插件在 init() 中自注册
func init() {
    plugin.Register("sxb", func() base.IWorker {
        return &SXBWorker{}
    })
}
```

main.go 通过空白导入激活：

```go
import _ "iot-middleware/plugins/sxb_poller"
```

### 三种插件模式

| 模式 | 描述 | 典型案例 |
|---|---|---|
| **推送接收型** | 设备主动 HTTP POST 推送数据，插件被动接收 | `teaching_device_scaffold`, `generic_http_listener`, `likaan_push_listener` |
| **主动轮询型** | 插件按固定间隔调用厂商 API 拉取数据 | `sxb_poller` (PollInterval 配置), `dyf20a_poller` |
| **组合型** | 同时提供推送接收 + 主动轮询 + HTTP API | `sxb_poller` |

---

## 下行指令（反向隧道）

```
用户系统                        OmniGate                        IoT 设备
  │                              │                               │
  ├── POST /device/command ────► │                               │
  │  {                           │                               │
  │    "unique_id":"...",        │                               │
  │    "method":"property_set",  │                               │
  │    "identifier":"switch",    │                               │
  │    "params":{"on":true}      │                               │
  │  }                           │                               │
  │                              ├── command.Dispatch() ────────►│
  │                              │   (设备认领)                   │
  │                              │   Plugin: SendCommand()       │
  │                              │◄── (reply, nil)              │
  │◄── {"code":0, "data":...}   │                               │
```

**指令认领**：
1. 先按 `plugin_id` 定向匹配（可选）
2. 未指定或没命中时，遍历所有注册的 `ICommandable` 插件
3. 每个插件调用 `SendCommand()` → 若设备不属于该插件 → 返回 `ErrCommandNotSupported`
4. 第一个接受认领的插件处理指令并返回结果

---

## 实时数据分发（WebSocket）

支持按订阅条件过滤：

```
ws://host:port/ws/events?device_type=SXB_SLEEP_MONITOR,SXB_SLEEP_PUSH&unique_id=UNIQUE_001

服务端推送格式：
{
  "type":         "device_realtime",
  "device_id":    "100000000000000025",
  "unique_id":    "100000000000000025",
  "device_type":  "SXB_SLEEP_MONITOR",
  "data_type":    "GetDeviceCurrentState",
  "timestamp":    "2025-01-01T00:00:00Z",
  "compony_name": "睡眠科技",
  "payload":      { ... 原始数据 ... }
}
```

---

## 数据落库与 Device ID 归一化

### Device ID 哈希归一化

```go
// 输入：厂商名称 + 插件名 + 设备类型 + 原始ID
// 输出：19位数字字符串（FNV-1a 64位哈希）
func ResolveDeviceUniqueID(explicitUniqueID, companyName, pluginName, deviceType, sourceDeviceID string) string
```

**目的**：不同厂商的不同设备 ID 格式（MAC、SN、自定义字符串）统一映射为纯数字 ID，作为全局主键。

### 批量写入策略

| 参数 | 值 |
|---|---|
| 批量大小 | 50 条 |
| 超时强制刷新 | 30 秒 |
| `_NODB` 后缀 | 跳过落库，仅广播 |

---

## 快速开始

### 前置条件

- Go 1.20+
- MySQL 5.7+ / 8.0
- Redis 6.x+
- 配置文件 `config.yaml`（项目根目录）

### 编译与运行

```bash
# 编译（静态链接）
./build.sh
# 或手动编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/iot-middleware ./cmd/server

# 运行
./bin/iot-middleware
```

### 配置示例

```yaml
global:
  redis_addr: "127.0.0.1:6379"
  db_dsn: "root:password@tcp(127.0.0.1:3306)/iot_db?charset=utf8mb4&parseTime=True&loc=Local"
  websocket:
    enabled: true
    listen_addr: ":8088"
    path: "/ws/events"
    auth_header: "X-Iot-Token"

plugins:
  - name: teaching_device_scaffold
    enabled: true
    configs:
      - listen_addr: ":8099"
        receive_path: "/push/teaching"
        device_type: "TEACHING_DEVICE"
        data_type: "TEACHING_UPLINK"
        demo_device_id: "TEACHING_DEVICE_001"
```

---

## 如何编写新插件

参见 [`plugins/teaching_device_scaffold/README.md`](plugins/teaching_device_scaffold/README.md) 教学脚手架。

### 最小插件骨架

```go
package my_plugin

import (
    "iot-middleware/pkg/base"
    "iot-middleware/pkg/plugin"
    "encoding/json"
)

type ConfigItem struct {
    // 你的配置字段
}

type MyWorker struct {
    configs []ConfigItem
}

func (w *MyWorker) Init(configs []json.RawMessage) error {
    // 解析、校验、填充默认值
    return nil
}

func (w *MyWorker) Start(ctx context.Context, out chan<- *base.DeviceData) {
    // 启动采集，标准化为 DeviceData，写入 out
}

func init() {
    plugin.Register("my_plugin", func() base.IWorker {
        return &MyWorker{}
    })
}
```

### 添加下行指令

```go
func (w *MyWorker) SendCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
    // 1. 判断设备是否属于本插件
    // 2. 不属于 → return nil, base.ErrCommandNotSupported
    // 3. 属于 → 调用厂商 API, 返回 CommandReply
}
```

---

## 许可证

[MIT License](LICENSE)# OmniGate — IoT 设备数据中台（中间件）

[![Go Version](https://img.shields.io/badge/Go-1.25.6-blue)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **OmniGate** 是一个基于 **Go 语言** 构建的 IoT 设备数据中台，以 **插件化架构** 为核心，支持多厂商、多协议设备的统一接入、标准化处理、实时分发、下行指令和数据持久化。

---

## 📋 目录

- [架构概览](#架构概览)
- [核心数据流](#核心数据流)
- [项目结构](#项目结构)
- [核心模块](#核心模块)
  - [入口与配置（cmd/server）](#入口与配置cmdserver)
  - [统一数据模型（pkg/base）](#统一数据模型pkgbase)
  - [插件注册中心（pkg/plugin）](#插件注册中心pkgplugin)
  - [全局命令路由（pkg/command）](#全局命令路由pkgcommand)
  - [公共基础设施（pkg/common）](#公共基础设施pkgcommon)
  - [实时 WebSocket 枢纽（pkg/realtime）](#实时-websocket-枢纽pkgrealtime)
  - [登录鉴权框架（pkg/auth）](#登录鉴权框架pkgauth)
- [Plugin 插件体系](#plugin-插件体系)
  - [IWorker 接口](#iworker-接口)
  - [ICommandable 接口](#icommandable-接口)
  - [插件注册机制](#插件注册机制)
  - [三种插件模式](#三种插件模式)
- [下行指令（反向隧道）](#下行指令反向隧道)
- [实时数据分发（WebSocket）](#实时数据分发-websocket)
- [数据落库与 Device ID 归一化](#数据落库与-device-id-归一化)
- [快速开始](#快速开始)
- [如何编写新插件](#如何编写新插件)

---

## 架构概览

```
                          ┌──────────────────────────────────────────────────────────────┐
                          │                       OmniGate Server                         │
                          │                                                              │
 config.yaml ──► Config   │   ┌─────────────────────────────────────────────────────┐    │
   (plugins)  ──► Loader  │   │                插件层 (Plugins)                       │    │
                          │   │  ┌──────────┐  ┌──────────┐  ┌──────────┐           │    │
                          │   │  │ Plugin A │  │ Plugin B │  │ Plugin C │  ...       │    │
                          │   │  │ (HTTP)   │  │ (MQTT)   │  │ (Poller) │           │    │
                          │   │  └────┬─────┘  └────┬─────┘  └────┬─────┘           │    │
                          │   └───────┼──────────────┼──────────────┼────────────────┘    │
                          │           │              │              │                       │
                          │           ▼              ▼              ▼                       │
                          │           ┌──────────────────────────────────┐                 │
                          │           │     ingressChan (Channel, 100)   │ ◄── 正向数据流    │
                          │           └──────────────┬───────────────────┘                 │
                          │                          │                                     │
                          │                          ▼                                     │
                          │           ┌──────────────────────────────────┐                 │
                          │           │     数据分发器 (Distributor)      │                 │
                          │           │  ┌──────────┐  ┌─────────────┐  │                 │
                          │           │  │WS Hub    │  │writerChan   │  │                 │
                          │           │  │(实时广播) │  │(持久化通道)  │  │                 │
                          │           │  └──────────┘  └──────┬──────┘  │                 │
                          │           └────────────────────────┼────────┘                 │
                          │                                    │                          │
                          │                                    ▼                          │
                          │                         ┌──────────────────┐                 │
                          │                         │ Data Writer     │                 │
                          │                         │ (MySQL 批量写入) │                 │
                          │                         └────────┬─────────┘                 │
                          │                                  │                           │
                          │                                  ▼                           │
                          │                         ┌──────────────────┐                 │
                          │                         │    MySQL DB      │                 │
                          │                         │ (device_data 表) │                 │
                          │                         └──────────────────┘                 │
                          └──────────────────────────────────────────────────────────────┘
                                          │
                              ┌───────────┴──────────────┐
                              │                          │
                              ▼                          ▼
                    ┌──────────────────┐      ┌──────────────────┐
                    │ 下行指令 HTTP API │      │ WebSocket        │
                    │ /device/command  │      │ /ws/events       │
                    │ POST 统一指令入口  │      │ 实时订阅推送      │
                    └────────┬─────────┘      └──────────────────┘
                             │
                             ▼
                    ┌──────────────────┐
                    │ Command Router   │
                    │ (认领 / 分发)     │
                    └────────┬─────────┘
                             │
               ┌─────────────┼─────────────┐
               │             │             │
               ▼             ▼             ▼
          Plugin A      Plugin B      Plugin C
        SendCommand    SendCommand    SendCommand
```

### 设计理念

| 概念 | 说明 |
|---|---|
| **正向隧道** | 插件从设备侧采集数据 → 标准化为 `DeviceData` → 推入主通道 |
| **反向隧道** | 外部 API 接收控制指令 → Command Router 分发 → 插件转送到设备 |
| **插件即 Worker** | 每个插件实现 `IWorker` 接口，以 goroutine 独立运行 |
| **配置驱动** | `config.yaml` 定义启停与参数，无需硬编码 |

---

## 核心数据流

```
┌────────────┐     ┌────────────┐     ┌─────────────────┐     ┌──────────────┐
│  设备数据    │ ──► │   插件     │ ──► │  ingressChan    │ ──► │  数据分发器    │
│ (HTTP/MQTT) │     │ (IWorker) │     │  (缓冲 100 条)    │     │              │
└────────────┘     └────────────┘     └─────────────────┘     └──────┬───────┘
                                                                     │
                                    ┌────────────────────────────────┼────────────┐
                                    │                                │            │
                                    ▼                                ▼            ▼
                          ┌─────────────────┐              ┌────────────┐ ┌────────────┐
                          │ WebSocket Hub    │              │ writerChan │ │ _(忽略)_   │
                          │ (实时广播)       │              │            │ │            │
                          └─────────────────┘              └──────┬─────┘ └────────────┘
                                                                  │
                                                                  ▼
                                                         ┌─────────────────┐
                                                         │ Data Writer     │
                                                         │ (批量写 MySQL)   │
                                                         │ (50条/30秒刷盘)  │
                                                         └─────────────────┘
```

数据从插件采集到最终落库，经过 **采集 → 标准化 → 分发 → 广播 → 持久化** 五个阶段，**完全解耦**。

---

## 项目结构

```
.
├── cmd/server/                  # 入口
│   ├── main.go                  # Main 函数：组装全局流程
│   └── config.go                # 配置解析（YAML → struct）
├── pkg/                         # 核心库
│   ├── base/
│   │   └── types.go             # DeviceData / IWorker / ICommandable / DeviceCommand / CommandReply
│   ├── plugin/
│   │   └── registry.go          # 全局插件注册中心
│   ├── command/
│   │   └── router.go            # 下行命令路由分发
│   ├── common/
│   │   ├── storage.go           # MySQL/Redis 初始化 + DataWriter
│   │   ├── device_identity.go   # 设备 ID 归一化（FNV-1a 哈希）
│   │   └── device_id_mapping.go # 设备 ID 映射表管理
│   ├── realtime/
│   │   └── hub.go               # WebSocket Hub（实时推送 + 命令 API）
│   └── auth/
│       └── http_auth.go         # 登录/Token 鉴权框架（HMAC-SHA256 + Redis）
├── plugins/                     # 插件目录
│   ├── teaching_device_scaffold/ # 教学脚手架（最小示例）
│   ├── sxb_poller/              # 主动轮询 + 被动推送双模式
│   ├── simple_http_responder/   # 简单 HTTP 服务 + 登录鉴权
│   ├── aiqiangua_x8/            # 爱牵挂 X8 设备
│   ├── ctwing_aep_push_listener/# 电信 AEP 平台推送
│   ├── dyf20a_poller/           # 迪云服设备轮询
│   ├── generic_http_listener/   # 通用 HTTP 推送接收
│   ├── huanjing_jiankong_v3/    # 环境监控 V3
│   ├── likaan_push_listener/    # 理康推送接收
│   ├── onenet_http_push_listener/# OneNET HTTP 推送
│   ├── shanghai_xikali/         # 上海希卡立
│   └── zixing_smart_home/       # 紫兴智能家居
├── docs/                        # 文档
├── config.yaml                  # 主配置文件
├── go.mod / go.sum              # Go 模块
└── build.sh                     # 编译脚本（CGO_ENABLED=0 静态编译）
```

---

## 核心模块

### 入口与配置（cmd/server）

#### `main.go` 启动流程

```go
func main() {
    cfg := loadConfig()               // 1. 读取 config.yaml
    common.InitRedis(cfg.Global.RedisAddr)  // 2. 初始化全局资源
    common.InitDB(cfg.Global.DBDsn)

    ingressChan := make(chan *base.DeviceData, 100)  // 3. 创建数据通道
    writerChan := make(chan *base.DeviceData, 100)

    // 4. 可选：启动 WebSocket Hub
    if wsCfg.Enabled {
        wsHub = realtime.NewHub(...)
        go wsHub.Start(ctx)
    }

    // 5. 数据分发器：ingressChan → (wsHub.Publish + writerChan)
    go func() { /* ... */ }()

    // 6. 数据写入器：writerChan → MySQL 批量写
    go func() { common.StartDataWriter(writerChan) }()

    // 7. 解析插件配置、启动插件
    for name, configs := range pluginConfigs {
        factory := plugin.Plugins[name]
        worker := factory()
        worker.Init(configs)

        if commandable, ok := worker.(base.ICommandable); ok {
            command.RegisterNamed(name, commandable)
        }

        go worker.Start(ctx, ingressChan)
    }

    // 8. 等待 SIGINT/SIGTERM → 优雅退出
    <-sigCh; cancel(); workerWG.Wait()
    close(ingressChan); /* wait for flush */
}
```

**优雅退出机制**：收到中断信号后 → 取消 context → 等待所有 Worker 退出 → 关闭 ingressChan → 等待分发器与写入器完成 → 确保数据已刷盘。

#### `config.go` 配置结构

```yaml
global:
  redis_addr: "127.0.0.1:6379"
  db_dsn: "user:pass@tcp(127.0.0.1:3306)/db_name?charset=utf8mb4"
  websocket:
    enabled: true
    listen_addr: ":8088"
    path: "/ws/events"
    auth_header: "X-Iot-Token"
    auth_token: ""
    event_buffer_size: 1024
    write_timeout_ms: 2000

plugins:
  - name: sxb
    enabled: true
    configs:
      - compony_name: "睡眠科技"
        client_id: "xxx"
        client_secret: "xxx"
        base_url: "https://openapi.sleepthing.com"
        device_id: "DEV001"
        unique_id: "SXB_UNIQUE_001"
        poll_interval: 60
        listen_addr: ":8090"
  - name: simple_http_responder
    enabled: true
    configs:
      - listen_addr: ":8080"
        password: "my_password"
        auth_key: "my_shared_key"
```

---

### 统一数据模型（pkg/base）

```go
type DeviceData struct {
    DeviceID    string          // 归一化后的设备 ID（数字字符串）
    UniqueID    string          // 设备唯一标识（由插件提供或自动哈希生成）
    DeviceType  string          // 设备类型标识
    Timestamp   time.Time       // 数据产生时间
    Payload     json.RawMessage // 原始数据 JSON
    ComponyName string          // 厂商/公司名
    DataType    string          // 数据类型（如 "HEART_RATE", "_NODB" 结尾的跳过落库）
}
```

核心接口：

```go
// IWorker — 所有插件必须实现的接口
type IWorker interface {
    Init(configs []json.RawMessage) error
    Start(ctx context.Context, out chan<- *DeviceData)
}

// ICommandable — 支持下行指令的插件可选实现
type ICommandable interface {
    SendCommand(ctx context.Context, deviceID string, cmd *DeviceCommand) (*CommandReply, error)
}
```

---

### 插件注册中心（pkg/plugin）

```go
var Plugins = make(map[string]func() base.IWorker)

func Register(name string, factory func() base.IWorker) {
    Plugins[name] = factory
}
```

插件在其 `init()` 函数中调用 `plugin.Register()` 完成自注册，main 函数通过 import 触发。

```go
import _ "iot-middleware/plugins/sxb_poller"
// 自动触发 sxb_poller/worker.go 中的 init()
// → plugin.Register("sxb", func() base.IWorker { return &SXBWorker{} })
```

---

### 全局命令路由（pkg/command）

```
POST /device/command { "unique_id":"XXX", "method":"property_set", ... }
         │
         ▼
   command.Dispatch(ctx, deviceID, cmd)
         │
         ├── Plugin A: SendCommand → ErrCommandNotSupported
         ├── Plugin B: SendCommand → ErrCommandNotSupported
         └── Plugin C: SendCommand → (*CommandReply, nil)  ✓
```

- **自动认领**：按注册顺序逐一遍历，直到某个插件返回非 `ErrCommandNotSupported`。
- **定向分发**：`DispatchWithPlugin(ctx, deviceID, cmd, pluginID)` 可指定目标插件。

---

### 公共基础设施（pkg/common）

| 组件 | 功能 |
|---|---|
| `common.InitRedis(addr)` | 初始化全局 Redis 客户端 |
| `common.InitDB(dsn)` | 初始化 MySQL/GORM 连接，自动建表、迁移旧数据 |
| `common.StartDataWriter(ch)` | 批量写入器：50条/30秒刷盘，`_NODB` 结尾的数据类型跳过落库 |
| `common.ResolveDeviceUniqueID(...)` | FNV-1a 哈希归一化 → 19 位数字 ID |
| `common.UpsertDeviceIDMapping(...)` | 维护 `device_id_mapping` 表（插件名→唯一ID→厂商设备ID） |

---

### 实时 WebSocket 枢纽（pkg/realtime）

```
Client ──WebSocket──► /ws/events?device_type=SXB_SLEEP_MONITOR
                         │
                         ▼
                    ┌──────────┐
                    │  Hub     │ (订阅过滤 + 广播)
                    └──────────┘
                         │
                         ▼
                    ┌──────────┐
                    │  Client  │ (匹配 client 收到实时数据)
                    └──────────┘
```

**特性**：
- 支持按 `device_type`、`device_id`、`unique_id`、`data_type` 订阅过滤
- 鉴权头 `X-Iot-Token`（可选）
- `POST /device/command` 下行指令 API
- `POST /device/replay` 历史数据重播

---

### 登录鉴权框架（pkg/auth）

```
Client                     Server (Validator)
  │                           │
  ├── POST /login ──────────► │  HMAC-SHA256(password, auth_key) 对比
  │  { "credential":"..." }   │  Redis 存储 token (12h TTL)
  │ ◄── { "token":"xxx" }    │
  │                           │
  ├── GET /api/xxx ──────────►│  X-Token 校验 → context 注入 username
  │  X-Token: xxx            │
  │ ◄── response             │
```

---

## Plugin 插件体系

### IWorker 接口

所有插件必须实现两个方法：

```go
type IWorker interface {
    // Init 接收多个配置项（每个配置项对应一个设备或端口）
    Init(configs []json.RawMessage) error

    // Start 启动工作协程，支持多个设备/端口的数据采集
    Start(ctx context.Context, out chan<- *base.DeviceData)
}
```

**约定**：
- `Init` 负责解析配置、校验冲突、填充默认值，**不应启动网络服务**
- `Start` 负责启动 HTTP 监听、轮询循环等
- 生产型插件应解析 `configs[]` 中的每个配置项，每个项可对应一个设备或接入端点

### ICommandable 接口

下行指令能力可选实现：

```go
type ICommandable interface {
    SendCommand(ctx context.Context, deviceID string, cmd *DeviceCommand) (*CommandReply, error)
}
```

关键机制：**设备认领**。若设备不属于此插件，返回 `ErrCommandNotSupported`，路由器自动尝试下一个插件。

### 插件注册机制

```go
// 每个插件在 init() 中自注册
func init() {
    plugin.Register("sxb", func() base.IWorker {
        return &SXBWorker{}
    })
}
```

main.go 通过空白导入激活：

```go
import _ "iot-middleware/plugins/sxb_poller"
```

### 三种插件模式

| 模式 | 描述 | 典型案例 |
|---|---|---|
| **推送接收型** | 设备主动 HTTP POST 推送数据，插件被动接收 | `teaching_device_scaffold`, `generic_http_listener`, `likaan_push_listener` |
| **主动轮询型** | 插件按固定间隔调用厂商 API 拉取数据 | `sxb_poller` (PollInterval 配置), `dyf20a_poller` |
| **组合型** | 同时提供推送接收 + 主动轮询 + HTTP API | `sxb_poller` |

---

## 下行指令（反向隧道）

```
用户系统                        OmniGate                        IoT 设备
  │                              │                               │
  ├── POST /device/command ────► │                               │
  │  {                           │                               │
  │    "unique_id":"...",        │                               │
  │    "method":"property_set",  │                               │
  │    "identifier":"switch",    │                               │
  │    "params":{"on":true}      │                               │
  │  }                           │                               │
  │                              ├── command.Dispatch() ────────►│
  │                              │   (设备认领)                   │
  │                              │   Plugin: SendCommand()       │
  │                              │◄── (reply, nil)              │
  │◄── {"code":0, "data":...}   │                               │
```

**指令认领**：
1. 先按 `plugin_id` 定向匹配（可选）
2. 未指定或没命中时，遍历所有注册的 `ICommandable` 插件
3. 每个插件调用 `SendCommand()` → 若设备不属于该插件 → 返回 `ErrCommandNotSupported`
4. 第一个接受认领的插件处理指令并返回结果

---

## 实时数据分发（WebSocket）

支持按订阅条件过滤：

```
ws://host:port/ws/events?device_type=SXB_SLEEP_MONITOR,SXB_SLEEP_PUSH&unique_id=UNIQUE_001

服务端推送格式：
{
  "type":         "device_realtime",
  "device_id":    "100000000000000025",
  "unique_id":    "100000000000000025",
  "device_type":  "SXB_SLEEP_MONITOR",
  "data_type":    "GetDeviceCurrentState",
  "timestamp":    "2025-01-01T00:00:00Z",
  "compony_name": "睡眠科技",
  "payload":      { ... 原始数据 ... }
}
```

---

## 数据落库与 Device ID 归一化

### Device ID 哈希归一化

```go
// 输入：厂商名称 + 插件名 + 设备类型 + 原始ID
// 输出：19位数字字符串（FNV-1a 64位哈希）
func ResolveDeviceUniqueID(explicitUniqueID, companyName, pluginName, deviceType, sourceDeviceID string) string
```

**目的**：不同厂商的不同设备 ID 格式（MAC、SN、自定义字符串）统一映射为纯数字 ID，作为全局主键。

### 批量写入策略

| 参数 | 值 |
|---|---|
| 批量大小 | 50 条 |
| 超时强制刷新 | 30 秒 |
| `_NODB` 后缀 | 跳过落库，仅广播 |

---

## 快速开始

### 前置条件

- Go 1.20+
- MySQL 5.7+ / 8.0
- Redis 6.x+
- 配置文件 `config.yaml`（项目根目录）

### 编译与运行

```bash
# 编译（静态链接）
./build.sh
# 或手动编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/iot-middleware ./cmd/server

# 运行
./bin/iot-middleware
```

### 配置示例

```yaml
global:
  redis_addr: "127.0.0.1:6379"
  db_dsn: "root:password@tcp(127.0.0.1:3306)/iot_db?charset=utf8mb4&parseTime=True&loc=Local"
  websocket:
    enabled: true
    listen_addr: ":8088"
    path: "/ws/events"
    auth_header: "X-Iot-Token"

plugins:
  - name: teaching_device_scaffold
    enabled: true
    configs:
      - listen_addr: ":8099"
        receive_path: "/push/teaching"
        device_type: "TEACHING_DEVICE"
        data_type: "TEACHING_UPLINK"
        demo_device_id: "TEACHING_DEVICE_001"
```

---

## 如何编写新插件

参见 [`plugins/teaching_device_scaffold/README.md`](plugins/teaching_device_scaffold/README.md) 教学脚手架。

### 最小插件骨架

```go
package my_plugin

import (
    "iot-middleware/pkg/base"
    "iot-middleware/pkg/plugin"
    "encoding/json"
)

type ConfigItem struct {
    // 你的配置字段
}

type MyWorker struct {
    configs []ConfigItem
}

func (w *MyWorker) Init(configs []json.RawMessage) error {
    // 解析、校验、填充默认值
    return nil
}

func (w *MyWorker) Start(ctx context.Context, out chan<- *base.DeviceData) {
    // 启动采集，标准化为 DeviceData，写入 out
}

func init() {
    plugin.Register("my_plugin", func() base.IWorker {
        return &MyWorker{}
    })
}
```

### 添加下行指令

```go
func (w *MyWorker) SendCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
    // 1. 判断设备是否属于本插件
    // 2. 不属于 → return nil, base.ErrCommandNotSupported
    // 3. 属于 → 调用厂商 API, 返回 CommandReply
}
```

---

## 许可证

[MIT License](LICENSE)