package simple_http_responder

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand 简单 HTTP 响应器不支持下行指令。
func (w *SimpleHTTPResponder) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
