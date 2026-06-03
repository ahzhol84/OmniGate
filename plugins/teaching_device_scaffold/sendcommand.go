package teaching_device_scaffold

import (
	"context"
	"encoding/json"
	"fmt"
	"iot-middleware/pkg/base"
	"strings"
	"time"
)

// SendCommand 是教学插件的“反向隧道”示例实现。
// 这里展示一个标准模式：
// 1) 先判断设备是否属于本插件（设备认领）
// 2) 不属于就返回 ErrCommandNotSupported，让路由器继续尝试其他插件
// 3) 属于则执行下行逻辑（教学场景先做回显，不直接调用真实厂商 API）
func (w *Worker) SendCommand(_ context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || cmd == nil {
		return nil, base.ErrCommandNotSupported
	}

	cfg, owned := w.matchOwnedDevice(deviceID)
	if !owned {
		return nil, base.ErrCommandNotSupported
	}

	// 教学说明：
	// 这里故意不写真实厂家 HTTP 下发，只构造一个“已认领 + 已处理”的结果。
	// 二开时把这里替换成厂家 API 调用即可。
	result := map[string]interface{}{
		"action":         "teaching_command_echo",
		"device_id":      deviceID,
		"request_id":     strings.TrimSpace(cmd.RequestID),
		"method":         strings.TrimSpace(cmd.Method),
		"identifier":     strings.TrimSpace(cmd.Identifier),
		"params":         cmd.Params,
		"plugin":         "teaching_device_scaffold",
		"handled_at":     time.Now().Format(time.RFC3339),
		"next_step_todo": "replace echo with vendor API call",
		"matched_by": map[string]string{
			"demo_device_id": strings.TrimSpace(cfg.DemoDeviceID),
			"demo_unique_id": strings.TrimSpace(cfg.DemoUniqueID),
		},
	}
	msgBytes, _ := json.Marshal(result)

	return &base.CommandReply{
		RequestID: cmd.RequestID,
		Code:      0,
		Message:   string(msgBytes),
		Data:      result,
	}, nil
}

// matchOwnedDevice 判断当前 deviceID 是否属于教学插件实例。
// 认领规则（教学简化版）：
// - 与 demo_device_id 相等，或
// - 与 demo_unique_id 相等。
func (w *Worker) matchOwnedDevice(deviceID string) (ConfigItem, bool) {
	target := strings.TrimSpace(deviceID)
	for _, cfg := range w.configs {
		if target == "" {
			continue
		}
		if strings.TrimSpace(cfg.DemoDeviceID) == target {
			return cfg, true
		}
		if strings.TrimSpace(cfg.DemoUniqueID) == target {
			return cfg, true
		}
	}
	return ConfigItem{}, false
}

// BuildVendorCommandExample 返回一个“如何替换为真实厂商 API”的示例文本。
// 该函数主要用于教学阅读，不参与主流程。
func BuildVendorCommandExample() string {
	return fmt.Sprintf("%s", "TODO: 1) 构建厂商请求URL 2) 鉴权签名 3) 参数映射 4) 失败重试 5) 回包标准化")
}
