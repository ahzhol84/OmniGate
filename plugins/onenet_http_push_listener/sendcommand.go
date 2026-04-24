package onenet_http_push_listener

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand OneNET 推送侦听器暂不支持下行指令。
func (w *Worker) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
