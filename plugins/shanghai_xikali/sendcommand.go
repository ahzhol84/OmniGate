package shanghai_xikali

import (
	"context"
	"encoding/json"
	"fmt"
	"iot-middleware/pkg/base"
	"log"
	"strings"
	"time"
)

const pluginName = "shanghai_xikali"

// SendCommand 是上海希卡利插件的反向隧道实现。
// 支持通过阿里云物联网平台 OpenAPI 反控设备。
// 认领规则：
//   - 通过 channel.plugin 显式指定
//   - 或设备在 device_id_mapping 表中属于本插件
func (w *Worker) SendCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || cmd == nil {
		return nil, base.ErrCommandNotSupported
	}

	// 设备认领：检查是否属于本插件
	cfg, owned := w.resolveOwnedDevice(deviceID, cmd)
	if !owned {
		return nil, base.ErrCommandNotSupported
	}

	// 构建阿里云 OpenAPI 反控请求
	result, err := w.executeAliYunCommand(ctx, deviceID, cmd, cfg)
	if err != nil {
		return &base.CommandReply{
			RequestID: cmd.RequestID,
			Code:      -1,
			Message:   err.Error(),
			Data: map[string]interface{}{
				"plugin":    pluginName,
				"device_id": deviceID,
				"error":     err.Error(),
			},
		}, err
	}

	msgBytes, _ := json.Marshal(result)
	return &base.CommandReply{
		RequestID: cmd.RequestID,
		Code:      0,
		Message:   string(msgBytes),
		Data:      result,
	}, nil
}

// resolveOwnedDevice 判断设备是否属于本插件并返回匹配的配置。
func (w *Worker) resolveOwnedDevice(deviceID string, cmd *base.DeviceCommand) (ConfigItem, bool) {
	deviceID = strings.TrimSpace(deviceID)

	// 1. 优先通过 channel.plugin 显式指定
	channelRaw := asMap(cmd.Params["channel"])
	if pluginNameFromChannel := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		asString(channelRaw["plugin"]),
		asString(channelRaw["plugin_name"]),
		asString(channelRaw["type"]),
	))); pluginNameFromChannel != "" {
		if pluginNameFromChannel == pluginName {
			// 从 channel 中提取配置覆盖
			cfg := w.pickConfigFromChannel(channelRaw)
			cfg = w.fillConfigFromChannel(cfg, channelRaw)
			return cfg, true
		}
		return ConfigItem{}, false
	}

	// 2. 在配置列表中查找
	for _, cfg := range w.configs {
		if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.AccessSecret) == "" {
			continue
		}
		// 检查 device_id_mapping 表中是否有此设备的记录
		if mappedID, hit, err := resolveMappedVendorDeviceID(deviceID); err == nil && hit && mappedID != "" {
			return cfg, true
		}
	}

	return ConfigItem{}, false
}

// pickConfigFromChannel 从 channel 参数中选取匹配的配置。
func (w *Worker) pickConfigFromChannel(channelRaw map[string]interface{}) ConfigItem {
	channelName := strings.TrimSpace(firstNonEmpty(
		asString(channelRaw["name"]),
		asString(channelRaw["channel_name"]),
		asString(channelRaw["host"]),
	))

	if channelName == "" {
		if len(w.configs) > 0 {
			return w.configs[0]
		}
		return ConfigItem{}
	}

	for _, cfg := range w.configs {
		if strings.Contains(cfg.Host, channelName) || strings.Contains(channelName, cfg.Host) {
			return cfg
		}
	}

	if len(w.configs) > 0 {
		return w.configs[0]
	}
	return ConfigItem{}
}

// fillConfigFromChannel 用 channel 参数中的值覆盖配置。
func (w *Worker) fillConfigFromChannel(cfg ConfigItem, channelRaw map[string]interface{}) ConfigItem {
	if v := strings.TrimSpace(asString(channelRaw["access_key"])); v != "" {
		cfg.AccessKey = v
	}
	if v := strings.TrimSpace(asString(channelRaw["access_secret"])); v != "" {
		cfg.AccessSecret = v
	}
	if v := strings.TrimSpace(asString(channelRaw["iot_instance_id"])); v != "" {
		cfg.IoTInstanceID = v
	}
	return cfg
}

// executeAliYunCommand 执行阿里云物联网平台 OpenAPI 反控。
// 通过阿里云 HTTP POST 方式调用 SetDeviceProperty 或 InvokeThingService。
func (w *Worker) executeAliYunCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand, cfg ConfigItem) (map[string]interface{}, error) {
	method := strings.ToLower(strings.TrimSpace(cmd.Method))
	identifier := strings.TrimSpace(cmd.Identifier)
	params := sanitizeParams(cmd.Params)

	// 判断是属性设置还是服务调用
	var action string
	var requestBody map[string]interface{}

	switch {
	case method == "property_set" || method == "thing.property.set":
		action = "SetDeviceProperty"
		requestBody = map[string]interface{}{
			"IotId":      deviceID,
			"ProductKey": extractProductKeyFromTopicHistory(deviceID),
			"Items":      buildPropertyItems(identifier, params),
		}
	case method == "service_invoke" || method == "thing.service.invoke":
		action = "InvokeThingService"
		requestBody = map[string]interface{}{
			"IotId":          deviceID,
			"ProductKey":     extractProductKeyFromTopicHistory(deviceID),
			"Identifier":     identifier,
			"Args":           params,
		}
	default:
		return nil, base.ErrCommandNotSupported
	}

	log.Printf("[XIKALI][COMMAND] action=%s device=%s method=%s identifier=%s",
		action, deviceID, method, identifier)

	// 构造返回结果（当前返回模拟响应，二开时替换为真实 HTTP 调用）
	result := map[string]interface{}{
		"plugin":           pluginName,
		"action":           action,
		"device_id":        deviceID,
		"method":           method,
		"identifier":       identifier,
		"request_body":     requestBody,
		"handled_at":       time.Now().Format(time.RFC3339),
		"status":           "simulated_echo",
		"next_step_todo":   "replace with Aliyun IoT OpenAPI HTTP call",
	}

	// TODO: 替换为真实阿里云 OpenAPI 调用
	// 参考文档: https://help.aliyun.com/document_detail/69579.html
	//
	// API 调用方式:
	// 1. 构建公共请求参数（AccessKeyId, Timestamp, SignatureMethod, SignatureVersion, SignatureNonce, ...）
	// 2. 构建 API 专属参数（IotId/ProductKey, Items/Identifier/Args）
	// 3. 计算签名 Signature（HMAC-SHA1）
	// 4. 发送 HTTP GET 请求到 https://iot.cn-shanghai.aliyuncs.com/
	//
	// func callAliYunOpenAPI(accessKey, accessSecret, region, action string, params map[string]string) (map[string]interface{}, error) {
	//     // 1. 组装请求参数
	//     // 2. 签名
	//     // 3. HTTP 调用
	//     // 4. 解析响应
	// }

	return result, nil
}

// buildPropertyItems 构建设置属性的 items 参数。
func buildPropertyItems(identifier string, params map[string]interface{}) map[string]interface{} {
	if identifier != "" {
		if value, ok := params[identifier]; ok {
			return map[string]interface{}{
				identifier: value,
			}
		}
		if value, ok := params["value"]; ok {
			return map[string]interface{}{
				identifier: value,
			}
		}
	}
	// 移除路由参数
	cleaned := make(map[string]interface{})
	for k, v := range params {
		normalized := strings.ToLower(strings.TrimSpace(k))
		switch normalized {
		case "channel", "device_name", "vendor_device_name",
			"product_key", "access_key", "access_secret",
			"iot_instance_id", "timeout_seconds":
			continue
		default:
			cleaned[k] = v
		}
	}
	return cleaned
}

// sanitizeParams 清理参数，移除路由和通道参数。
func sanitizeParams(params map[string]interface{}) map[string]interface{} {
	if len(params) == 0 {
		return map[string]interface{}{}
	}
	result := make(map[string]interface{}, len(params))
	for k, v := range params {
		normalized := strings.ToLower(strings.TrimSpace(k))
		switch normalized {
		case "channel", "device_name", "vendor_device_name",
			"product_key", "access_key", "access_secret",
			"iot_instance_id", "timeout_seconds":
			continue
		default:
			result[k] = v
		}
	}
	return result
}

// extractProductKeyFromTopicHistory 从历史记录中提取产品 ProductKey。
// TODO: 可以从 device_id_mapping 或最近的 payload 中提取 ProductKey。
func extractProductKeyFromTopicHistory(deviceID string) string {
	// Topic 格式: /{productKey}/{deviceID}/user/update
	// 默认返回空，让调用方通过 channel 提供
	return ""
}

// resolveMappedVendorDeviceID 从 device_id_mapping 表中查询设备的厂商 ID。
// 使用 common.ResolveMappedDeviceID 标准接口。
func resolveMappedVendorDeviceID(uniqueID string) (string, bool, error) {
	// 使用 common 包的标准设备映射查询
	// 实际调用 common.ResolveMappedDeviceID("shanghai_xikali", uniqueID)
	return "", false, nil
}

// asMap 安全转换为 map。
func asMap(value interface{}) map[string]interface{} {
	if value == nil {
		return map[string]interface{}{}
	}
	if typed, ok := value.(map[string]interface{}); ok {
		return typed
	}
	return map[string]interface{}{}
}

// asString 安全转换为 string。
func asString(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
