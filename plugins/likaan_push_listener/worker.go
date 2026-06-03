package likaan_push_listener

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/plugin"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ConfigItem struct {
	ListenAddr   string `json:"listen_addr"`
	ReceivePath  string `json:"receive_path"`
	ComponyName  string `json:"compony_name"`
	DeviceType   string `json:"device_type"`
	AuthToken    string `json:"auth_token"`
	AuthHeader   string `json:"auth_header"`
	RequestLimit int64  `json:"request_limit_bytes"`
}

type Worker struct {
	configs    []ConfigItem
	server     *http.Server
	listenAddr string
}

type pushPayload struct {
	DeviceMsgID interface{} `json:"deviceMsgId"`
	DevInfo     struct {
		DevID string `json:"devId"`
	} `json:"devInfo"`
	IotMsg map[string]interface{} `json:"iotMsg"`
}

func (w *Worker) Init(configs []json.RawMessage) error {
	if len(configs) == 0 {
		return fmt.Errorf("empty configs")
	}

	receivePathIndex := make(map[string]int)

	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d] parse failed: %w", i, err)
		}

		if strings.TrimSpace(cfg.ListenAddr) == "" {
			cfg.ListenAddr = ":8086"
		}
		if strings.TrimSpace(cfg.ReceivePath) == "" {
			return fmt.Errorf("config[%d] receive_path is required", i)
		}
		cfg.ReceivePath = normalizePath(cfg.ReceivePath)

		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "LIKAAN"
		}
		if strings.TrimSpace(cfg.AuthHeader) == "" {
			cfg.AuthHeader = "X-Likaan-Token"
		}
		if cfg.RequestLimit <= 0 {
			cfg.RequestLimit = 1 << 20
		}

		if prev, exists := receivePathIndex[cfg.ReceivePath]; exists {
			return fmt.Errorf("config[%d] receive_path %q conflicts with config[%d]", i, cfg.ReceivePath, prev)
		}
		receivePathIndex[cfg.ReceivePath] = i

		if w.listenAddr == "" {
			w.listenAddr = cfg.ListenAddr
		} else if w.listenAddr != cfg.ListenAddr {
			return fmt.Errorf("all configs must use the same listen_addr, got %s and %s", w.listenAddr, cfg.ListenAddr)
		}

		w.configs = append(w.configs, cfg)
	}

	return nil
}

func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	receiveHandlers := make(map[string]http.HandlerFunc, len(w.configs))

	for i := range w.configs {
		cfg := w.configs[i]
		receiveHandlers[cfg.ReceivePath] = w.buildHandler(cfg, out)
		log.Printf("[LIKAAN] route mounted path=%s listen=%s", cfg.ReceivePath, w.listenAddr)
	}

	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		path := normalizePath(req.URL.Path)

		if recvHandler, ok := receiveHandlers[path]; ok {
			recvHandler(rw, req)
			return
		}

		log.Printf("[LIKAAN] unmatched route method=%s path=%s remote=%s", req.Method, req.URL.Path, req.RemoteAddr)
		http.NotFound(rw, req)
	})

	w.server = &http.Server{
		Addr:    w.listenAddr,
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.server.Shutdown(shutdownCtx)
	}()

	log.Printf("[LIKAAN] listening at %s", w.listenAddr)
	if err := w.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[LIKAAN] server exited with error: %v", err)
	}
}

func (w *Worker) buildHandler(cfg ConfigItem, out chan<- *base.DeviceData) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !matchAuth(req, cfg.AuthHeader, cfg.AuthToken) {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(io.LimitReader(req.Body, cfg.RequestLimit))
		if err != nil {
			http.Error(rw, "read body failed", http.StatusBadRequest)
			return
		}
		_ = req.Body.Close()

		var incoming pushPayload
		if err := json.Unmarshal(body, &incoming); err != nil {
			http.Error(rw, "invalid json body", http.StatusBadRequest)
			return
		}

		devID := strings.TrimSpace(incoming.DevInfo.DevID)
		if devID == "" {
			devID = strings.TrimSpace(getStringValue(incoming.IotMsg["devId"]))
		}
		if devID == "" {
			http.Error(rw, "devInfo.devId or iotMsg.devId is required", http.StatusBadRequest)
			return
		}

		if devID == "866960079840045" {
			log.Printf("[LIKAAN] raw push devId=%s body=%s", devID, string(body))
		}

		msgType := normalizeDataType(incoming.IotMsg["msgType"])
		if msgType == "" {
			msgType = normalizeDataType(incoming.DeviceMsgID)
		}
		if msgType == "" {
			msgType = "LIKAAN_PUSH"
		}

		ts := time.Now()
		if createTimeMillis := extractCreateTimeMillis(incoming.IotMsg); createTimeMillis > 0 {
			if candidate := time.UnixMilli(createTimeMillis); !candidate.IsZero() {
				ts = candidate
			}
		}

		filteredPayload, err := buildForwardIotMsgPayload(incoming.IotMsg, devID, msgType)
		if err != nil {
			http.Error(rw, "build payload failed", http.StatusBadRequest)
			return
		}

		deviceData := &base.DeviceData{
			DeviceID:    devID,
			UniqueID:    devID,
			DeviceType:  strings.TrimSpace(cfg.DeviceType),
			Timestamp:   ts,
			Payload:     filteredPayload,
			ComponyName: strings.TrimSpace(cfg.ComponyName),
			DataType:    msgType,
		}

		select {
		case out <- deviceData:
			rw.Header().Set("Content-Type", "application/json; charset=utf-8")
			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"code":0,"message":"ok"}`))
			log.Printf("[LIKAAN] recv path=%s devId=%s type=%s", cfg.ReceivePath, devID, msgType)
		case <-time.After(3 * time.Second):
			http.Error(rw, "busy", http.StatusServiceUnavailable)
			log.Printf("[LIKAAN] channel busy, drop path=%s devId=%s", cfg.ReceivePath, devID)
		}
	}
}

func buildForwardIotMsgPayload(iotMsg map[string]interface{}, fallbackDevID string, normalizedMsgType string) (json.RawMessage, error) {
	if iotMsg == nil {
		return nil, fmt.Errorf("iotMsg is required")
	}

	forward := make(map[string]interface{}, len(iotMsg)+2)
	for key, value := range iotMsg {
		forward[key] = value
	}

	if strings.TrimSpace(getStringValue(forward["devId"])) == "" {
		forward["devId"] = fallbackDevID
	}
	if normalizeDataType(forward["msgType"]) == "" {
		forward["msgType"] = normalizedMsgType
	}

	data, err := json.Marshal(forward)
	if err != nil {
		return nil, err
	}

	return json.RawMessage(data), nil
}

func getStringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

func extractCreateTimeMillis(iotMsg map[string]interface{}) int64 {
	if iotMsg == nil {
		return 0
	}

	value, ok := iotMsg["createTime"]
	if !ok || value == nil {
		return 0
	}

	switch v := value.(type) {
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func matchAuth(req *http.Request, headerName string, expectedToken string) bool {
	expectedToken = strings.TrimSpace(expectedToken)
	if expectedToken == "" {
		return true
	}
	got := strings.TrimSpace(req.Header.Get(headerName))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedToken)) == 1
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
		if path == "" {
			path = "/"
		}
	}
	return path
}

func normalizeDataType(value interface{}) string {
	if value == nil {
		return ""
	}
	trimmed := strings.TrimSpace(fmt.Sprintf("%v", value))
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, "-", "_")
	trimmed = strings.ReplaceAll(trimmed, " ", "_")
	return strings.ToUpper(trimmed)
}

func init() {
	plugin.Register("likaan_push_listener", func() base.IWorker {
		return &Worker{}
	})
}
