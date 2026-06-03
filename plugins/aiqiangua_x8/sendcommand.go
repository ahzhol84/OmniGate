package aiqiangua_x8

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type sosContactPayload struct {
	Name        string
	Number      string
	DialFlag    int
	NetworkType string
	Index       int
	Clear       bool
}

type deviceEditPayload struct {
	NetworkType string
	Settings    map[string]string
}

type commandSession struct {
	Client *http.Client
}

var deviceEditFieldTypes = map[string]string{
	"sos_dial_cycle_times":   "int",
	"frequency_location":     "int",
	"frequency_step":         "int",
	"frequency_heartrate":    "int",
	"theshold_heartrate_h":   "int",
	"theshold_heartrate_l":   "int",
	"incoming_restriction":   "bool",
	"profile":                "int",
	"sleep_period_begin":     "string",
	"sleep_period_end":       "string",
	"step_objective":         "int",
	"pedometer_enable":       "bool",
	"sleep_enable":           "bool",
	"sim_phone":              "string",
	"fall_enable":            "bool",
	"track_enable":           "bool",
	"heartrate_enable":       "bool",
	"bloodoxygen_enable":     "bool",
	"bloodpressure_enable":   "bool",
	"power_down_enable":      "bool",
	"short_message_enable":   "bool",
	"gps_enable":             "bool",
	"alertreply_enable":      "bool",
	"theshold_bloodoxygen_h": "int",
	"theshold_bloodoxygen_l": "int",
	"systolic_pressure_h":    "int",
	"diastolic_pressure_l":   "int",
	"callmode":               "int",
	"sim_phone_type":         "string",
	"name":                   "string",
	"theshold_low_battery":   "int",
	"sos_sendmail":           "bool",
	"exercise_heartrate_h":   "int",
	"exercise_heartrate_l":   "int",
}

// SendCommand 实现爱牵挂 X8 的紧急联系人设置下行。
// 入参：ctx context.Context 上下文，不可为空；deviceID string 设备ID，不可为空；cmd *base.DeviceCommand 命令对象，不可为空
// 处理过程：解析命令参数，支持SOS联系人设置或设备编辑，调用相应API发送命令
// 出参：*base.CommandReply 命令回复，包含请求ID、状态码、消息和数据；error 错误信息
// 副作用：进行HTTP请求（IO操作）
// @Author ahzhol
func (w *Worker) SendCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || cmd == nil {
		return nil, base.ErrCommandNotSupported
	}
	log.Printf("[AIQG-X8][COMMAND] recv req=%s source_device=%s method=%s identifier=%s", cmd.RequestID, deviceID, cmd.Method, cmd.Identifier)

	cfg, err := w.selectCommandConfig()
	if err != nil {
		return &base.CommandReply{
			RequestID: cmd.RequestID,
			Code:      -1,
			Message:   err.Error(),
		}, err
	}

	payload, supported := parseSOSPayload(cmd, cfg)
	if !supported {
		editPayload, editSupported, parseErr := parseDeviceEditPayload(cmd, cfg)
		if parseErr != nil {
			log.Printf("[AIQG-X8][COMMAND] invalid req=%s source_device=%s err=%v", cmd.RequestID, deviceID, parseErr)
			return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: parseErr.Error()}, parseErr
		}
		if !editSupported {
			log.Printf("[AIQG-X8][COMMAND] skip req=%s source_device=%s reason=unsupported_payload", cmd.RequestID, deviceID)
			return nil, base.ErrCommandNotSupported
		}

		vendorDeviceID, resolveFrom, resolveErr := resolveVendorDeviceID(deviceID)
		if resolveErr != nil {
			log.Printf("[AIQG-X8][COMMAND] resolve failed req=%s source_device=%s err=%v", cmd.RequestID, deviceID, resolveErr)
			return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: resolveErr.Error()}, resolveErr
		}

		log.Printf("[AIQG-X8][COMMAND] mapped req=%s source_device=%s resolved_device=%s from=%s action=device_edit fields=%d", cmd.RequestID, deviceID, vendorDeviceID, resolveFrom, len(editPayload.Settings))

		if err = w.setDeviceSettings(ctx, cfg, vendorDeviceID, editPayload); err != nil {
			log.Printf("[AIQG-X8][COMMAND] send failed req=%s resolved_device=%s action=device_edit err=%v", cmd.RequestID, vendorDeviceID, err)
			return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: err.Error()}, err
		}

		responseData := map[string]interface{}{
			"action":       "device_edit",
			"source_id":    deviceID,
			"device_id":    vendorDeviceID,
			"network_type": editPayload.NetworkType,
			"settings":     editPayload.Settings,
		}
		echoMessageBytes, _ := json.Marshal(responseData)
		log.Printf("[AIQG-X8][COMMAND] send ok req=%s resolved_device=%s action=device_edit fields=%v", cmd.RequestID, vendorDeviceID, sortedSettingKeys(editPayload.Settings))
		return &base.CommandReply{RequestID: cmd.RequestID, Code: 0, Message: string(echoMessageBytes), Data: responseData}, nil
	}

	vendorDeviceID, resolveFrom, resolveErr := resolveVendorDeviceID(deviceID)
	if resolveErr != nil {
		log.Printf("[AIQG-X8][COMMAND] resolve failed req=%s source_device=%s err=%v", cmd.RequestID, deviceID, resolveErr)
		return &base.CommandReply{
			RequestID: cmd.RequestID,
			Code:      -1,
			Message:   resolveErr.Error(),
		}, resolveErr
	}

	log.Printf("[AIQG-X8][COMMAND] mapped req=%s source_device=%s resolved_device=%s from=%s index=%d clear=%t", cmd.RequestID, deviceID, vendorDeviceID, resolveFrom, payload.Index, payload.Clear)

	if err = w.setSOSContact(ctx, cfg, vendorDeviceID, payload); err != nil {
		log.Printf("[AIQG-X8][COMMAND] send failed req=%s resolved_device=%s index=%d clear=%t err=%v", cmd.RequestID, vendorDeviceID, payload.Index, payload.Clear, err)
		return &base.CommandReply{
			RequestID: cmd.RequestID,
			Code:      -1,
			Message:   err.Error(),
		}, err
	}

	log.Printf("[AIQG-X8][COMMAND] send ok req=%s resolved_device=%s index=%d clear=%t name=%q num=%q", cmd.RequestID, vendorDeviceID, payload.Index, payload.Clear, payload.Name, payload.Number)
	responseData := map[string]interface{}{
		"source_id":    deviceID,
		"device_id":    vendorDeviceID,
		"sos_index":    payload.Index,
		"name":         payload.Name,
		"num":          payload.Number,
		"dial_flag":    payload.DialFlag,
		"network_type": payload.NetworkType,
		"clear":        payload.Clear,
	}
	echoMessageBytes, _ := json.Marshal(responseData)

	return &base.CommandReply{
		RequestID: cmd.RequestID,
		Code:      0,
		Message:   string(echoMessageBytes),
		Data:      responseData,
	}, nil
}

// sortedSettingKeys 对设置映射的键进行排序。
// 入参：settings map[string]string 设置映射，不可为空
// 处理过程：提取键并排序返回
// 出参：[]string 排序后的键列表
// @Author ahzhol
func sortedSettingKeys(settings map[string]string) []string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// selectCommandConfig 选择有效的命令配置。
// 入参：无
// 处理过程：遍历配置列表，返回第一个有用户名和密码的配置
// 出参：ConfigItem 配置项；error 错误信息
// @Author ahzhol
func (w *Worker) selectCommandConfig() (ConfigItem, error) {
	for _, cfg := range w.configs {
		if strings.TrimSpace(cfg.Username) != "" && strings.TrimSpace(cfg.Password) != "" {
			return cfg, nil
		}
	}
	return ConfigItem{}, fmt.Errorf("aiqiangua_x8 command config missing username/password")
}

// parseSOSPayload 解析SOS联系人设置的负载。
// 入参：cmd *base.DeviceCommand 命令对象，不可为空；cfg ConfigItem 配置项，不可为空
// 处理过程：从命令参数中提取姓名、号码、索引等信息，构造SOS负载
// 出参：sosContactPayload SOS联系人负载；bool 是否支持
// @Author ahzhol
func parseSOSPayload(cmd *base.DeviceCommand, cfg ConfigItem) (sosContactPayload, bool) {
	method := strings.ToLower(strings.TrimSpace(cmd.Method))
	identifier := strings.ToLower(strings.TrimSpace(cmd.Identifier))
	if method != "" && method != "property_set" && method != "thing.property.set" {
		return sosContactPayload{}, false
	}

	if cmd.Params == nil {
		return sosContactPayload{}, false
	}

	nested := map[string]interface{}{}
	if value, ok := cmd.Params[identifier]; ok {
		if asMap, ok2 := value.(map[string]interface{}); ok2 {
			nested = asMap
		}
	}

	name := firstNonEmptyString(
		asString(cmd.Params["name"]),
		asString(cmd.Params["contact_name"]),
		asString(nested["name"]),
	)
	number := firstNonEmptyString(
		asString(cmd.Params["num"]),
		asString(cmd.Params["phone"]),
		asString(cmd.Params["contact_phone"]),
		asString(nested["num"]),
	)

	clear := asBool(cmd.Params["clear"]) ||
		asBool(cmd.Params["remove"]) ||
		asBool(cmd.Params["delete"]) ||
		asBool(nested["clear"]) ||
		identifier == "sos_numbers_clear" ||
		identifier == "sos_numbers_delete"
	if clear {
		name = ""
		number = ""
	}
	if !clear && (name == "" || number == "") {
		return sosContactPayload{}, false
	}

	dialFlag := firstPositiveInt(
		asInt(cmd.Params["dial_flag"]),
		asInt(cmd.Params["dialFlag"]),
		asInt(nested["dial_flag"]),
	)
	if dialFlag == 0 {
		dialFlag = 1
	}
	if clear {
		dialFlag = 0
	}

	index := firstPositiveInt(
		asInt(cmd.Params["sos_index"]),
		asInt(cmd.Params["index"]),
		asInt(cmd.Params["slot"]),
		asInt(nested["sos_index"]),
	)
	if index == 0 {
		index = cfg.SOSIndex
	}

	networkType := firstNonEmptyString(
		asString(cmd.Params["network_type"]),
		asString(cmd.Params["network"]),
		asString(nested["network_type"]),
		strings.TrimSpace(cfg.NetworkType),
	)
	if networkType == "" {
		networkType = "4g"
	}

	supportedIdentifier := identifier == "" ||
		identifier == "sos_numbers" ||
		identifier == "sos_contact" ||
		identifier == "emergency_contact" ||
		identifier == "emergency_number"
	if !supportedIdentifier {
		return sosContactPayload{}, false
	}

	return sosContactPayload{
		Name:        name,
		Number:      number,
		DialFlag:    dialFlag,
		NetworkType: networkType,
		Index:       index,
		Clear:       clear,
	}, true
}

// parseDeviceEditPayload 解析设备编辑设置的负载。
// 入参：cmd *base.DeviceCommand 命令对象，不可为空；cfg ConfigItem 配置项，不可为空
// 处理过程：从命令参数中提取设备设置字段，构造编辑负载
// 出参：deviceEditPayload 设备编辑负载；bool 是否支持；error 错误信息
// @Author ahzhol
func parseDeviceEditPayload(cmd *base.DeviceCommand, cfg ConfigItem) (deviceEditPayload, bool, error) {
	method := strings.ToLower(strings.TrimSpace(cmd.Method))
	if method != "" && method != "property_set" && method != "thing.property.set" {
		return deviceEditPayload{}, false, nil
	}
	if cmd.Params == nil {
		return deviceEditPayload{}, false, nil
	}

	identifier := strings.ToLower(strings.TrimSpace(cmd.Identifier))
	nested := map[string]interface{}{}
	if identifier != "" {
		if value, ok := cmd.Params[identifier]; ok {
			if asMap, ok2 := value.(map[string]interface{}); ok2 {
				nested = asMap
			}
		}
	}

	settings := make(map[string]string)
	for field, fieldType := range deviceEditFieldTypes {
		value, exists := lookupSettingValue(field, cmd.Params, nested)
		if !exists {
			continue
		}
		normalized, ok, err := normalizeEditSettingValue(field, fieldType, value)
		if err != nil {
			return deviceEditPayload{}, false, err
		}
		if ok {
			settings[field] = normalized
		}
	}

	if len(settings) == 0 {
		return deviceEditPayload{}, false, nil
	}

	networkType := firstNonEmptyString(
		asString(cmd.Params["network_type"]),
		asString(cmd.Params["network"]),
		asString(nested["network_type"]),
		strings.TrimSpace(cfg.NetworkType),
	)
	if networkType == "" {
		networkType = "4g"
	}

	return deviceEditPayload{NetworkType: networkType, Settings: settings}, true, nil
}

// lookupSettingValue 查找设置字段的值。
// 入参：field string 字段名，不可为空；top map[string]interface{} 顶级参数映射，不可为空；nested map[string]interface{} 嵌套参数映射，不可为空
// 处理过程：在顶级和嵌套映射中查找字段值
// 出参：interface{} 值；bool 是否存在
// @Author ahzhol
func lookupSettingValue(field string, top map[string]interface{}, nested map[string]interface{}) (interface{}, bool) {
	if value, ok := top[field]; ok {
		return value, true
	}
	if value, ok := nested[field]; ok {
		return value, true
	}
	return nil, false
}

// normalizeEditSettingValue 规范化编辑设置的值。
// 入参：field string 字段名，不可为空；fieldType string 字段类型，不可为空；value interface{} 值，可为空
// 处理过程：根据字段类型转换值
// 出参：string 规范化后的值；bool 是否成功；error 错误信息
// @Author ahzhol
func normalizeEditSettingValue(field, fieldType string, value interface{}) (string, bool, error) {
	switch fieldType {
	case "bool":
		if normalized, ok := to01Bool(value); ok {
			return normalized, true, nil
		}
		return "", false, fmt.Errorf("invalid bool value for %s", field)
	case "int":
		s := strings.TrimSpace(asString(value))
		if s == "" {
			return "", false, nil
		}
		if _, err := strconv.Atoi(s); err != nil {
			return "", false, fmt.Errorf("invalid int value for %s", field)
		}
		return s, true, nil
	default:
		return asString(value), true, nil
	}
}

// to01Bool 将值转换为0或1的字符串表示布尔值。
// 入参：value interface{} 值，可为空
// 处理过程：根据值类型判断布尔值，返回"0"或"1"
// 出参：string 布尔字符串；bool 是否成功转换
// @Author ahzhol
func to01Bool(value interface{}) (string, bool) {
	if value == nil {
		return "", false
	}
	switch v := value.(type) {
	case bool:
		if v {
			return "1", true
		}
		return "0", true
	case int:
		if v == 0 {
			return "0", true
		}
		return "1", true
	case int32:
		if v == 0 {
			return "0", true
		}
		return "1", true
	case int64:
		if v == 0 {
			return "0", true
		}
		return "1", true
	case float32:
		if v == 0 {
			return "0", true
		}
		return "1", true
	case float64:
		if v == 0 {
			return "0", true
		}
		return "1", true
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		switch s {
		case "1", "true", "yes", "on":
			return "1", true
		case "0", "false", "no", "off":
			return "0", true
		default:
			return "", false
		}
	default:
		s := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", value)))
		switch s {
		case "1", "true", "yes", "on":
			return "1", true
		case "0", "false", "no", "off":
			return "0", true
		default:
			return "", false
		}
	}
}

// resolveVendorDeviceID 解析供应商设备ID。
// 入参：sourceID string 源设备ID，不可为空
// 处理过程：检查映射表或数据库，提取供应商设备ID
// 出参：string 供应商设备ID；string 来源；error 错误信息
// 副作用：查询数据库（IO操作）
// @Author ahzhol
func resolveVendorDeviceID(sourceID string) (string, string, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return "", "", fmt.Errorf("device id is empty")
	}
	if looksLikeVendorDeviceID(sourceID) {
		return sourceID, "source_device", nil
	}

	if mappedDeviceID, hit, err := common.ResolveMappedDeviceID("aiqiangua_x8", sourceID); err != nil {
		return "", "", fmt.Errorf("resolve mapping table failed for unique_id=%s: %w", sourceID, err)
	} else if hit {
		if looksLikeVendorDeviceID(mappedDeviceID) {
			return mappedDeviceID, "device_id_mapping", nil
		}
	}

	if common.DB == nil {
		return "", "", fmt.Errorf("database unavailable, cannot resolve unique_id=%s to vendor device id", sourceID)
	}

	var row struct {
		DeviceID string `gorm:"column:device_id"`
		Payload  []byte `gorm:"column:payload"`
	}
	err := common.DB.Table("device_data").
		Select("device_id", "payload").
		Where("unique_id = ? OR device_id = ?", sourceID, sourceID).
		Order("id DESC").
		Limit(1).
		Take(&row).Error
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve unique_id=%s to vendor device id: %w", sourceID, err)
	}

	if looksLikeVendorDeviceID(row.DeviceID) {
		return strings.TrimSpace(row.DeviceID), "device_data.device_id", nil
	}

	if fromPayload := extractVendorDeviceIDFromPayload(row.Payload); looksLikeVendorDeviceID(fromPayload) {
		return fromPayload, "device_data.payload", nil
	}

	return "", "", fmt.Errorf("resolved record for unique_id=%s but vendor device id is empty", sourceID)
}

// extractVendorDeviceIDFromPayload 从JSON负载中提取供应商设备ID。
// 入参：payload []byte JSON负载，可为空
// 处理过程：解析JSON，查找设备ID字段
// 出参：string 供应商设备ID
// @Author ahzhol
func extractVendorDeviceIDFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var root map[string]interface{}
	if err := json.Unmarshal(payload, &root); err != nil {
		return ""
	}
	for _, key := range []string{"imei", "device_id", "deviceid", "devID", "devId"} {
		if value := asString(root[key]); value != "" {
			return value
		}
	}
	if form, ok := root["form"].(map[string]interface{}); ok {
		for _, key := range []string{"imei", "device_id", "deviceid", "devID", "devId"} {
			if value := asString(form[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

// looksLikeVendorDeviceID 检查值是否像供应商设备ID（15位数字IMEI）。
// 入参：value string 值，不可为空
// 处理过程：检查长度为15且全为数字
// 出参：bool 是否匹配
// @Author ahzhol
func looksLikeVendorDeviceID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	// 爱牵挂设备标识使用 IMEI，通常固定 15 位数字。
	if len(value) != 15 {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// setSOSContact 设置SOS联系人。
// 入参：ctx context.Context 上下文，不可为空；cfg ConfigItem 配置项，不可为空；deviceID string 设备ID，不可为空；payload sosContactPayload 负载，不可为空
// 处理过程：登录API，发送POST请求设置SOS联系人
// 出参：error 错误信息
// 副作用：进行HTTP请求（IO操作）
// @Author ahzhol
func (w *Worker) setSOSContact(ctx context.Context, cfg ConfigItem, deviceID string, payload sosContactPayload) error {
	baseURL := normalizedAPIBaseURL(cfg)
	commandURL := fmt.Sprintf("%s/api/device/%s/%s/sos_numbers/%d/", baseURL, url.PathEscape(deviceID), url.PathEscape(payload.NetworkType), payload.Index)
	form := url.Values{}
	if payload.Clear {
		form.Set("clear", "1")
	} else {
		form.Set("name", payload.Name)
		form.Set("dial_flag", strconv.Itoa(payload.DialFlag))
		form.Set("num", payload.Number)
	}
	log.Printf("[AIQG-X8][COMMAND] final request action=set_sos_contact method=POST url=%s content_type=application/x-www-form-urlencoded body=%s", commandURL, form.Encode())

	return w.postFormWithAuthRetry(ctx, cfg, commandURL, form, "set sos contact")
}

// setDeviceSettings 设置设备设置。
// 入参：ctx context.Context 上下文，不可为空；cfg ConfigItem 配置项，不可为空；deviceID string 设备ID，不可为空；payload deviceEditPayload 负载，不可为空
// 处理过程：登录API，发送POST请求设置设备设置
// 出参：error 错误信息
// 副作用：进行HTTP请求（IO操作）
// @Author ahzhol
func (w *Worker) setDeviceSettings(ctx context.Context, cfg ConfigItem, deviceID string, payload deviceEditPayload) error {
	baseURL := normalizedAPIBaseURL(cfg)
	commandURL := fmt.Sprintf("%s/api/device/%s/%s/edit/", baseURL, url.PathEscape(deviceID), url.PathEscape(payload.NetworkType))
	form := url.Values{}
	for key, value := range payload.Settings {
		form.Set(key, value)
	}

	return w.postFormWithAuthRetry(ctx, cfg, commandURL, form, "set device settings")
}

// normalizedAPIBaseURL 返回规范化后的爱牵挂 API 基地址。
// 入参：cfg ConfigItem 配置项，不可为空
// 处理过程：去除空白与末尾斜杠，必要时回退默认地址
// 出参：string API 基地址
// @Author ahzhol
func normalizedAPIBaseURL(cfg ConfigItem) string {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	if baseURL == "" {
		baseURL = "http://api.aiqiangua.com:8888"
	}
	return baseURL
}

// postFormWithAuthRetry 使用缓存会话发送表单请求，认证失效时自动重登并重试一次。
// 入参：ctx context.Context 上下文，不可为空；cfg ConfigItem 配置项，不可为空；requestURL string 请求地址，不可为空；form url.Values 表单参数，不可为空；action string 动作描述，不可为空
// 处理过程：优先复用缓存会话请求，若返回401/403则清理缓存、重新登录并重试一次
// 出参：error 错误信息
// 副作用：进行HTTP请求（IO操作）
// @Author ahzhol
func (w *Worker) postFormWithAuthRetry(ctx context.Context, cfg ConfigItem, requestURL string, form url.Values, action string) error {
	retryAuth, err := w.withCommandSession(ctx, cfg, func(client *http.Client) (bool, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(form.Encode()))
		if reqErr != nil {
			return false, fmt.Errorf("build %s request failed: %w", action, reqErr)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		log.Printf("[AIQG-X8][COMMAND] request action=%s method=POST url=%s body=%s", action, requestURL, form.Encode())

		resp, doErr := client.Do(req)
		if doErr != nil {
			return false, fmt.Errorf("%s failed: %w", action, doErr)
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		trimmedBody := strings.TrimSpace(string(body))
		log.Printf("[AIQG-X8][COMMAND] response action=%s status=%d body=%s", action, resp.StatusCode, trimmedBody)

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return isAuthFailedStatus(resp.StatusCode), fmt.Errorf("%s status=%d body=%s", action, resp.StatusCode, trimmedBody)
		}

		if businessErr, authExpired := parseBusinessResponseError(trimmedBody); businessErr != "" {
			return authExpired, fmt.Errorf("%s status=%d body=%s", action, resp.StatusCode, businessErr)
		}

		return false, nil
	})
	if retryAuth {
		log.Printf("[AIQG-X8][COMMAND] auth retry failed action=%s err=%v", action, err)
	}
	return err
}

// withCommandSession 使用缓存会话执行请求，认证失败时重登并重试一次。
// 入参：ctx context.Context 上下文，不可为空；cfg ConfigItem 配置项，不可为空；do func(*http.Client)(bool,error) 请求函数，不可为空
// 处理过程：优先获取缓存会话；无缓存则登录并缓存；请求认证失败时清理缓存后重登重试一次
// 出参：bool 是否发生过认证重试；error 错误信息
// 副作用：可能触发登录请求与缓存更新（IO操作）
// @Author ahzhol
func (w *Worker) withCommandSession(ctx context.Context, cfg ConfigItem, do func(client *http.Client) (bool, error)) (bool, error) {
	client, ok := w.getCommandSessionClient(cfg)
	if !ok {
		var err error
		client, err = w.loginAndStoreCommandSession(ctx, cfg)
		if err != nil {
			return false, err
		}
	}

	retryAuth, err := do(client)
	if !retryAuth {
		return false, err
	}

	w.clearCommandSession(cfg)
	client, loginErr := w.loginAndStoreCommandSession(ctx, cfg)
	if loginErr != nil {
		return true, fmt.Errorf("auth expired and relogin failed: %w", loginErr)
	}

	_, retryErr := do(client)
	return true, retryErr
}

// loginAndStoreCommandSession 登录并缓存会话。
// 入参：ctx context.Context 上下文，不可为空；cfg ConfigItem 配置项，不可为空
// 处理过程：创建新会话并调用登录接口，登录成功后写入会话缓存
// 出参：*http.Client 可复用的HTTP客户端；error 错误信息
// 副作用：进行HTTP请求并更新内存缓存（IO操作）
// @Author ahzhol
func (w *Worker) loginAndStoreCommandSession(ctx context.Context, cfg ConfigItem) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar failed: %w", err)
	}
	client := &http.Client{Jar: jar}

	baseURL := normalizedAPIBaseURL(cfg)
	loginURL := baseURL + "/api/auth/login"
	loginForm := url.Values{}
	loginForm.Set("username", strings.TrimSpace(cfg.Username))
	loginForm.Set("password", strings.TrimSpace(cfg.Password))

	loginReq, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(loginForm.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build login request failed: %w", err)
	}
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	loginResp, err := client.Do(loginReq)
	if err != nil {
		return nil, fmt.Errorf("aiqiangua login failed: %w", err)
	}
	defer func() { _ = loginResp.Body.Close() }()
	if loginResp.StatusCode < 200 || loginResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(loginResp.Body, 4096))
		return nil, fmt.Errorf("aiqiangua login status=%d body=%s", loginResp.StatusCode, strings.TrimSpace(string(body)))
	}

	w.setCommandSessionClient(cfg, client)
	return client, nil
}

// commandSessionKey 生成命令会话缓存键。
// 入参：cfg ConfigItem 配置项，不可为空
// 处理过程：基于API地址和用户名构造稳定键
// 出参：string 缓存键
// @Author ahzhol
func commandSessionKey(cfg ConfigItem) string {
	return normalizedAPIBaseURL(cfg) + "|" + strings.TrimSpace(cfg.Username)
}

// getCommandSessionClient 获取缓存会话客户端。
// 入参：cfg ConfigItem 配置项，不可为空
// 处理过程：从会话缓存中读取客户端
// 出参：*http.Client 客户端；bool 是否命中
// @Author ahzhol
func (w *Worker) getCommandSessionClient(cfg ConfigItem) (*http.Client, bool) {
	key := commandSessionKey(cfg)
	w.commandSessionMu.RLock()
	defer w.commandSessionMu.RUnlock()
	if w.commandSessions == nil {
		return nil, false
	}
	session, ok := w.commandSessions[key]
	if !ok || session == nil || session.Client == nil {
		return nil, false
	}
	return session.Client, true
}

// setCommandSessionClient 设置缓存会话客户端。
// 入参：cfg ConfigItem 配置项，不可为空；client *http.Client 客户端，不可为空
// 处理过程：将客户端写入会话缓存
// 出参：无
// @Author ahzhol
func (w *Worker) setCommandSessionClient(cfg ConfigItem, client *http.Client) {
	if client == nil {
		return
	}
	key := commandSessionKey(cfg)
	w.commandSessionMu.Lock()
	defer w.commandSessionMu.Unlock()
	if w.commandSessions == nil {
		w.commandSessions = make(map[string]*commandSession)
	}
	w.commandSessions[key] = &commandSession{Client: client}
}

// clearCommandSession 清理缓存会话。
// 入参：cfg ConfigItem 配置项，不可为空
// 处理过程：删除对应缓存会话
// 出参：无
// @Author ahzhol
func (w *Worker) clearCommandSession(cfg ConfigItem) {
	key := commandSessionKey(cfg)
	w.commandSessionMu.Lock()
	defer w.commandSessionMu.Unlock()
	if w.commandSessions == nil {
		return
	}
	delete(w.commandSessions, key)
}

// isAuthFailedStatus 判断HTTP状态是否代表认证失效。
// 入参：status int 状态码
// 处理过程：匹配常见认证失效状态
// 出参：bool 是否认证失效
// @Author ahzhol
func isAuthFailedStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// parseBusinessResponseError 解析业务层返回并提取错误信息及认证失效标记。
// 入参：body string 响应体字符串，可为空
// 处理过程：尝试解析JSON中的success/error_code/error_desc，识别业务失败与认证过期
// 出参：string 错误信息；bool 是否认证失效
// @Author ahzhol
func parseBusinessResponseError(body string) (string, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", false
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", false
	}

	rawSuccess, hasSuccess := payload["success"]
	success := true
	if hasSuccess {
		success = asBool(rawSuccess)
	}
	if success {
		return "", false
	}

	errorCode := strings.TrimSpace(asString(payload["error_code"]))
	errorDesc := strings.TrimSpace(asString(payload["error_desc"]))
	if errorDesc == "" {
		errorDesc = strings.TrimSpace(asString(payload["message"]))
	}
	if errorDesc == "" {
		errorDesc = strings.TrimSpace(asString(payload["msg"]))
	}
	if errorDesc == "" {
		errorDesc = "business response indicates failure"
	}

	authExpired := errorCode == "101" || strings.Contains(errorDesc, "登录状态") || strings.Contains(errorDesc, "重新登陆") || strings.Contains(errorDesc, "重新登录")
	return errorDesc, authExpired
}

// asString 将值转换为字符串。
// 入参：value interface{} 值，可为空
// 处理过程：转换为字符串并去除空格
// 出参：string 字符串值
// @Author ahzhol
func asString(value interface{}) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

// asInt 将值转换为整数。
// 入参：value interface{} 值，可为空
// 处理过程：根据类型转换，失败返回0
// 出参：int 整数值
// @Author ahzhol
func asInt(value interface{}) int {
	if value == nil {
		return 0
	}
	switch v := value.(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float32:
		return int(v)
	case float64:
		return int(v)
	case string:
		iv, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return iv
	default:
		iv, err := strconv.Atoi(strings.TrimSpace(fmt.Sprintf("%v", value)))
		if err != nil {
			return 0
		}
		return iv
	}
}

// asBool 将值转换为布尔值。
// 入参：value interface{} 值，可为空
// 处理过程：根据类型判断布尔值
// 出参：bool 布尔值
// @Author ahzhol
func asBool(value interface{}) bool {
	if value == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case int32:
		return v != 0
	case int64:
		return v != 0
	case float32:
		return v != 0
	case float64:
		return v != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "1" || s == "true" || s == "yes" || s == "on"
	default:
		s := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", value)))
		return s == "1" || s == "true" || s == "yes" || s == "on"
	}
}

// firstNonEmptyString 返回第一个非空字符串。
// 入参：values ...string 字符串列表，可为空
// 处理过程：遍历列表，返回第一个非空值
// 出参：string 非空字符串或空字符串
// @Author ahzhol
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

// firstPositiveInt 返回第一个正整数。
// 入参：values ...int 整数列表，可为空
// 处理过程：遍历列表，返回第一个大于0的值
// 出参：int 正整数或0
// @Author ahzhol
func firstPositiveInt(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
