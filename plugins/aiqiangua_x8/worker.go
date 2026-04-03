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

	eventType := detectEventType(req.URL, req.URL.Path, values)
	deviceSourceID := resolveDeviceSourceID(values)
	if deviceSourceID == "" {
		deviceSourceID = "unknown"
	}

	resolvedUniquePrefix := strings.TrimSpace(cfg.UniquePrefix)
	if resolvedUniquePrefix == "" {
		resolvedUniquePrefix = fmt.Sprintf("%s|%s", cfg.ComponyName, deviceSourceID)
	}

	uniqueID := common.ResolveDeviceUniqueID(
		resolvedUniquePrefix,
		cfg.ComponyName,
		"aiqiangua_x8",
		cfg.DeviceType,
		deviceSourceID,
	)

	eventTime := parseEventTime(values)

	payloadObj := map[string]interface{}{
		"vendor":      "aiqiangua",
		"model":       "x8",
		"event_type":  eventType,
		"path":        req.URL.Path,
		"query":       req.URL.RawQuery,
		"remote_ip":   remoteIP,
		"content_type": req.Header.Get("Content-Type"),
		"form":        values,
		"raw_body":    string(body),
		"received_at": time.Now().Format(time.RFC3339),
	}
	if !eventTime.IsZero() {
		payloadObj["event_time"] = eventTime.Format(time.RFC3339)
	}

	payload, err := json.Marshal(payloadObj)
	if err != nil {
		http.Error(rw, "payload marshal failed", http.StatusInternalServerError)
		return
	}

	data := &base.DeviceData{
		DeviceID:    uniqueID,
		UniqueID:    uniqueID,
		DeviceType:  cfg.DeviceType,
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
		log.Printf("[AIQG-X8-%d] recv event=%s imei=%s unique=%s", workerIndex, eventType, deviceSourceID, uniqueID)
	case <-time.After(3 * time.Second):
		http.Error(rw, "busy", http.StatusServiceUnavailable)
		log.Printf("[AIQG-X8-%d] channel busy, drop event=%s imei=%s", workerIndex, eventType, deviceSourceID)
	}
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

func detectEventType(u *url.URL, path string, form map[string]string) string {
	if v := strings.TrimSpace(u.Query().Get("event")); v != "" {
		return normalizeDataType(v)
	}
	if v := strings.TrimSpace(u.Query().Get("type_name")); v != "" {
		return normalizeDataType(v)
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
	if _, ok := form["bloodoxygen"]; ok {
		return "BLOOD_OXYGEN"
	}
	if _, ok := form["dbp"]; ok {
		return "BLOOD_PRESSURE"
	}
	if _, ok := form["remaining_power"]; ok {
		return "POWER"
	}
	if _, ok := form["deep_sleep"]; ok {
		return "SLEEP"
	}
	if _, ok := form["value"]; ok {
		return "STEP"
	}
	if _, ok := form["max_heartrate"]; ok {
		return "EXERCISE_HEART_RATE"
	}
	if _, ok := form["heartrate"]; ok {
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
