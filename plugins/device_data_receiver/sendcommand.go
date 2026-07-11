package device_data_receiver

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand 设备数据接收插件不支持下行指令。
func (w *Worker) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
