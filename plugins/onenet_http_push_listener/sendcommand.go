package onenet_http_push_listener

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	oneNetPluginName = "onenet_http_push_listener"
)

var (
	oneNetPropertySet = map[string]struct{}{
		"emergency_contact": {},
		"family_numbers":    {},
		"function_keys":     {},
		"location":          {},
		"timer_tasks":       {},
		"report_interval":   {},
	}
	oneNetServiceSet = map[string]struct{}{
		"leave_message_add":      {},
		"leave_message_text_add": {},
		"leave_message_clear":    {},
	}
)

type oneNetCommandContext struct {
	ChannelRaw        map[string]interface{}
	ChannelName       string
	UserID            string
	AccessKey         string
	ProductID         string
	SignMethod        string
	VendorDeviceName  string
	TimeoutSeconds    int
	ResolvedByChannel bool
}

// SendCommand OneNET 下行实现：
// 1) 先识别是否属于本插件（优先看 channel.plugin）
// 2) 解析通道/配置中的鉴权参数与产品参数
// 3) 将指定能力路由到 OneNET 的属性设置或服务调用接口
func (w *Worker) SendCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || cmd == nil {
		return nil, base.ErrCommandNotSupported
	}

	commandCtx, owned := w.buildCommandContext(deviceID, cmd)
	if !owned {
		return nil, base.ErrCommandNotSupported
	}
	if strings.TrimSpace(commandCtx.UserID) == "" || strings.TrimSpace(commandCtx.AccessKey) == "" {
		err := fmt.Errorf("onenet command config missing: cmiot_user_id/cmiot_access_key")
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: err.Error()}, err
	}

	vendorDeviceName, resolvedFrom, err := resolveOneNetVendorDeviceName(deviceID, commandCtx, cmd)
	if err != nil {
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: err.Error()}, err
	}
	if strings.TrimSpace(commandCtx.ProductID) == "" {
		err := fmt.Errorf("onenet command config missing: cmiot_product_id")
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: err.Error()}, err
	}

	endpoint, requestBody, action, routeErr := buildOneNetRequestBody(cmd, commandCtx.ProductID, vendorDeviceName)
	if routeErr == base.ErrCommandNotSupported {
		return nil, base.ErrCommandNotSupported
	}
	if routeErr != nil {
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: routeErr.Error()}, routeErr
	}

	timeoutSeconds := commandCtx.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 10
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	respBody, err := callOneNetAPI(reqCtx, endpoint, commandCtx.UserID, commandCtx.AccessKey, commandCtx.SignMethod, requestBody)
	if err != nil {
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: err.Error()}, err
	}

	responseData := map[string]interface{}{
		"plugin":               oneNetPluginName,
		"action":               action,
		"source_device_id":     deviceID,
		"vendor_device_name":   vendorDeviceName,
		"resolved_device_from": resolvedFrom,
		"product_id":           commandCtx.ProductID,
		"endpoint":             endpoint,
		"request":              requestBody,
		"response":             respBody,
	}
	if commandCtx.ChannelName != "" {
		responseData["channel"] = commandCtx.ChannelName
	}

	msgBytes, _ := json.Marshal(responseData)
	log.Printf("[ONENET][COMMAND] req=%s source=%s vendor=%s action=%s endpoint=%s", cmd.RequestID, deviceID, vendorDeviceName, action, endpoint)

	return &base.CommandReply{
		RequestID: cmd.RequestID,
		Code:      0,
		Message:   string(msgBytes),
		Data:      responseData,
	}, nil
}

func (w *Worker) buildCommandContext(deviceID string, cmd *base.DeviceCommand) (oneNetCommandContext, bool) {
	ctx := oneNetCommandContext{}

	channelRaw := asMap(cmd.Params["channel"])
	ctx.ChannelRaw = channelRaw
	owned := false
	if pluginName := strings.ToLower(strings.TrimSpace(firstNonEmptyString(
		asString(channelRaw["plugin"]),
		asString(channelRaw["plugin_name"]),
		asString(channelRaw["type"]),
	))); pluginName != "" {
		if pluginName != oneNetPluginName {
			return oneNetCommandContext{}, false
		}
		ctx.ResolvedByChannel = true
		owned = true
	}
	if !owned {
		owned = isLikelyOneNetDevice(deviceID)
	}
	if !owned {
		return oneNetCommandContext{}, false
	}

	ctx.ChannelName = strings.TrimSpace(firstNonEmptyString(
		asString(channelRaw["name"]),
		asString(channelRaw["channel_name"]),
		asString(channelRaw["receive_path"]),
	))

	ctx.UserID = strings.TrimSpace(firstNonEmptyString(
		asString(channelRaw["user_id"]),
		asString(channelRaw["cmiot_user_id"]),
		asString(cmd.Params["user_id"]),
		asString(cmd.Params["cmiot_user_id"]),
		strings.TrimSpace(os.Getenv("CMIOT_USER_ID")),
	))
	ctx.AccessKey = strings.TrimSpace(firstNonEmptyString(
		asString(channelRaw["access_key"]),
		asString(channelRaw["cmiot_access_key"]),
		asString(cmd.Params["access_key"]),
		asString(cmd.Params["cmiot_access_key"]),
		strings.TrimSpace(os.Getenv("CMIOT_ACCESS_KEY")),
	))
	ctx.ProductID = strings.TrimSpace(firstNonEmptyString(
		asString(channelRaw["product_id"]),
		asString(channelRaw["cmiot_product_id"]),
		asString(cmd.Params["product_id"]),
		asString(cmd.Params["cmiot_product_id"]),
		strings.TrimSpace(os.Getenv("CMIOT_PRODUCT_ID")),
	))
	ctx.SignMethod = strings.TrimSpace(firstNonEmptyString(
		asString(channelRaw["sign_method"]),
		asString(channelRaw["cmiot_sign_method"]),
		asString(cmd.Params["sign_method"]),
		asString(cmd.Params["cmiot_sign_method"]),
		strings.TrimSpace(os.Getenv("CMIOT_SIGN_METHOD")),
	))
	ctx.VendorDeviceName = strings.TrimSpace(firstNonEmptyString(
		asString(channelRaw["device_name"]),
		asString(channelRaw["vendor_device_name"]),
		asString(cmd.Params["device_name"]),
		asString(cmd.Params["vendor_device_name"]),
	))
	ctx.TimeoutSeconds = firstPositiveInt(
		asInt(channelRaw["timeout_seconds"]),
		asInt(channelRaw["command_timeout_seconds"]),
		asInt(cmd.Params["timeout_seconds"]),
	)

	for _, cfg := range w.configs {
		if ctx.UserID == "" {
			ctx.UserID = strings.TrimSpace(cfg.CMiotUserID)
		}
		if ctx.AccessKey == "" {
			ctx.AccessKey = strings.TrimSpace(cfg.CMiotAccessKey)
		}
		if ctx.ProductID == "" {
			ctx.ProductID = strings.TrimSpace(cfg.CMiotProductID)
		}
		if ctx.SignMethod == "" {
			ctx.SignMethod = strings.TrimSpace(cfg.CMiotSignMethod)
		}
		if ctx.TimeoutSeconds <= 0 {
			ctx.TimeoutSeconds = cfg.CommandTimeoutSeconds
		}
		if ctx.ChannelName == "" {
			ctx.ChannelName = strings.TrimSpace(cfg.ReceivePath)
		}
		if ctx.UserID != "" && ctx.AccessKey != "" && ctx.ProductID != "" {
			break
		}
	}
	if ctx.ProductID == "" {
		ctx.ProductID = pickOneNetProductIDFromHistory(deviceID)
	}

	if strings.TrimSpace(ctx.SignMethod) == "" {
		ctx.SignMethod = "sha1"
	}

	_ = deviceID
	return ctx, true
}

func isLikelyOneNetDevice(deviceID string) bool {
	if common.DB == nil {
		return false
	}
	target := strings.TrimSpace(deviceID)
	if target == "" {
		return false
	}
	var count int64
	err := common.DB.Table("device_data").
		Where("(unique_id = ? OR device_id = ?) AND (device_type = ? OR data_type = ? OR compony_name = ?)",
			target, target, "ONENET", "ONENET_PUSH", "OneNET").
		Count(&count).Error
	if err != nil {
		return false
	}
	return count > 0
}

func pickOneNetProductIDFromHistory(deviceID string) string {
	if common.DB == nil {
		return ""
	}
	target := strings.TrimSpace(deviceID)
	if target == "" {
		return ""
	}
	type row struct {
		Payload []byte `gorm:"column:payload"`
	}
	var record row
	err := common.DB.Table("device_data").
		Select("payload").
		Where("(unique_id = ? OR device_id = ?) AND (device_type = ? OR data_type = ? OR compony_name = ?)",
			target, target, "ONENET", "ONENET_PUSH", "OneNET").
		Order("id DESC").
		Limit(1).
		Take(&record).Error
	if err != nil || len(record.Payload) == 0 {
		return ""
	}
	var obj map[string]interface{}
	if err = json.Unmarshal(record.Payload, &obj); err != nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmptyString(
		asString(obj["productId"]),
		asString(obj["product_id"]),
	))
}

func resolveOneNetVendorDeviceName(sourceDeviceID string, ctx oneNetCommandContext, cmd *base.DeviceCommand) (string, string, error) {
	sourceDeviceID = strings.TrimSpace(sourceDeviceID)
	if strings.TrimSpace(ctx.VendorDeviceName) != "" {
		return strings.TrimSpace(ctx.VendorDeviceName), "channel", nil
	}

	if v := strings.TrimSpace(firstNonEmptyString(
		asString(cmd.Params["device_name"]),
		asString(cmd.Params["imei"]),
		asString(cmd.Params["sn"]),
	)); v != "" {
		return v, "params", nil
	}

	if sourceDeviceID != "" {
		if mapped, hit, err := common.ResolveMappedDeviceID(oneNetPluginName, sourceDeviceID); err == nil && hit && strings.TrimSpace(mapped) != "" {
			return strings.TrimSpace(mapped), "device_id_mapping", nil
		}
	}

	if common.DB != nil && sourceDeviceID != "" {
		type row struct {
			DeviceID string `gorm:"column:device_id"`
			Payload  []byte `gorm:"column:payload"`
		}
		var record row
		err := common.DB.Table("device_data").
			Select("device_id", "payload").
			Where("unique_id = ? OR device_id = ?", sourceDeviceID, sourceDeviceID).
			Order("id DESC").
			Limit(1).
			Take(&record).Error
		if err == nil {
			if extracted := extractDeviceNameFromPayload(record.Payload); extracted != "" {
				return extracted, "device_data.payload", nil
			}
			if strings.TrimSpace(record.DeviceID) != "" {
				return strings.TrimSpace(record.DeviceID), "device_data.device_id", nil
			}
		}
	}

	if sourceDeviceID != "" {
		return sourceDeviceID, "request_device_id", nil
	}
	return "", "", fmt.Errorf("cannot resolve onenet device_name")
}

func extractDeviceNameFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return ""
	}
	for _, key := range []string{"deviceName", "device_name", "imei", "sn", "devId", "dev_id"} {
		if val := strings.TrimSpace(asString(obj[key])); val != "" {
			return val
		}
	}
	return ""
}

func buildOneNetRequestBody(cmd *base.DeviceCommand, productID string, deviceName string) (string, map[string]interface{}, string, error) {
	method := strings.ToLower(strings.TrimSpace(cmd.Method))
	identifier := strings.ToLower(strings.TrimSpace(cmd.Identifier))

	if method == "service_invoke" || method == "thing.service.invoke" || inStringSet(oneNetServiceSet, identifier) {
		serviceIdentifier := identifier
		if serviceIdentifier == "" {
			return "", nil, "", fmt.Errorf("service identifier required")
		}
		if !inStringSet(oneNetServiceSet, serviceIdentifier) {
			return "", nil, "", base.ErrCommandNotSupported
		}
		return "https://iot-api.heclouds.com/thingmodel/call-service", map[string]interface{}{
			"product_id":  productID,
			"device_name": deviceName,
			"identifier":  serviceIdentifier,
			"params":      sanitizeCommandParams(cmd.Params),
		}, serviceIdentifier, nil
	}

	if method == "property_set" || method == "thing.property.set" || inStringSet(oneNetPropertySet, identifier) {
		if identifier != "" && !inStringSet(oneNetPropertySet, identifier) {
			return "", nil, "", base.ErrCommandNotSupported
		}

		params := sanitizeCommandParams(cmd.Params)
		if identifier != "" {
			if value, hit := pickPropertyValue(cmd.Params, identifier); hit {
				if strings.EqualFold(identifier, "family_numbers") {
					value = normalizeFamilyNumbersValue(value)
				}
				if strings.EqualFold(identifier, "location") {
					value = normalizeLocationValue(value)
				}
				if strings.EqualFold(identifier, "report_interval") {
					value = normalizeReportIntervalValue(value)
				}
				if strings.EqualFold(identifier, "function_keys") {
					value = normalizeFunctionKeysValue(value)
				}
				params = map[string]interface{}{identifier: value}
			}
		} else {
			params = pickAllowedProperties(params)
		}
		if len(params) == 0 {
			return "", nil, "", fmt.Errorf("property params required")
		}
		return "https://iot-api.heclouds.com/thingmodel/set-device-property", map[string]interface{}{
			"product_id":  productID,
			"device_name": deviceName,
			"params":      params,
		}, "set_device_property", nil
	}

	return "", nil, "", base.ErrCommandNotSupported
}

func inStringSet(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

func pickAllowedProperties(params map[string]interface{}) map[string]interface{} {
	if len(params) == 0 {
		return map[string]interface{}{}
	}
	result := make(map[string]interface{})
	for key, value := range params {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if !inStringSet(oneNetPropertySet, normalized) {
			continue
		}
		if normalized == "family_numbers" {
			value = normalizeFamilyNumbersValue(value)
		}
		if normalized == "location" {
			value = normalizeLocationValue(value)
		}
		if normalized == "report_interval" {
			value = normalizeReportIntervalValue(value)
		}
		if normalized == "function_keys" {
			value = normalizeFunctionKeysValue(value)
		}
		result[key] = value
	}

	if !hasLocationParam(result) {
		if locationValue, ok := buildLocationFromParams(params); ok {
			result["location"] = locationValue
		}
	}

	return result
}

func hasLocationParam(params map[string]interface{}) bool {
	if len(params) == 0 {
		return false
	}
	for key := range params {
		if strings.EqualFold(strings.TrimSpace(key), "location") {
			return true
		}
	}
	return false
}

func buildLocationFromParams(params map[string]interface{}) (map[string]interface{}, bool) {
	if len(params) == 0 {
		return nil, false
	}

	getByNames := func(names ...string) string {
		for key, value := range params {
			normalized := strings.ToLower(strings.TrimSpace(key))
			for _, name := range names {
				if normalized == name {
					return strings.TrimSpace(asString(value))
				}
			}
		}
		return ""
	}

	longitude := getByNames("longitude", "lon")
	latitude := getByNames("latitude", "lat")
	if longitude == "" || latitude == "" {
		return nil, false
	}

	return map[string]interface{}{
		"longitude": longitude,
		"latitude":  latitude,
	}, true
}

func normalizeFunctionKeysValue(value interface{}) interface{} {
	if value == nil {
		return []map[string]interface{}{}
	}

	if arrayValue, ok := value.([]interface{}); ok {
		result := make([]map[string]interface{}, 0, len(arrayValue))
		for _, item := range arrayValue {
			obj, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			result = append(result, map[string]interface{}{
				"function": asInt(obj["function"]),
				"param":    asInt(obj["param"]),
			})
		}
		return result
	}

	if arrayValue, ok := value.([]map[string]interface{}); ok {
		result := make([]map[string]interface{}, 0, len(arrayValue))
		for _, item := range arrayValue {
			result = append(result, map[string]interface{}{
				"function": asInt(item["function"]),
				"param":    asInt(item["param"]),
			})
		}
		return result
	}

	if text, ok := value.(string); ok {
		return normalizeFunctionKeysFromText(text)
	}

	return value
}

func normalizeFunctionKeysFromText(text string) []map[string]interface{} {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return []map[string]interface{}{}
	}

	if strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"") && len(trimmed) >= 2 {
		var unquoted string
		if err := json.Unmarshal([]byte(trimmed), &unquoted); err == nil {
			trimmed = strings.TrimSpace(unquoted)
		}
	}

	if strings.HasPrefix(trimmed, "[") {
		var arrayObj []map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &arrayObj); err == nil {
			result := make([]map[string]interface{}, 0, len(arrayObj))
			for _, item := range arrayObj {
				result = append(result, map[string]interface{}{
					"function": asInt(item["function"]),
					"param":    asInt(item["param"]),
				})
			}
			return result
		}
	}

	parts := strings.Split(strings.ReplaceAll(trimmed, "，", ","), ",")
	result := make([]map[string]interface{}, 0, len(parts)/2+1)
	for i := 0; i < len(parts); {
		functionValue := strings.TrimSpace(parts[i])
		if functionValue == "" {
			i++
			continue
		}

		functionInt := asInt(functionValue)
		paramInt := 0
		if i+1 < len(parts) {
			paramInt = asInt(strings.TrimSpace(parts[i+1]))
			i += 2
		} else {
			i++
		}

		result = append(result, map[string]interface{}{
			"function": functionInt,
			"param":    paramInt,
		})
	}
	return result
}

func normalizeLocationValue(value interface{}) interface{} {
	if value == nil {
		return value
	}

	if obj, ok := value.(map[string]interface{}); ok {
		return map[string]interface{}{
			"longitude": strings.TrimSpace(asString(obj["longitude"])),
			"latitude":  strings.TrimSpace(asString(obj["latitude"])),
		}
	}

	if text, ok := value.(string); ok {
		return normalizeLocationFromText(text)
	}

	return value
}

func normalizeLocationFromText(text string) interface{} {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return map[string]interface{}{}
	}

	if strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"") && len(trimmed) >= 2 {
		var unquoted string
		if err := json.Unmarshal([]byte(trimmed), &unquoted); err == nil {
			trimmed = strings.TrimSpace(unquoted)
		}
	}

	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			return map[string]interface{}{
				"longitude": strings.TrimSpace(asString(obj["longitude"])),
				"latitude":  strings.TrimSpace(asString(obj["latitude"])),
			}
		}
	}

	parts := strings.Split(strings.ReplaceAll(trimmed, "，", ","), ",")
	if len(parts) >= 2 {
		return map[string]interface{}{
			"longitude": strings.TrimSpace(parts[0]),
			"latitude":  strings.TrimSpace(parts[1]),
		}
	}

	return map[string]interface{}{
		"longitude": trimmed,
		"latitude":  "",
	}
}

func normalizeReportIntervalValue(value interface{}) interface{} {
	if value == nil {
		return value
	}
	if intValue, ok := toIntValue(value); ok {
		return intValue
	}
	return value
}

func toIntValue(value interface{}) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case int32:
		return int(typed), true
	case float64:
		return int(typed), true
	case float32:
		return int(typed), true
	case json.Number:
		if v, err := typed.Int64(); err == nil {
			return int(v), true
		}
		return 0, false
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		v, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return v, true
	default:
		return 0, false
	}
}

func normalizeFamilyNumbersValue(value interface{}) interface{} {
	if value == nil {
		return []map[string]interface{}{}
	}

	if arrayValue, ok := value.([]interface{}); ok {
		return normalizeFamilyNumbersFromArray(arrayValue)
	}

	if arrayValue, ok := value.([]map[string]interface{}); ok {
		result := make([]map[string]interface{}, 0, len(arrayValue))
		for _, item := range arrayValue {
			phone := strings.TrimSpace(asString(item["phone"]))
			if phone == "" {
				continue
			}
			result = append(result, map[string]interface{}{
				"phone": phone,
				"type":  asInt(item["type"]),
			})
		}
		return result
	}

	if text, ok := value.(string); ok {
		return normalizeFamilyNumbersFromText(text)
	}

	return value
}

func normalizeFamilyNumbersFromArray(arrayValue []interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(arrayValue))
	for _, item := range arrayValue {
		if obj, ok := item.(map[string]interface{}); ok {
			phone := strings.TrimSpace(asString(obj["phone"]))
			if phone == "" {
				continue
			}
			result = append(result, map[string]interface{}{
				"phone": phone,
				"type":  asInt(obj["type"]),
			})
			continue
		}

		phone := strings.TrimSpace(asString(item))
		if phone == "" {
			continue
		}
		result = append(result, map[string]interface{}{
			"phone": phone,
			"type":  0,
		})
	}
	return result
}

func normalizeFamilyNumbersFromText(text string) []map[string]interface{} {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return []map[string]interface{}{}
	}

	if strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"") && len(trimmed) >= 2 {
		var unquoted string
		if err := json.Unmarshal([]byte(trimmed), &unquoted); err == nil {
			trimmed = strings.TrimSpace(unquoted)
		}
	}

	if strings.HasPrefix(trimmed, "[") {
		var arrayObj []map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &arrayObj); err == nil {
			return normalizeFamilyNumbersFromObjectArray(arrayObj)
		}

		var arrayText []string
		if err := json.Unmarshal([]byte(trimmed), &arrayText); err == nil {
			result := make([]map[string]interface{}, 0, len(arrayText))
			for _, phone := range arrayText {
				p := strings.TrimSpace(phone)
				if p == "" {
					continue
				}
				result = append(result, map[string]interface{}{
					"phone": p,
					"type":  0,
				})
			}
			return result
		}
	}

	if strings.HasPrefix(trimmed, "{") {
		if singleObj := parseFamilyNumberSingleObject(trimmed); len(singleObj) > 0 {
			return singleObj
		}
		if joinedObj := parseFamilyNumberJoinedObjects(trimmed); len(joinedObj) > 0 {
			return joinedObj
		}
	}

	parts := strings.Split(strings.ReplaceAll(trimmed, "，", ","), ",")
	result := make([]map[string]interface{}, 0, len(parts))
	for i := 0; i < len(parts); {
		phone := strings.TrimSpace(parts[i])
		if phone == "" {
			i++
			continue
		}

		typeValue := 0
		if i+1 < len(parts) {
			next := strings.TrimSpace(parts[i+1])
			if isDigits(next) {
				typeValue = asInt(next)
				i += 2
			} else {
				i++
			}
		} else {
			i++
		}

		result = append(result, map[string]interface{}{
			"phone": phone,
			"type":  typeValue,
		})
	}
	return result
}

func normalizeFamilyNumbersFromObjectArray(arrayObj []map[string]interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(arrayObj))
	for _, item := range arrayObj {
		phone := strings.TrimSpace(asString(item["phone"]))
		if phone == "" {
			continue
		}
		result = append(result, map[string]interface{}{
			"phone": phone,
			"type":  asInt(item["type"]),
		})
	}
	return result
}

func parseFamilyNumberSingleObject(text string) []map[string]interface{} {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return nil
	}
	phone := strings.TrimSpace(asString(obj["phone"]))
	if phone == "" {
		return nil
	}
	return []map[string]interface{}{
		{
			"phone": phone,
			"type":  asInt(obj["type"]),
		},
	}
}

func parseFamilyNumberJoinedObjects(text string) []map[string]interface{} {
	candidate := "[" + text + "]"
	var arrayObj []map[string]interface{}
	if err := json.Unmarshal([]byte(candidate), &arrayObj); err != nil {
		return nil
	}
	return normalizeFamilyNumbersFromObjectArray(arrayObj)
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func pickPropertyValue(params map[string]interface{}, identifier string) (interface{}, bool) {
	if params == nil {
		return nil, false
	}
	if value, ok := params[identifier]; ok {
		return value, true
	}
	if value, ok := params["value"]; ok {
		return value, true
	}
	return nil, false
}

func sanitizeCommandParams(params map[string]interface{}) map[string]interface{} {
	if len(params) == 0 {
		return map[string]interface{}{}
	}
	result := make(map[string]interface{}, len(params))
	for key, value := range params {
		normalized := strings.ToLower(strings.TrimSpace(key))
		switch normalized {
		case "channel", "device_name", "vendor_device_name", "product_id", "cmiot_product_id", "user_id", "cmiot_user_id", "access_key", "cmiot_access_key", "sign_method", "cmiot_sign_method", "timeout_seconds":
			continue
		default:
			result[key] = value
		}
	}
	return result
}

func callOneNetAPI(ctx context.Context, endpoint, userID, accessKey, signMethod string, requestBody map[string]interface{}) (map[string]interface{}, error) {
	res := fmt.Sprintf("userid/%s", strings.TrimSpace(userID))
	et := fmt.Sprintf("%d", time.Now().Unix()+3600)
	auth, err := buildCMiotAuthorizationForCommand(accessKey, res, et, signMethod)
	if err != nil {
		return nil, err
	}

	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, fmt.Errorf("build request failed: %w", err)
	}
	req.Header.Set("authorization", auth)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call onenet failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	result := map[string]interface{}{}
	_ = json.Unmarshal(body, &result)
	if len(result) == 0 {
		result = map[string]interface{}{"raw": string(body)}
	}

	if code := asInt(result["code"]); code != 0 {
		return result, fmt.Errorf("onenet api failed code=%d msg=%s", code, strings.TrimSpace(asString(result["msg"])))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("onenet http status=%d", resp.StatusCode)
	}
	return result, nil
}

func buildCMiotAuthorizationForCommand(accessKey, res, et, method string) (string, error) {
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		method = "sha1"
	}
	if method != "sha1" && method != "sha256" {
		return "", fmt.Errorf("unsupported sign method: %s", method)
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(accessKey))
	if err != nil {
		return "", fmt.Errorf("base64 decode access_key failed: %w", err)
	}

	version := "2022-05-01"
	strForSign := et + "\n" + method + "\n" + res + "\n" + version

	var digest []byte
	switch method {
	case "sha1":
		h := hmac.New(sha1.New, key)
		_, _ = h.Write([]byte(strForSign))
		digest = h.Sum(nil)
	case "sha256":
		h := hmac.New(sha256.New, key)
		_, _ = h.Write([]byte(strForSign))
		digest = h.Sum(nil)
	}

	sign := base64.StdEncoding.EncodeToString(digest)
	return fmt.Sprintf(
		"version=%s&res=%s&et=%s&method=%s&sign=%s",
		version,
		url.QueryEscape(res),
		et,
		method,
		url.QueryEscape(sign),
	), nil
}

func asMap(value interface{}) map[string]interface{} {
	if value == nil {
		return map[string]interface{}{}
	}
	if typed, ok := value.(map[string]interface{}); ok {
		return typed
	}
	return map[string]interface{}{}
}

func asString(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

func asInt(value interface{}) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case int32:
		return int(typed)
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	case json.Number:
		v, _ := typed.Int64()
		return int(v)
	case string:
		var num int
		_, _ = fmt.Sscanf(strings.TrimSpace(typed), "%d", &num)
		return num
	default:
		return 0
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstPositiveInt(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
