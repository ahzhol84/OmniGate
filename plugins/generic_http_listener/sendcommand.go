package generic_http_listener

import (
	"context"
	"iot-middleware/pkg/base"
	"strings"
)

// SendCommand 通用 HTTP 侦听器的下行指令实现。
// 当配置中存在与 deviceID 匹配的设备时，本插件认领该指令。
// 当前返回 ErrCommandNotSupported；如需对接具体厂商的设备控制 HTTP API，在此实现。
func (w *GenericHTTPListener) SendCommand(_ context.Context, deviceID string, _ *base.DeviceCommand) (*base.CommandReply, error) {
	deviceID = strings.TrimSpace(deviceID)
	for _, cfg := range w.configs {
		if strings.TrimSpace(cfg.DeviceID) == deviceID || strings.TrimSpace(cfg.UniqueID) == deviceID {
			// TODO: 根据 cfg 及 cmd 调用厂商 HTTP 控制接口
			// 示例: POST cfg.CommandURL with token + cmd params
			return nil, base.ErrCommandNotSupported
		}
	}
	return nil, base.ErrCommandNotSupported
}
