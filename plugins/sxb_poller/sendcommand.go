package sxb

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand 数孛轮询器暂不支持下行指令。
func (w *SXBWorker) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
