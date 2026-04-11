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

func resolveDeviceSourceID(values map[string]string) string {
	keys := []string{"imei", "deviceid", "device_id", "devID", "devId"}
	for _, key := range keys {
		if v := strings.TrimSpace(values[key]); v != "" {
			return v
		}
	}
	return ""
}

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

func resolveDeviceIMEI(values map[string]string) string {
	for _, key := range []string{"imei", "deviceid", "device_id", "devID", "devId"} {
		if value := normalizeIdentityPart(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func resolveReportedDeviceType(values map[string]string, fallback string) string {
	for _, key := range []string{"device_type", "devicetype", "model", "type_name"} {
		if value := strings.TrimSpace(values[key]); value != "" {
			return normalizeDataType(value)
		}
	}
	return normalizeDataType(fallback)
}

func normalizeIdentityPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "|", "_")
	return value
}

func buildPhysicalKey(deviceType, imei string) string {
	resolvedType := normalizeDataType(deviceType)
	resolvedIMEI := normalizeIdentityPart(imei)
	if resolvedIMEI == "" {
		resolvedIMEI = "unknown"
	}
	return fmt.Sprintf("%s|%s", resolvedType, resolvedIMEI)
}

func buildLogicalKey(physicalKey, eventType string) string {
	return fmt.Sprintf("%s|%s", normalizeIdentityPart(physicalKey), normalizeDataType(eventType))
}

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

func chooseTimestamp(eventTime time.Time) time.Time {
	if eventTime.IsZero() {
		return time.Now()
	}
	return eventTime
}

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

func normalizeDataType(name string) string {
	name = strings.TrimSpace(strings.ToUpper(name))
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, " ", "_")
	if name == "" {
		return "UNKNOWN"
	}
	return name
}

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

func init() {
	plugin.Register("aiqiangua_x8", func() base.IWorker {
		return &Worker{}
	})
}
