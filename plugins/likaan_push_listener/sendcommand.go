package likaan_push_listener

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand 立可安推送侦听器的下行指令实现。
// 如需调用立可安云端设备控制接口，在此实现。
func (w *Worker) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
