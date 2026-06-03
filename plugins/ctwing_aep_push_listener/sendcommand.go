package ctwing_aep_push_listener

import (
	"context"
	"iot-middleware/pkg/base"
)

// SendCommand 电信CTWing AEP平台下行指令实现。
// 如需调用 CTWing AEP CreateCommandLwm2mProfile 接口，在此实现。
// 接口文档：https://www.ctwing.cn/channel-help-api.htm?api=99
func (w *Worker) SendCommand(_ context.Context, _ string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	return nil, base.ErrCommandNotSupported
}
