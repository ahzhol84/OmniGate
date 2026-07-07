package generic_body_pass

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand 通用 Body 透传插件不支持下行指令。
func (w *Worker) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
