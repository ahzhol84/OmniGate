// pkg/base/types.go
package base

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// DeviceData 是所有设备上报数据的统一格式
type DeviceData struct {
	DeviceID    string          `json:"device_id"`
	UniqueID    string          `json:"unique_id" gorm:"column:unique_id"`
	DeviceType  string          `json:"device_type"`
	Timestamp   time.Time       `json:"timestamp"` // Unix timestamp
	Payload     json.RawMessage `json:"payload" gorm:"column:payload;type:json"`
	ComponyName string          `json:"compony_name"`
	DataType    string          `json:"data_type"`
}

func (DeviceData) TableName() string {
	return "device_data"
}

// IWorker 插件接口（增强版）
type IWorker interface {
	// Init 接收多个配置项（每个配置项对应一个设备或端口）
	Init(configs []json.RawMessage) error
	// Start 启动工作协程，支持多个设备/端口的数据采集
	Start(ctx context.Context, out chan<- *DeviceData)
}

// ErrCommandNotSupported 插件不支持下行指令时返回此哨兵错误。
var ErrCommandNotSupported = errors.New("command not supported by this plugin")

// DeviceCommand 下行指令结构
type DeviceCommand struct {
	RequestID  string                 `json:"request_id"`
	Method     string                 `json:"method"`     // property_set | service_invoke
	Identifier string                 `json:"identifier"` // 属性/服务标识
	Params     map[string]interface{} `json:"params"`
}

// CommandReply 指令回复结构
type CommandReply struct {
	RequestID string                 `json:"request_id"`
	Code      int                    `json:"code"` // 0=成功
	Message   string                 `json:"message"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// ICommandable 可处理下行指令的插件接口（可选实现，插件按需实现）
type ICommandable interface {
	// SendCommand 向指定设备发送指令。
	// 若设备不属于本插件，必须返回 ErrCommandNotSupported。
	SendCommand(ctx context.Context, deviceID string, cmd *DeviceCommand) (*CommandReply, error)
}
