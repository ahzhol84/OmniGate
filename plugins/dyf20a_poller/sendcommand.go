package dyf20a_poller

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand DYF20A 轮询器暂不支持下行指令。
func (w *DYF20AWorker) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
