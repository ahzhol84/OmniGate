// pkg/base/types.go
package base

import (
	"context"
	"encoding/json"
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
