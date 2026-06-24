package shanghai_xikali

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// ============================================================
// 希卡立设备数据解析器
//
// 支持两种设备类型：
//   1. 希卡立睡眠监测仪（床垫式）— WiFi/4G 双版本
//   2. 60G 跌倒雷达 L4 — WiFi/4G 双版本
//
// 声明分类：
//   - 消息类型 A（数据上报）→ /user/update → 解析后广播，不落库
//   - 消息类型 B（通知/警告）→ /user/warning → 解析后广播，不落库
// ============================================================

// 设备类型常量
const (
	DeviceTypeSleepMonitor = "希卡立睡眠监测仪" // 床垫式睡眠监测
	DeviceTypeFallRadar    = "60G跌倒雷达"  // 60G毫米波跌倒检测雷达
	DeviceTypeNotification = "希卡立设备通知"  // 设备上下线/警告通知
)

// msgCategory 表示 AMQP 消息的类别
type msgCategory int

const (
	msgUnknown        msgCategory = iota // 无法识别
	msgDataUpdate                        // 数据上报（/user/update）- 高频，解析后广播，不落库
	msgWarning                           // 设备通知（/user/warning）- 设备上下线等
	msgPlatformOutput                    // 平台结构化输出格式 — 识别 device_type/device_id 顶层字段，仅广播不落库
)

// detectMsgCategory 根据 topic 和 payload 判断消息类别。
// 注意：睡眠监测仪 4G 版数据平铺在顶层（无 items），与 warning 消息结构不同，
// 需要靠字段特征区分。
func detectMsgCategory(rawObj map[string]interface{}, topic string) msgCategory {
	// 1. 先看 topic
	if strings.HasSuffix(topic, "/user/warning") {
		return msgWarning
	}
	if strings.HasSuffix(topic, "/user/update") || strings.HasSuffix(topic, "/thing/event/property/post") {
		return msgDataUpdate
	}

	// 2. 没有 topic 时，通过 payload 字段特征判断
	// warning 消息一定有 title、content、type、typeStr、dateTime 等字段
	if _, hasTitle := rawObj["title"]; hasTitle {
		if _, hasContent := rawObj["content"]; hasContent {
			return msgWarning
		}
	}
	if _, hasTypeStr := rawObj["typeStr"]; hasTypeStr {
		return msgWarning
	}

	// 3. 有 items 字段 → 物模型数据上报
	if _, hasItems := rawObj["items"]; hasItems {
		return msgDataUpdate
	}

	// 4. 顶层有单字母缩写（O/S/H/R/I/D/A/X/M/B/V）→ 4G 数据上报
	shortKeys := []string{"O", "S", "H", "R", "D", "A", "X", "M", "B", "V", "I"}
	shortCount := 0
	for _, k := range shortKeys {
		if _, ok := rawObj[k]; ok {
			shortCount++
		}
	}
	if shortCount >= 5 {
		return msgDataUpdate
	}

	// 5. 有 id + dateTime → warning 类
	if _, hasID := rawObj["id"]; hasID {
		if _, hasDT := rawObj["dateTime"]; hasDT {
			return msgWarning
		}
	}

	// 6. 结构化输出格式检测：
	//    顶层有 device_type + device_id + (people_present / heart_rate / 等嵌套对象字段)
	//    这是平台二次处理后的格式，数据频繁，不应落库
	if _, hasDevType := rawObj["device_type"]; hasDevType {
		if _, hasDevID := rawObj["device_id"]; hasDevID {
			// 确认是结构化格式：检查是否有 people_present 或 heart_rate 等嵌套对象字段
			nestedMetricFields := []string{"people_present", "heart_rate", "respiratory_rate",
				"body_movement", "sleep_stage", "anxiety_score", "signal_amplitude"}
			metricCount := 0
			for _, f := range nestedMetricFields {
				if v, ok := rawObj[f]; ok {
					if _, isObj := v.(map[string]interface{}); isObj {
						metricCount++
					}
				}
			}
			if metricCount >= 2 {
				return msgPlatformOutput
			}
		}
	}

	return msgUnknown
}

// ============================================================
// 结构化解析输出（通用）
// ============================================================

// ParsedPayload 是所有设备解析后的统一结构。
// 原始 JSON 不保留，仅保留此结构中定义的字段。
type ParsedPayload struct {
	// ===== 通用字段 =====
	DeviceType      string `json:"device_type"`      // 设备类型（希卡立睡眠监测仪 / 60G跌倒雷达）
	DeviceID        string `json:"device_id"`        // 设备ID
	DeviceName      string `json:"device_name"`      // 设备备注名
	FirmwareVersion string `json:"firmware_version"` // 固件版本
	ReportTime      string `json:"report_time"`      // 数据时间（ISO8601 或接收时间）
	ProtocolVersion string `json:"protocol_version"` // WiFi / 4G

	// ===== 公共状态字段 =====
	PeoplePresent *BoolField `json:"people_present,omitempty"` // 有人/无人

	// ===== 睡眠监测仪专属字段 =====
	HeartRate       *IntField    `json:"heart_rate,omitempty"`       // 心率 (bpm)
	RespiratoryRate *IntField    `json:"respiratory_rate,omitempty"` // 呼吸率 (bpm)
	BodyMovement    *BoolField   `json:"body_movement,omitempty"`    // 有无大体动
	SleepStage      *StringField `json:"sleep_stage,omitempty"`      // 睡眠阶段
	SleepStageRaw   *IntField    `json:"sleep_stage_raw,omitempty"`  // 睡眠阶段原始值
	ApneaDuration   *IntField    `json:"apnea_duration,omitempty"`   // 呼吸暂停时长（秒）
	AnxietyScore    *IntField    `json:"anxiety_score,omitempty"`    // HRV 放松指数 (0-40)
	SignalQuality   *IntField    `json:"signal_quality,omitempty"`   // 信号强度 (RSSI)
	SignalAmplitude *IntField    `json:"signal_amplitude,omitempty"` // 体动幅度

	// ===== 跌倒雷达专属字段 =====
	FallStatus    *FallStatusField `json:"fall_status,omitempty"`    // 跌倒状态
	InstallConfig *InstallConfig   `json:"install_config,omitempty"` // 安装配置（只在配置变更时携带）

	// ===== 通知消息专属字段（/user/warning） =====
	NotificationType   *NotificationTypeField `json:"notification_type,omitempty"`    // 通知类型
	NotificationTitle  string                 `json:"notification_title,omitempty"`   // 通知标题
	NotificationBody   string                 `json:"notification_body,omitempty"`    // 通知内容
	NotificationTime   int64                  `json:"notification_time,omitempty"`    // 通知时间戳（毫秒）
	NotificationTypeID int                    `json:"notification_type_id,omitempty"` // 通知类型原始值

	// ===== 原始数据类型标记（用于区分不同消息类型的必要字段，dataType 计算用） =====
	Method string `json:"-"` // 阿里云物模型 method，不序列化，仅用于外部 DataType 计算
}

// BoolField 布尔字段
type BoolField struct {
	Value   bool   `json:"value"`
	Display string `json:"display"` // 人类可读描述
}

// IntField 整数字段
type IntField struct {
	Value   int    `json:"value"`
	Unit    string `json:"unit,omitempty"`    // 单位
	Range   string `json:"range,omitempty"`   // 有效范围描述
	Display string `json:"display,omitempty"` // 人类可读描述
}

// StringField 字符串字段
type StringField struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

// FallStatusField 跌倒状态字段
type FallStatusField struct {
	Alarm     bool   `json:"alarm"`                // 是否处于跌倒报警状态
	Display   string `json:"display"`              // "正常" / "跌倒报警"
	AlarmTime string `json:"alarm_time,omitempty"` // 跌倒发生时间
}

// NotificationTypeField 通知类型字段
type NotificationTypeField struct {
	Value   string `json:"value"`              // 人类可读类型
	TypeID  int    `json:"type_id,omitempty"`  // 原始类型数字
	TimeStr string `json:"time_str,omitempty"` // 格式化时间
}

// InstallConfig 雷达安装配置
type InstallConfig struct {
	Height        float64 `json:"height_m,omitempty"`         // 安装高度（米）
	RangeZB       float64 `json:"range_zb_m,omitempty"`       // Z轴后半轴（米）
	RangeZF       float64 `json:"range_zf_m,omitempty"`       // Z轴前半轴（米）
	RangeXR       float64 `json:"range_xr_m,omitempty"`       // X轴右半轴（米）
	RangeXL       float64 `json:"range_xl_m,omitempty"`       // X轴左半轴（米）
	Sensitivity   int     `json:"sensitivity,omitempty"`      // 跌倒检测灵敏度 (2-10)
	FallThreshold float64 `json:"fall_threshold_m,omitempty"` // 跌倒判定高度阈值（米）
}

// ============================================================
// 设备版本检测
// ============================================================

// deviceVersion 表示设备的通信版本
type deviceVersion int

const (
	versionUnknown deviceVersion = iota
	versionWiFi                  // WiFi 版（完整字段名 + gmtCreate）
	versionLTE                   // 4G 版（缩写字段名或无 gmtCreate）
)

// deviceClass 表示设备大类
type deviceClass int

const (
	classUnknown      deviceClass = iota
	classSleepMonitor             // 希卡立睡眠监测仪
	classFallRadar                // 60G 跌倒雷达
)

// getFields 返回要检测的字段集合。
// 优先从 items 中取（标准物模型），若没有 items 则直接从 rawObj 顶层取（平铺格式）。
func getFields(rawObj map[string]interface{}) map[string]interface{} {
	if items, ok := rawObj["items"].(map[string]interface{}); ok && len(items) > 0 {
		return items
	}
	// 平铺格式：数据直接在最顶层，排除 _topic _method _id _version 等元数据字段
	fields := make(map[string]interface{}, len(rawObj))
	for k, v := range rawObj {
		switch k {
		case "_topic", "_method", "_id", "_version", "id", "method", "version":
			continue
		default:
			fields[k] = v
		}
	}
	return fields
}

// detectDeviceInfo 从原始 JSON 中检测设备类型和版本。
//
// 判断逻辑：
//   - 检查是否有 DeviceID 字段 → 跌倒雷达（WiFi/4G 都用完整字段名）
//   - 检查 fields 是否有单字母缩写（O/S/H/R/I/D等）→ 睡眠监测仪 4G 版
//   - 检查是否有 HeartRate/RespiratoryRate 长字段名 → 睡眠监测仪 WiFi 版
//
// 注意：同时支持 items 内嵌套和平铺顶层两种结构。
func detectDeviceInfo(rawObj map[string]interface{}, topic string) (deviceClass deviceClass, version deviceVersion) {
	fields := getFields(rawObj)
	if len(fields) == 0 {
		return classUnknown, versionUnknown
	}

	// ========== 第一步：检测是否是跌倒雷达 L4 ==========
	// 跌倒雷达 WiFi/4G 版都使用完整字段名：DeviceID, Fall_flag, People_flag, Height 等
	if _, hasDeviceID := fields["DeviceID"]; hasDeviceID {
		// 跌倒雷达有 Fall_flag 字段
		if _, hasFallFlag := fields["Fall_flag"]; hasFallFlag {
			return classFallRadar, detectRadarVersion(fields, topic)
		}
		// 如果有 Height, ZB, ZF, XR, XL, Sensitivity, Threshold 等安装配置参数，也是雷达
		radarConfigFields := []string{"Height", "ZB", "ZF", "XR", "XL", "Sensitivity", "Threshold"}
		configCount := 0
		for _, f := range radarConfigFields {
			if _, ok := fields[f]; ok {
				configCount++
			}
		}
		if configCount >= 3 {
			return classFallRadar, detectRadarVersion(fields, topic)
		}
		// 有 DeviceID 但没有 Fall_flag，可能是睡眠监测仪的 WiFi 版（也有 DeviceID）
		// 继续往下检查
	}

	// ========== 第二步：检测是否是睡眠监测仪 ==========
	// 检测单字母缩写（4G 版）
	shortKeys := []string{"O", "S", "H", "R", "D", "A", "X", "M", "B", "V", "I"}
	shortCount := 0
	for _, k := range shortKeys {
		if _, ok := fields[k]; ok {
			shortCount++
		}
	}
	if shortCount >= 5 {
		return classSleepMonitor, versionLTE
	}

	// 检测长字段名（WiFi 版）
	longFields := []string{"HeartRate", "RespiratoryRate", "People_flag", "moving", "HRV"}
	longCount := 0
	for _, f := range longFields {
		if _, ok := fields[f]; ok {
			longCount++
		}
	}
	if longCount >= 3 {
		return classSleepMonitor, versionWiFi
	}

	// 如果有 DeviceID + HeartRate 等，也是睡眠监测仪 WiFi 版
	if _, hasHR := fields["HeartRate"]; hasHR {
		return classSleepMonitor, versionWiFi
	}
	if _, hasRR := fields["RespiratoryRate"]; hasRR {
		return classSleepMonitor, versionWiFi
	}

	// 纯字段提取：只有 I 和其它单字母，但不够5个（可能只有 I + 少量字段），
	// 尝试通过 I 是纯数字 + 有单字母字段来判定
	idStr := extractStr(fields, "I")
	if idStr != "" && isNumeric(idStr) {
		// 至少有 H(心率) R(呼吸率) O(有人) 中至少两个 → 睡眠监测仪 4G
		miniCheck := []string{"H", "R", "O"}
		miniCount := 0
		for _, k := range miniCheck {
			if _, ok := fields[k]; ok {
				miniCount++
			}
		}
		if miniCount >= 2 {
			return classSleepMonitor, versionLTE
		}
	}

	return classUnknown, versionUnknown
}

// detectRadarVersion 检测雷达版本：WiFi 版有 gmtCreate，4G 版无
func detectRadarVersion(items map[string]interface{}, topic string) deviceVersion {
	if _, hasGMT := items["gmtCreate"]; hasGMT {
		return versionWiFi
	}
	// 从 topic 后缀判断
	if strings.HasSuffix(topic, "/thing/event/property/post") {
		return versionWiFi
	}
	if strings.HasSuffix(topic, "/user/update") {
		return versionLTE
	}
	return versionLTE
}

// ============================================================
// 睡眠监测仪解析
// ============================================================

// parseSleepMonitor 解析希卡立睡眠监测仪数据。
func parseSleepMonitor(rawObj map[string]interface{}, topic string, version deviceVersion) *ParsedPayload {
	flat := flattenItems(rawObj)
	now := time.Now()

	p := &ParsedPayload{
		DeviceType:      DeviceTypeSleepMonitor,
		DeviceID:        extractStr(flat, "DeviceID", "I"),
		DeviceName:      extractStr(flat, "DeviceName", "Nickname"),
		FirmwareVersion: extractStr(flat, "Version", "V"),
		ReportTime:      extractReportTime(flat, topic, now),
		Method:          extractStr(rawObj, "method"),
	}

	if version == versionWiFi {
		p.ProtocolVersion = "WiFi"
	} else {
		p.ProtocolVersion = "4G"
	}

	// ---- 有人/无人 ----
	if v, ok := extractInt(flat, "People_flag", "O"); ok {
		p.PeoplePresent = &BoolField{
			Value:   v == 1,
			Display: boolDisplay(v == 1, "有人", "无人"),
		}
	}

	// ---- 心率 ----
	if v, ok := extractInt(flat, "HeartRate", "H"); ok {
		p.HeartRate = &IntField{
			Value:   v,
			Unit:    "bpm",
			Range:   "0-150",
			Display: hrDisplay(v, p.PeoplePresent),
		}
	}

	// ---- 呼吸率 ----
	if v, ok := extractInt(flat, "RespiratoryRate", "R"); ok {
		p.RespiratoryRate = &IntField{
			Value:   v,
			Unit:    "bpm",
			Range:   "0-50",
			Display: rrDisplay(v, p.PeoplePresent),
		}
	}

	// ---- 有无大体动 ----
	if v, ok := extractInt(flat, "moving", "M"); ok {
		p.BodyMovement = &BoolField{
			Value:   v == 1,
			Display: boolDisplay(v == 1, "有大体动", "无大体动"),
		}
	}

	// ---- 睡眠阶段 ----
	if v, ok := extractInt(flat, "D", "D"); ok {
		p.SleepStageRaw = &IntField{
			Value:   v,
			Display: fmt.Sprintf("原始值=%d", v),
		}
		stage := decodeSleepStage(v)
		p.SleepStage = &StringField{
			Value:   stage,
			Display: fmt.Sprintf("当前睡眠阶段: %s", stage),
		}
	}

	// ---- 呼吸暂停时长 ----
	if v, ok := extractInt(flat, "E", "A"); ok {
		apneaDisplay := "无呼吸暂停"
		if v > 0 {
			apneaDisplay = fmt.Sprintf("呼吸暂停持续%d秒", v)
		}
		p.ApneaDuration = &IntField{
			Value:   v,
			Unit:    "秒",
			Display: apneaDisplay,
		}
	}

	// ---- HRV 放松指数 ----
	if v, ok := extractInt(flat, "HRV", "X"); ok {
		p.AnxietyScore = &IntField{
			Value:   v,
			Range:   "0-40",
			Display: anxietyDisplay(v),
		}
	}

	// ---- 信号强度 ----
	if v, ok := extractInt(flat, "RSSI", "B"); ok {
		p.SignalQuality = &IntField{
			Value:   v,
			Display: fmt.Sprintf("信号强度=%d", v),
		}
	}

	// ---- 体动幅度 ----
	if v, ok := extractInt(flat, "Amp_value", "S"); ok {
		p.SignalAmplitude = &IntField{
			Value:   v,
			Display: fmt.Sprintf("体动幅度=%d", v),
		}
	}

	return p
}

// ============================================================
// 跌倒雷达解析
// ============================================================

// parseFallRadar 解析 60G 跌倒雷达数据。
func parseFallRadar(rawObj map[string]interface{}, topic string, version deviceVersion) *ParsedPayload {
	flat := flattenItems(rawObj)
	now := time.Now()

	p := &ParsedPayload{
		DeviceType:      DeviceTypeFallRadar,
		DeviceID:        extractStr(flat, "DeviceID"),
		DeviceName:      extractStr(flat, "Nickname"),
		FirmwareVersion: extractStr(flat, "Version"),
		ReportTime:      extractReportTime(flat, topic, now),
		Method:          extractStr(rawObj, "method"),
	}

	if version == versionWiFi {
		p.ProtocolVersion = "WiFi"
	} else {
		p.ProtocolVersion = "4G"
	}

	// ---- 有人/无人 ----
	if v, ok := extractInt(flat, "People_flag"); ok {
		p.PeoplePresent = &BoolField{
			Value:   v == 1,
			Display: boolDisplay(v == 1, "有人", "无人"),
		}
	}

	// ---- 跌倒状态 ----
	if v, ok := extractInt(flat, "Fall_flag"); ok {
		isAlarm := v == 1
		p.FallStatus = &FallStatusField{
			Alarm:     isAlarm,
			Display:   boolDisplay(!isAlarm, "正常", "跌倒报警"),
			AlarmTime: time.Now().Format(time.RFC3339),
		}
	}

	// ---- 安装配置（只有在配置变更时才携带完整配置） ----
	config := &InstallConfig{}
	hasConfig := false

	if v, ok := extractFloat64(flat, "Height"); ok {
		config.Height = v
		hasConfig = true
	}
	if v, ok := extractFloat64(flat, "ZB"); ok {
		config.RangeZB = v
		hasConfig = true
	}
	if v, ok := extractFloat64(flat, "ZF"); ok {
		config.RangeZF = v
		hasConfig = true
	}
	if v, ok := extractFloat64(flat, "XR"); ok {
		config.RangeXR = v
		hasConfig = true
	}
	if v, ok := extractFloat64(flat, "XL"); ok {
		config.RangeXL = v
		hasConfig = true
	}
	if v, ok := extractInt(flat, "Sensitivity"); ok {
		config.Sensitivity = v
		hasConfig = true
	}
	if v, ok := extractFloat64(flat, "Threshold"); ok {
		config.FallThreshold = v
		hasConfig = true
	}

	if hasConfig {
		p.InstallConfig = config
	}

	return p
}

// ============================================================
// 结构化输出格式解析（platform_output）
//
// 当平台已将原始设备数据二次处理为人类可读的结构化格式时使用。
// 格式特征：
//   - 顶层字段：device_type, device_id, device_name, report_time, protocol_version
//   - 各指标字段为嵌套对象：{ "value": ..., "unit": "...", "range": "...", "display": "..." }
//   - 无 items 嵌套，无单字母缩写
//
// 该格式数据频率高，标记为 msgPlatformOutput，仅广播不落库（_NODB）。
// ============================================================

// parsePlatformOutput 解析平台结构化输出格式。
func parsePlatformOutput(rawObj map[string]interface{}, topic string) *ParsedPayload {
	p := &ParsedPayload{
		DeviceType:      extractStr(rawObj, "device_type"),
		DeviceID:        extractStr(rawObj, "device_id"),
		DeviceName:      extractStr(rawObj, "device_name"),
		FirmwareVersion: extractStr(rawObj, "firmware_version"),
		ReportTime:      extractReportTime(rawObj, topic, time.Now()),
		ProtocolVersion: extractStr(rawObj, "protocol_version"),
		Method:          extractStr(rawObj, "method"),
	}

	// ---- 有人/无人 ----
	if v, ok := extractPlatformBool(rawObj, "people_present"); ok {
		p.PeoplePresent = &BoolField{Value: v, Display: boolDisplay(v, "有人", "无人")}
	}

	// ---- 心率 ----
	if rawHeartRate, ok := rawObj["heart_rate"].(map[string]interface{}); ok {
		if v, err := extractFromObj[int](rawHeartRate, "value"); err == nil {
			p.HeartRate = &IntField{
				Value:   v,
				Unit:    extractStr(rawHeartRate, "unit"),
				Range:   extractStr(rawHeartRate, "range"),
				Display: extractStr(rawHeartRate, "display"),
			}
		}
	}

	// ---- 呼吸率 ----
	if rawRR, ok := rawObj["respiratory_rate"].(map[string]interface{}); ok {
		if v, err := extractFromObj[int](rawRR, "value"); err == nil {
			p.RespiratoryRate = &IntField{
				Value:   v,
				Unit:    extractStr(rawRR, "unit"),
				Range:   extractStr(rawRR, "range"),
				Display: extractStr(rawRR, "display"),
			}
		}
	}

	// ---- 有无大体动 ----
	if v, ok := extractPlatformBool(rawObj, "body_movement"); ok {
		p.BodyMovement = &BoolField{Value: v, Display: boolDisplay(v, "有大体动", "无大体动")}
	}

	// ---- 睡眠阶段 ----
	if rawSleep, ok := rawObj["sleep_stage"].(map[string]interface{}); ok {
		p.SleepStage = &StringField{
			Value:   extractStr(rawSleep, "value"),
			Display: extractStr(rawSleep, "display"),
		}
	}

	// ---- 睡眠阶段原始值 ----
	if rawSleepRaw, ok := rawObj["sleep_stage_raw"].(map[string]interface{}); ok {
		if v, err := extractFromObj[int](rawSleepRaw, "value"); err == nil {
			p.SleepStageRaw = &IntField{
				Value:   v,
				Display: fmt.Sprintf("原始值=%d", v),
			}
		}
	}

	// ---- 呼吸暂停时长 ----
	if rawApnea, ok := rawObj["apnea_duration"].(map[string]interface{}); ok {
		if v, err := extractFromObj[int](rawApnea, "value"); err == nil {
			p.ApneaDuration = &IntField{
				Value:   v,
				Unit:    extractStr(rawApnea, "unit"),
				Display: extractStr(rawApnea, "display"),
			}
		}
	}

	// ---- HRV 放松指数（焦虑评分） ----
	if rawAnxiety, ok := rawObj["anxiety_score"].(map[string]interface{}); ok {
		if v, err := extractFromObj[int](rawAnxiety, "value"); err == nil {
			p.AnxietyScore = &IntField{
				Value:   v,
				Range:   extractStr(rawAnxiety, "range"),
				Display: extractStr(rawAnxiety, "display"),
			}
		}
	}

	// ---- 信号幅度（体动幅度） ----
	if rawAmp, ok := rawObj["signal_amplitude"].(map[string]interface{}); ok {
		if v, err := extractFromObj[int](rawAmp, "value"); err == nil {
			p.SignalAmplitude = &IntField{
				Value:   v,
				Display: extractStr(rawAmp, "display"),
			}
		}
	}

	return p
}

// extractPlatformBool 从结构化输出格式中提取布尔值。
// 格式：{ "value": true/false, "display": "..." }
func extractPlatformBool(rawObj map[string]interface{}, key string) (bool, bool) {
	obj, ok := rawObj[key].(map[string]interface{})
	if !ok {
		return false, false
	}
	v, err := extractFromObj[bool](obj, "value")
	if err != nil {
		return false, false
	}
	return v, true
}

// extractFromObj 从 map[string]interface{} 中提取指定类型的值。
// 支持 bool、int、float64、string 类型。
func extractFromObj[T bool | int | float64 | string](obj map[string]interface{}, key string) (T, error) {
	v, ok := obj[key]
	if !ok {
		return *new(T), fmt.Errorf("key %s not found", key)
	}
	switch typed := v.(type) {
	case T:
		return typed, nil
	case float64:
		if _, ok := any(*new(T)).(int); ok {
			return any(int(typed)).(T), nil
		}
		return any(typed).(T), nil
	case bool:
		if _, ok := any(*new(T)).(bool); ok {
			return any(typed).(T), nil
		}
	case string:
		if _, ok := any(*new(T)).(string); ok {
			return any(typed).(T), nil
		}
	}
	return *new(T), fmt.Errorf("cannot convert %T to %T", v, *new(T))
}

// ============================================================
// 通知消息解析（/user/warning）
// ============================================================

// parseWarning 解析设备通知消息。
//
// 示例消息：
//
//	{
//	  "_id": "35687551",
//	  "_topic": "/i0m7ESANTkX/35687551/user/warning",
//	  "content": "11:22分，设备上线！",
//	  "dateTime": 1780543353527,
//	  "deviceName": "X1LTE_S01B03N127",
//	  "id": "35687551",
//	  "title": "设备上线",
//	  "type": 3,
//	  "typeStr": "设备上线"
//	}
func parseWarning(rawObj map[string]interface{}, topic string) *ParsedPayload {
	p := &ParsedPayload{
		DeviceType: DeviceTypeNotification,
		DeviceID:   extractStr(rawObj, "id", "deviceId", "I"),
		DeviceName: extractStr(rawObj, "deviceName", "device_name"),
		ReportTime: extractReportTime(rawObj, topic, time.Now()),
	}

	// 通知标题
	p.NotificationTitle = extractStr(rawObj, "title")

	// 通知内容
	p.NotificationBody = extractStr(rawObj, "content")

	// 通知时间戳（毫秒）
	if v, ok := extractInt(rawObj, "dateTime"); ok {
		p.NotificationTime = int64(v)
	}

	// 通知类型
	typeVal, hasType := extractInt(rawObj, "type")
	typeStr := extractStr(rawObj, "typeStr")
	if hasType {
		p.NotificationTypeID = typeVal
		displayType := typeStr
		if displayType == "" {
			displayType = decodeWarningType(typeVal)
		}
		// 从时间戳格式化时间
		timeStr := ""
		if p.NotificationTime > 0 {
			sec := p.NotificationTime / 1000
			timeStr = time.Unix(sec, 0).Format("15:04:05")
		}
		p.NotificationType = &NotificationTypeField{
			Value:   displayType,
			TypeID:  typeVal,
			TimeStr: timeStr,
		}
	}

	return p
}

// decodeWarningType 将通知类型数字转为可读描述。
func decodeWarningType(t int) string {
	switch t {
	case 0:
		return "设备上线" // deviceOnline
	case 1:
		return "设备离线" // deviceOffline
	case 2:
		return "告警" // alarm
	case 3:
		return "设备上线" // X1LTE 特定
	case 4:
		return "设备离线" // X1LTE 特定
	default:
		return fmt.Sprintf("未知通知(%d)", t)
	}
}

// ============================================================
// 全局入口：ParsePayload
// ============================================================

// ParsePayload 是解析器的统一入口。
// 接收原始 AMQP 消息的 rawObj（已解析的 map），返回解析后的结构化数据。
//
// 返回值：
//   - parsed:    解析后的结构化数据（非 nil）
//   - deviceID:  设备 ID
//   - msgCat:    消息类别（msgDataUpdate / msgWarning / msgUnknown）
//   - isParsed:  是否成功识别设备类型并结构化解析
func ParsePayload(rawObj map[string]interface{}, topic string) (parsed *ParsedPayload, deviceID string, msgCat msgCategory, isParsed bool) {
	// 第一步：判断消息类别
	msgCat = detectMsgCategory(rawObj, topic)

	// 第二步：如果是通知消息，走通知解析
	if msgCat == msgWarning {
		parsed = parseWarning(rawObj, topic)
		log.Printf("[PARSER] warning detected device_id=%s title=%s",
			parsed.DeviceID, parsed.NotificationTitle)
		return parsed, parsed.DeviceID, msgCat, true
	}

	// 第三步：如果是平台结构化输出格式，走专门解析器
	if msgCat == msgPlatformOutput {
		parsed = parsePlatformOutput(rawObj, topic)
		log.Printf("[PARSER] platform output format detected device_id=%s device_type=%s",
			parsed.DeviceID, parsed.DeviceType)
		return parsed, parsed.DeviceID, msgCat, true
	}

	// 第四步：检测设备类型并解析
	cls, ver := detectDeviceInfo(rawObj, topic)

	switch cls {
	case classSleepMonitor:
		parsed = parseSleepMonitor(rawObj, topic, ver)
		log.Printf("[PARSER] sleep monitor detected version=%s device_id=%s",
			parsed.ProtocolVersion, parsed.DeviceID)
		return parsed, parsed.DeviceID, msgCat, true

	case classFallRadar:
		parsed = parseFallRadar(rawObj, topic, ver)
		log.Printf("[PARSER] fall radar detected version=%s device_id=%s",
			parsed.ProtocolVersion, parsed.DeviceID)
		return parsed, parsed.DeviceID, msgCat, true

	default:
		// 未识别设备类型，回退提取基础信息
		log.Printf("[PARSER] unknown device type, fallback to basic extraction")
		parsed = basicFallback(rawObj, topic)
		return parsed, parsed.DeviceID, msgCat, false
	}
}

// basicFallback 当无法识别设备类型时的回退解析。
func basicFallback(rawObj map[string]interface{}, topic string) *ParsedPayload {
	flat := flattenItems(rawObj)
	p := &ParsedPayload{
		DeviceType: "未知设备",
		DeviceID:   extractStr(flat, "DeviceID", "I"),
		DeviceName: extractStr(flat, "DeviceName", "Nickname"),
		ReportTime: extractReportTime(flat, topic, time.Now()),
	}
	return p
}

// ============================================================
// 辅助解码函数
// ============================================================

// decodeSleepStage 解码睡眠阶段。
//
// 原始值 = D 字段的值。
// 睡眠阶段 = D % 8
//
//	0 = 设备初始化中
//	1 = 清醒
//	2 = REM（快速眼动）
//	3 = 浅睡
//	4 = 深睡
//	其他值 = 未知
func decodeSleepStage(rawValue int) string {
	stage := rawValue % 8
	switch stage {
	case 0:
		return "设备初始化中"
	case 1:
		return "清醒"
	case 2:
		return "REM快速眼动"
	case 3:
		return "浅睡"
	case 4:
		return "深睡"
	default:
		return fmt.Sprintf("未知(%d)", stage)
	}
}

// hrDisplay 心率的展示描述。
func hrDisplay(hr int, peoplePresent *BoolField) string {
	if peoplePresent != nil && !peoplePresent.Value {
		return "无人状态，心率值忽略"
	}
	if hr <= 0 {
		return "无人状态或无效数据"
	}
	if hr > 150 {
		return fmt.Sprintf("%d bpm（超出正常范围）", hr)
	}
	return fmt.Sprintf("%d bpm", hr)
}

// rrDisplay 呼吸率的展示描述。
func rrDisplay(rr int, peoplePresent *BoolField) string {
	if peoplePresent != nil && !peoplePresent.Value {
		return "无人状态，呼吸率忽略"
	}
	if rr <= 0 {
		return "无人状态或无效数据"
	}
	if rr > 50 {
		return fmt.Sprintf("%d bpm（超出正常范围）", rr)
	}
	return fmt.Sprintf("%d bpm", rr)
}

// anxietyDisplay HRV 放松指数展示。
func anxietyDisplay(score int) string {
	switch {
	case score <= 0:
		return "无人/无效数据"
	case score <= 10:
		return fmt.Sprintf("%d（放松度低，可能紧张或不适）", score)
	case score <= 20:
		return fmt.Sprintf("%d（放松度中等）", score)
	case score <= 30:
		return fmt.Sprintf("%d（放松度良好）", score)
	default:
		return fmt.Sprintf("%d（非常放松）", score)
	}
}

// ============================================================
// 扁平化 items
// ============================================================

// flattenItems 将阿里云物模型 items 结构展开为扁平 map。
//
// 输入：{"items": {"DeviceID": {"value": "xxx"}, "HeartRate": {"value": 75}}}
// 输出：{"DeviceID": "xxx", "HeartRate": 75}
//
// 也处理 4G 版无嵌套 value 的情况：
// 输入：{"items": {"O": 1, "H": 75, "R": 18}}
// 输出：{"O": 1, "H": 75, "R": 18}
func flattenItems(obj map[string]interface{}) map[string]interface{} {
	items, ok := obj["items"].(map[string]interface{})
	if !ok || len(items) == 0 {
		// 无 items，尝试直接在顶层提取（部分消息结构）
		flat := make(map[string]interface{})
		for k, v := range obj {
			if k == "_topic" || k == "_method" || k == "_id" || k == "_version" {
				continue
			}
			flat[k] = v
		}
		return flat
	}
	flat := make(map[string]interface{}, len(items))
	for key, val := range items {
		valObj, ok := val.(map[string]interface{})
		if !ok {
			// 非嵌套结构，直接取值（4G 版单字母缩写）
			flat[key] = val
			continue
		}
		// WiFi 版标准结构：{ "value": xxx }
		if v, exists := valObj["value"]; exists {
			// 如果 value 里还是嵌套 map（如 time_now），取 value 本身
			if vm, isMap := v.(map[string]interface{}); isMap {
				// 递归取内层 value
				if vv, vvExists := vm["value"]; vvExists {
					flat[key] = vv
				} else {
					flat[key] = vm
				}
			} else {
				flat[key] = v
			}
		} else {
			// 没有 value 字段，取整个对象
			flat[key] = valObj
		}
	}
	return flat
}

// ============================================================
// 提取工具函数
// ============================================================

// extractStr 从扁平 map 中按优先级提取字符串值。
func extractStr(flat map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := flat[key]; ok {
			return strings.TrimSpace(fmt.Sprintf("%v", v))
		}
	}
	return ""
}

// extractInt 从扁平 map 中按优先级提取整数值。
func extractInt(flat map[string]interface{}, keys ...string) (int, bool) {
	for _, key := range keys {
		if v, ok := flat[key]; ok {
			switch typed := v.(type) {
			case float64:
				return int(typed), true
			case int:
				return typed, true
			case int64:
				return int(typed), true
			case json.Number:
				if f, err := typed.Float64(); err == nil {
					return int(f), true
				}
			case string:
				// 尝试解析数字字符串
				var f float64
				if _, err := fmt.Sscanf(typed, "%f", &f); err == nil {
					return int(f), true
				}
			case bool:
				if typed {
					return 1, true
				}
				return 0, true
			}
		}
	}
	return 0, false
}

// extractFloat64 从扁平 map 中按优先级提取浮点值。
func extractFloat64(flat map[string]interface{}, keys ...string) (float64, bool) {
	for _, key := range keys {
		if v, ok := flat[key]; ok {
			switch typed := v.(type) {
			case float64:
				return typed, true
			case int:
				return float64(typed), true
			case int64:
				return float64(typed), true
			case json.Number:
				if f, err := typed.Float64(); err == nil {
					return f, true
				}
			case string:
				var f float64
				if _, err := fmt.Sscanf(typed, "%f", &f); err == nil {
					return f, true
				}
			}
		}
	}
	return 0, false
}

// extractReportTime 提取数据上报时间。
// WiFi 版优先使用 gmtCreate，4G 版使用服务器接收时间。
func extractReportTime(flat map[string]interface{}, topic string, now time.Time) string {
	// 优先使用 gmtCreate
	if v := extractStr(flat, "gmtCreate"); v != "" {
		return v
	}
	// 其次使用 time_now
	if v := extractStr(flat, "time_now"); v != "" {
		return v
	}
	// 最后使用接收时间
	return now.Format(time.RFC3339)
}

// boolDisplay 生成布尔字段的人类可读描述。
func boolDisplay(cond bool, trueVal, falseVal string) string {
	if cond {
		return trueVal
	}
	return falseVal
}

// isNumeric 判断字符串是否全部为数字。
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
