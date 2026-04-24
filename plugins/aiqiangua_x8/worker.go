package aiqiangua_x8

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ConfigItem struct {
	ComponyName  string   `json:"compony_name"`
	ListenAddr   string   `json:"listen_addr"`
	ReceivePath  string   `json:"receive_path"`
	PathPrefix   string   `json:"path_prefix"`
	DeviceType   string   `json:"device_type"`
	UniquePrefix string   `json:"unique_prefix"`
	AllowedIPs   []string `json:"allowed_ips"`
}

type Worker struct {
	configs []ConfigItem
	servers []*http.Server
}

// Init 初始化插件配置。
// 入参：configs 为原始 JSON 配置数组。
// 处理：逐条反序列化并补齐默认值（监听地址、接收路径、路径前缀、设备类型），同时规范路径格式并记录日志。
// 出参：成功返回 nil；配置解析失败返回 error。
func (w *Worker) Init(configs []json.RawMessage) error {
	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d] parse failed: %w", i, err)
		}
		if strings.TrimSpace(cfg.ListenAddr) == "" {
			cfg.ListenAddr = ":8090"
		}
		if strings.TrimSpace(cfg.ReceivePath) == "" {
			cfg.ReceivePath = "/push/aiqiangua/x8"
		}
		cfg.ReceivePath = normalizePath(cfg.ReceivePath)

		if strings.TrimSpace(cfg.PathPrefix) == "" {
			cfg.PathPrefix = "/push/aiqiangua/x8"
		}
		cfg.PathPrefix = normalizePath(cfg.PathPrefix)

		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "AIQIANGUA_X8"
		}

		w.configs = append(w.configs, cfg)
		log.Printf("[AIQG-X8] config[%d] listen=%s receive_path=%s path_prefix=%s", i+1, cfg.ListenAddr, cfg.ReceivePath, cfg.PathPrefix)
	}
	return nil
}

// Start 启动所有配置对应的 HTTP 接收服务。
// 入参：ctx 为生命周期上下文，out 为输出设备数据通道。
// 处理：为每个配置并发启动服务并等待；收到取消信号后优雅关闭所有 server。
// 出参：无。
// @Author ahzhol
func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	var wg sync.WaitGroup

	for idx, cfg := range w.configs {
		wg.Add(1)
		go func(i int, c ConfigItem) {
			defer wg.Done()
			w.runServer(ctx, out, i, c)
		}(idx, cfg)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		for _, s := range w.servers {
			if s == nil {
				continue
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Shutdown(shutdownCtx)
			cancel()
		}
		<-done
	case <-done:
	}
}

// runServer 运行单个配置实例的 HTTP 服务。
// 入参：ctx 为生命周期上下文，out 为输出通道，index 为实例序号，cfg 为实例配置。
// 处理：注册精确路径与前缀路径处理器，启动 ListenAndServe，并在上下文结束时退出。
// 出参：无。
// @Author ahzhol
func (w *Worker) runServer(ctx context.Context, out chan<- *base.DeviceData, index int, cfg ConfigItem) {
	mux := http.NewServeMux()

	handler := func(rw http.ResponseWriter, req *http.Request) {
		w.handlePush(rw, req, out, cfg, index+1)
	}

	mux.HandleFunc(cfg.ReceivePath, handler)
	mux.HandleFunc(cfg.PathPrefix+"/", handler)

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}
	w.servers = append(w.servers, server)

	log.Printf("[AIQG-X8-%d] listening at %s, receive=%s, prefix=%s", index+1, cfg.ListenAddr, cfg.ReceivePath, cfg.PathPrefix)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[AIQG-X8-%d] server exited: %v", index+1, err)
		}
	}()

	<-ctx.Done()
}

// handlePush 处理设备推送请求并转发为统一设备数据。
// 入参：rw/req 为 HTTP 响应与请求，out 为输出通道，cfg 为当前实例配置，workerIndex 为实例编号。
// 处理：校验方法与来源 IP、解析表单、识别事件类型与设备标识、构建 payload、写入输出通道并返回业务响应。
// 出参：无。
// @Author ahzhol
func (w *Worker) handlePush(rw http.ResponseWriter, req *http.Request, out chan<- *base.DeviceData, cfg ConfigItem, workerIndex int) {
	if req.Method != http.MethodPost && req.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	remoteIP := clientIP(req)
	if len(cfg.AllowedIPs) > 0 && !ipAllowed(remoteIP, cfg.AllowedIPs) {
		http.Error(rw, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(rw, "read body failed", http.StatusBadRequest)
		return
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(strings.NewReader(string(body)))

	values, formErr := parseForm(req)
	if formErr != nil {
		http.Error(rw, "invalid form body", http.StatusBadRequest)
		return
	}
	normalizeStepNumberField(values)

	eventType := detectEventType(req.URL, req.URL.Path, values)
	reportedDeviceType := resolveReportedDeviceType(values, cfg.DeviceType)
	deviceIMEI := resolveDeviceIMEI(values)
	deviceSourceID := deviceIMEI
	if deviceSourceID == "" {
		deviceSourceID = resolveDeviceSourceID(values)
	}
	if deviceSourceID == "" {
		deviceSourceID = "unknown"
	}
	if deviceIMEI == "" {
		deviceIMEI = deviceSourceID
	}

	resolvedUniquePrefix := strings.TrimSpace(cfg.UniquePrefix)
	if resolvedUniquePrefix == "" {
		resolvedUniquePrefix = cfg.ComponyName
	}

	physicalKey := buildPhysicalKey(reportedDeviceType, deviceIMEI)
	logicalKey := buildLogicalKey(physicalKey, eventType)

	deviceID := common.ResolveDeviceUniqueID(
		fmt.Sprintf("%s|%s", resolvedUniquePrefix, physicalKey),
		cfg.ComponyName,
		"aiqiangua_x8",
		reportedDeviceType,
		deviceSourceID,
	)

	uniqueID := common.ResolveDeviceUniqueID(
		fmt.Sprintf("%s|%s", resolvedUniquePrefix, logicalKey),
		cfg.ComponyName,
		"aiqiangua_x8",
		reportedDeviceType,
		deviceSourceID,
	)

	eventTime := parseEventTime(values)

	payloadObj, keep := buildPayload(eventType, values, deviceIMEI, eventTime)
	if !keep {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"code":0,"msg":"ignored"}`))
		log.Printf("[AIQG-X8-%d] ignore unmapped event=%s imei=%s", workerIndex, eventType, deviceSourceID)
		return
	}

	payload, err := json.Marshal(payloadObj)
	if err != nil {
		http.Error(rw, "payload marshal failed", http.StatusInternalServerError)
		return
	}

	data := &base.DeviceData{
		DeviceID:    deviceID,
		UniqueID:    uniqueID,
		DeviceType:  reportedDeviceType,
		DataType:    eventType,
		Timestamp:   chooseTimestamp(eventTime),
		Payload:     payload,
		ComponyName: cfg.ComponyName,
	}

	select {
	case out <- data:
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"code":0,"msg":"ok"}`))
		log.Printf("[AIQG-X8-%d] recv event=%s imei=%s device_id=%s unique=%s", workerIndex, eventType, deviceSourceID, deviceID, uniqueID)
	case <-time.After(3 * time.Second):
		http.Error(rw, "busy", http.StatusServiceUnavailable)
		log.Printf("[AIQG-X8-%d] channel busy, drop event=%s imei=%s", workerIndex, eventType, deviceSourceID)
	}
}

// buildPayload 根据事件类型构建上报 payload。
// 入参：eventType 为事件类型，values 为表单字段，deviceIMEI 为设备 IMEI，eventTime 为事件时间。
// 处理：按事件白名单字段提取值并做必要转换（如 heartrate 数值化、time_begin 回填）。
// 出参：返回 payload 对象与是否保留该事件；不支持或无有效字段时返回 (nil, false)。
func buildPayload(eventType string, values map[string]string, deviceIMEI string, eventTime time.Time) (map[string]interface{}, bool) {
	allowed := map[string][]string{
		"EXERCISE_HEART_RATE": {"average_heartrate", "imei", "max_heartrate", "min_heartrate", "step", "time_begin"},
		"BLOOD_OXYGEN":        {"bloodoxygen", "imei", "time_begin"},
		"BLOOD_PRESSURE":      {"dbp", "imei", "sbp", "time_begin"},
		"SLEEP":               {"awake_time", "deep_sleep", "imei", "light_sleep", "time_begin", "time_end", "total_sleep"},
		"STEP":                {"imei", "step_number_value", "time_begin"},
		"HEART_RATE":          {"heartrate", "imei", "time_begin"},
		"LOCATION":            {"address", "city", "imei", "lat", "lon", "time_begin"},
	}

	fields, ok := allowed[eventType]
	if !ok {
		return nil, false
	}

	payload := make(map[string]interface{}, len(fields))
	for _, key := range fields {
		switch key {
		case "imei":
			if imei := normalizeIdentityPart(deviceIMEI); imei != "" {
				payload["imei"] = imei
			}
		case "time_begin":
			if raw := strings.TrimSpace(values["time_begin"]); raw != "" {
				payload["time_begin"] = raw
			} else if !eventTime.IsZero() {
				payload["time_begin"] = eventTime.Format("2006-01-02 15:04:05")
			}
		case "heartrate":
			raw := strings.TrimSpace(values["heartrate"])
			if raw == "" {
				continue
			}
			if v, err := strconv.Atoi(raw); err == nil {
				payload["heartrate"] = v
			} else {
				payload["heartrate"] = raw
			}
		default:
			if raw := strings.TrimSpace(values[key]); raw != "" {
				payload[key] = raw
			}
		}
	}

	if len(payload) == 0 {
		return nil, false
	}
	return payload, true
}

// parseForm 解析请求表单数据。
// 入参：req 为 HTTP 请求。
// 处理：根据 Content-Type 选择 ParseMultipartForm 或 ParseForm，并将最终表单整理为 key->最后一个值。
// 出参：返回解析后的字段映射；解析失败返回 error。
func parseForm(req *http.Request) (map[string]string, error) {
	ct := strings.ToLower(req.Header.Get("Content-Type"))
	if strings.Contains(ct, "multipart/form-data") {
		if err := req.ParseMultipartForm(8 << 20); err != nil {
			return nil, err
		}
	} else {
		if err := req.ParseForm(); err != nil {
			return nil, err
		}
	}

	result := make(map[string]string)
	for k, vals := range req.Form {
		if len(vals) == 0 {
			continue
		}
		result[k] = vals[len(vals)-1]
	}
	return result, nil
}

// resolveDeviceSourceID 提取设备源标识。
// 入参：values 为表单字段。
// 处理：按 imei/deviceid/device_id/devID/devId 优先级依次查找首个非空值。
// 出参：返回设备源标识；未找到返回空字符串。
func resolveDeviceSourceID(values map[string]string) string {
	keys := []string{"imei", "deviceid", "device_id", "devID", "devId"}
	for _, key := range keys {
		if v := strings.TrimSpace(values[key]); v != "" {
			return v
		}
	}
	return ""
}

// normalizeStepNumberField 统一步数字段命名。
// 入参：values 为表单字段。
// 处理：在 step_number_value、stepNumber、value 三个字段之间做归一化同步，并移除 value 冗余字段。
// 出参：无（就地修改 values）。
func normalizeStepNumberField(values map[string]string) {
	if values == nil {
		return
	}

	if v := strings.TrimSpace(values["step_number_value"]); v != "" {
		values["step_number_value"] = v
		if raw := strings.TrimSpace(values["stepNumber"]); raw == "" {
			values["stepNumber"] = v
		}
		delete(values, "value")
		return
	}

	if v := strings.TrimSpace(values["stepNumber"]); v != "" {
		values["stepNumber"] = v
		values["step_number_value"] = v
		delete(values, "value")
		return
	}

	if v := strings.TrimSpace(values["value"]); v != "" {
		values["step_number_value"] = v
		values["stepNumber"] = v
		delete(values, "value")
	}
}

// resolveDeviceIMEI 提取并规范设备 IMEI。
// 入参：values 为表单字段。
// 处理：按候选键顺序读取并做标识规范化（去空格、替换非法分隔符）。
// 出参：返回有效 IMEI；未找到返回空字符串。
func resolveDeviceIMEI(values map[string]string) string {
	for _, key := range []string{"imei", "deviceid", "device_id", "devID", "devId"} {
		if value := normalizeIdentityPart(values[key]); value != "" {
			return value
		}
	}
	return ""
}

// resolveReportedDeviceType 解析上报设备类型。
// 入参：values 为表单字段，fallback 为兜底设备类型。
// 处理：优先从请求字段读取设备类型并标准化；缺失时使用 fallback。
// 出参：返回标准化后的设备类型字符串。
func resolveReportedDeviceType(values map[string]string, fallback string) string {
	for _, key := range []string{"device_type", "devicetype", "model", "type_name"} {
		if value := strings.TrimSpace(values[key]); value != "" {
			return normalizeDataType(value)
		}
	}
	return normalizeDataType(fallback)
}

// normalizeIdentityPart 规范设备标识片段。
// 入参：value 为原始标识。
// 处理：去除首尾空格，并将竖线替换为下划线以避免与内部分隔符冲突。
// 出参：返回规范化标识；空值返回空字符串。
func normalizeIdentityPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "|", "_")
	return value
}

// buildPhysicalKey 构建设备物理唯一键。
// 入参：deviceType 为设备类型，imei 为设备 IMEI。
// 处理：标准化设备类型与 IMEI，IMEI 为空时使用 unknown，再按 type|imei 拼接。
// 出参：返回物理唯一键字符串。
func buildPhysicalKey(deviceType, imei string) string {
	resolvedType := normalizeDataType(deviceType)
	resolvedIMEI := normalizeIdentityPart(imei)
	if resolvedIMEI == "" {
		resolvedIMEI = "unknown"
	}
	return fmt.Sprintf("%s|%s", resolvedType, resolvedIMEI)
}

// buildLogicalKey 构建事件维度逻辑键。
// 入参：physicalKey 为物理键，eventType 为事件类型。
// 处理：规范化 physicalKey 与 eventType 后拼接。
// 出参：返回 logicalKey 字符串。
// Author ahzhol
func buildLogicalKey(physicalKey, eventType string) string {
	return fmt.Sprintf("%s|%s", normalizeIdentityPart(physicalKey), normalizeDataType(eventType))
}

// detectEventType 识别事件类型。
// 入参：u 为请求 URL，path 为请求路径，form 为表单字段。
// 处理：依次按 URL 参数、表单字段、路径后缀和字段特征进行判定，无法命中则返回 UNKNOWN。
// 出参：返回标准化事件类型。
func detectEventType(u *url.URL, path string, form map[string]string) string {
	if v := strings.TrimSpace(u.Query().Get("event")); v != "" {
		return normalizeDataType(v)
	}
	if v := strings.TrimSpace(u.Query().Get("type_name")); v != "" {
		return normalizeDataType(v)
	}

	for _, key := range []string{"event", "type_name", "event_type", "data_type"} {
		if v := strings.TrimSpace(form[key]); v != "" {
			return normalizeDataType(v)
		}
	}

	lowerPath := strings.ToLower(path)
	pathMap := map[string]string{
		"location":           "LOCATION",
		"sos":                "SOS",
		"heartrate":          "HEART_RATE",
		"step":               "STEP",
		"sleep":              "SLEEP",
		"power":              "POWER",
		"bloodpressure":      "BLOOD_PRESSURE",
		"fall":               "FALL",
		"reply":              "ALERT_REPLY",
		"bloodoxygen":        "BLOOD_OXYGEN",
		"exercise_heartrate": "EXERCISE_HEART_RATE",
		"notify":             "NOTIFY",
	}
	for suffix, name := range pathMap {
		if strings.HasSuffix(lowerPath, "/"+suffix) {
			return name
		}
	}

	if _, ok := form["reply_type"]; ok {
		return "ALERT_REPLY"
	}
	if _, ok := form["sos"]; ok {
		return "SOS"
	}
	if _, ok := form["bloodoxygen"]; ok {
		return "BLOOD_OXYGEN"
	}
	if _, ok := form["dbp"]; ok {
		return "BLOOD_PRESSURE"
	}
	if _, ok := form["remaining_power"]; ok {
		return "POWER"
	}
	if _, ok := form["awake_time"]; ok {
		_, hasLon := form["lon"]
		_, hasLat := form["lat"]
		_, hasAddress := form["address"]
		if hasLon && hasLat && hasAddress {
			return "FALL"
		}
	}
	if _, ok := form["deep_sleep"]; ok {
		return "SLEEP"
	}
	if _, ok := form["step_number_value"]; ok {
		return "STEP"
	}
	if _, ok := form["stepNumber"]; ok {
		return "STEP"
	}
	if _, ok := form["max_heartrate"]; ok {
		return "EXERCISE_HEART_RATE"
	}
	if _, ok := form["heartrate"]; ok {
		_, hasCity := form["city"]
		_, hasAddress := form["address"]
		_, hasLon := form["lon"]
		_, hasLat := form["lat"]
		_, hasHighThreshold := form["theshold_heartrate_h"]
		_, hasLowThreshold := form["theshold_heartrate_l"]
		if hasCity && hasAddress && hasLon && hasLat && !hasHighThreshold && !hasLowThreshold {
			return "SOS"
		}
		return "HEART_RATE"
	}
	if _, ok := form["is_track"]; ok {
		return "LOCATION"
	}
	if _, ok := form["lon"]; ok {
		if _, hasLat := form["lat"]; hasLat {
			return "LOCATION"
		}
	}
	if _, ok := form["deviceid"]; ok {
		if _, hasType := form["type"]; hasType {
			return "NOTIFY"
		}
	}

	return "UNKNOWN"
}

// parseEventTime 解析事件发生时间。
// 入参：form 为表单字段。
// 处理：优先解析 time_begin_str，再尝试 time_begin/time_end/timestamp 的多种时间格式与秒/毫秒时间戳。
// 出参：成功返回时间对象；失败返回零值时间。
func parseEventTime(form map[string]string) time.Time {
	if v := strings.TrimSpace(form["time_begin_str"]); v != "" {
		if t, err := time.ParseInLocation("20060102150405", v, time.Local); err == nil {
			return t
		}
	}

	candidates := []string{"time_begin", "time_end", "timestamp"}
	for _, key := range candidates {
		raw := strings.TrimSpace(form[key])
		if raw == "" {
			continue
		}

		layouts := []string{
			time.RFC3339,
			"2006-01-02 15:04:05",
			"2006/01/02 15:04:05",
		}
		for _, layout := range layouts {
			if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
				return t
			}
		}

		if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if sec > 1000000000000 {
				return time.UnixMilli(sec)
			}
			if sec > 0 {
				return time.Unix(sec, 0)
			}
		}
	}
	return time.Time{}
}

// chooseTimestamp 选择最终写入时间戳。
// 入参：eventTime 为解析出的事件时间。
// 处理：若 eventTime 为零值则回退到当前时间。
// 出参：返回最终时间戳。
func chooseTimestamp(eventTime time.Time) time.Time {
	if eventTime.IsZero() {
		return time.Now()
	}
	return eventTime
}

// normalizePath 规范化 HTTP 路径。
// 入参：path 为原始路径。
// 处理：去空格、补前导斜杠、移除尾部斜杠，并保证至少返回根路径。
// 出参：返回规范化路径。
func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	return path
}

// normalizeDataType 规范化数据类型名称。
// 入参：name 为原始名称。
// 处理：转大写、去空格并将连字符/空格替换为下划线。
// 出参：返回标准化类型名；空值返回 UNKNOWN。
func normalizeDataType(name string) string {
	name = strings.TrimSpace(strings.ToUpper(name))
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, " ", "_")
	if name == "" {
		return "UNKNOWN"
	}
	return name
}

// clientIP 提取客户端真实 IP。
// 入参：req 为 HTTP 请求。
// 处理：优先读取 X-Forwarded-For 首个地址，其次 X-Real-IP，最后从 RemoteAddr 拆分。
// 出参：返回解析出的客户端 IP 字符串。
func clientIP(req *http.Request) string {
	if xff := strings.TrimSpace(req.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	if xrip := strings.TrimSpace(req.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(req.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(req.RemoteAddr)
}

// ipAllowed 判断客户端 IP 是否在白名单中。
// 入参：ip 为客户端地址，allowed 为白名单列表（支持精确 IP 与 CIDR）。
// 处理：先做精确匹配，再尝试按 CIDR 网段匹配。
// 出参：命中白名单返回 true，否则返回 false。
func ipAllowed(ip string, allowed []string) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}
	for _, item := range allowed {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == ip {
			return true
		}
		if strings.Contains(item, "/") {
			if _, cidr, err := net.ParseCIDR(item); err == nil {
				if parsedIP := net.ParseIP(ip); parsedIP != nil && cidr.Contains(parsedIP) {
					return true
				}
			}
		}
	}
	return false
}

// init 注册 aiqiangua_x8 插件工厂。
// 入参：无。
// 处理：在包初始化阶段向插件注册表登记创建函数。
// 出参：无。
// @Author ahzhol
func init() {
	plugin.Register("aiqiangua_x8", func() base.IWorker {
		return &Worker{}
	})
}
