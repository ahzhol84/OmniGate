package aiqiangua_x8

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand 爱牵挂 X8 暂不支持下行指令。
// 如需对接爱牵挂云端设备控制 API，在此实现。
func (w *Worker) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
