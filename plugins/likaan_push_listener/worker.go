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
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ConfigItem struct {
	ListenAddr          string `json:"listen_addr"`
	ReceivePath         string `json:"receive_path"`
	WebSocketPath       string `json:"websocket_path"`
	ComponyName         string `json:"compony_name"`
	DeviceType          string `json:"device_type"`
	AuthToken           string `json:"auth_token"`
	AuthHeader          string `json:"auth_header"`
	WebSocketAuthToken  string `json:"websocket_auth_token"`
	WebSocketAuthHeader string `json:"websocket_auth_header"`
	RequestLimit        int64  `json:"request_limit_bytes"`
}

type Worker struct {
	configs    []ConfigItem
	server     *http.Server
	listenAddr string
	clientsMu  sync.RWMutex
	clients    map[*websocket.Conn]struct{}
	upgrader   websocket.Upgrader
}

type pushPayload struct {
	DeviceMsgID interface{} `json:"deviceMsgId"`
	DevInfo     struct {
		DevID string `json:"devId"`
	} `json:"devInfo"`
	IotMsg struct {
		CreateTime int64       `json:"createTime"`
		DevID      string      `json:"devId"`
		MsgType    interface{} `json:"msgType"`
		MsgDesc    string      `json:"msgDesc"`
	} `json:"iotMsg"`
}

func (w *Worker) Init(configs []json.RawMessage) error {
	w.clients = make(map[*websocket.Conn]struct{})
	w.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	if len(configs) == 0 {
		return fmt.Errorf("empty configs")
	}

	receivePathIndex := make(map[string]int)
	websocketConfigIndex := make(map[string]int)

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
		if strings.TrimSpace(cfg.WebSocketPath) == "" {
			cfg.WebSocketPath = "/ws/likaan/alarm"
		}
		cfg.WebSocketPath = normalizePath(cfg.WebSocketPath)

		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "LIKAAN"
		}
		if strings.TrimSpace(cfg.AuthHeader) == "" {
			cfg.AuthHeader = "X-Likaan-Token"
		}
		if strings.TrimSpace(cfg.WebSocketAuthHeader) == "" {
			cfg.WebSocketAuthHeader = cfg.AuthHeader
		}
		if strings.TrimSpace(cfg.WebSocketAuthToken) == "" {
			cfg.WebSocketAuthToken = cfg.AuthToken
		}
		if cfg.RequestLimit <= 0 {
			cfg.RequestLimit = 1 << 20
		}

		if prev, exists := receivePathIndex[cfg.ReceivePath]; exists {
			return fmt.Errorf("config[%d] receive_path %q conflicts with config[%d]", i, cfg.ReceivePath, prev)
		}
		receivePathIndex[cfg.ReceivePath] = i

		if prev, exists := websocketConfigIndex[cfg.WebSocketPath]; exists {
			prevCfg := w.configs[prev]
			if err := validateSharedWebSocketConfig(prevCfg, cfg, prev, i); err != nil {
				return err
			}
		} else {
			websocketConfigIndex[cfg.WebSocketPath] = i
		}

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
	websocketHandlers := make(map[string]http.HandlerFunc)

	for i := range w.configs {
		cfg := w.configs[i]
		receiveHandlers[cfg.ReceivePath] = w.buildHandler(cfg, out)
		log.Printf("[LIKAAN] route mounted path=%s listen=%s", cfg.ReceivePath, w.listenAddr)

		if _, exists := websocketHandlers[cfg.WebSocketPath]; !exists {
			websocketHandlers[cfg.WebSocketPath] = w.buildWebSocketHandler(cfg)
			log.Printf("[LIKAAN] websocket mounted path=%s listen=%s", cfg.WebSocketPath, w.listenAddr)
		}
	}

	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		path := normalizePath(req.URL.Path)

		if wsHandler, ok := websocketHandlers[path]; ok {
			wsHandler(rw, req)
			return
		}

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
	w.closeAllClients()
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
			devID = strings.TrimSpace(incoming.IotMsg.DevID)
		}
		if devID == "" {
			http.Error(rw, "devInfo.devId or iotMsg.devId is required", http.StatusBadRequest)
			return
		}

		msgType := normalizeDataType(incoming.IotMsg.MsgType)
		if msgType == "" {
			msgType = normalizeDataType(incoming.DeviceMsgID)
		}
		if msgType == "" {
			msgType = "LIKAAN_PUSH"
		}

		ts := time.Now()
		if incoming.IotMsg.CreateTime > 0 {
			if candidate := time.UnixMilli(incoming.IotMsg.CreateTime); !candidate.IsZero() {
				ts = candidate
			}
		}

		filteredPayload, err := buildFilteredIotMsgPayload(incoming, devID, msgType)
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
			w.broadcastAlarm(cfg, deviceData, filteredPayload)
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

func buildFilteredIotMsgPayload(incoming pushPayload, fallbackDevID string, normalizedMsgType string) (json.RawMessage, error) {
	devID := strings.TrimSpace(incoming.IotMsg.DevID)
	if devID == "" {
		devID = fallbackDevID
	}

	msgDesc := strings.TrimSpace(incoming.IotMsg.MsgDesc)
	msgType := incoming.IotMsg.MsgType
	if normalizeDataType(msgType) == "" {
		msgType = normalizedMsgType
	}

	filtered := struct {
		CreateTime int64       `json:"createTime"`
		DevID      string      `json:"devId"`
		MsgType    interface{} `json:"msgType"`
		MsgDesc    string      `json:"msgDesc"`
	}{}

	filtered.CreateTime = incoming.IotMsg.CreateTime
	filtered.DevID = devID
	filtered.MsgType = msgType
	filtered.MsgDesc = msgDesc

	data, err := json.Marshal(filtered)
	if err != nil {
		return nil, err
	}

	return json.RawMessage(data), nil
}

func (w *Worker) buildWebSocketHandler(cfg ConfigItem) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !matchAuth(req, cfg.WebSocketAuthHeader, cfg.WebSocketAuthToken) {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}

		conn, err := w.upgrader.Upgrade(rw, req, nil)
		if err != nil {
			log.Printf("[LIKAAN] websocket upgrade failed path=%s remote=%s err=%v", req.URL.Path, req.RemoteAddr, err)
			return
		}

		w.addClient(conn)
		clientTag := strings.TrimSpace(req.Header.Get("X-Iot-Client"))
		if clientTag == "" {
			clientTag = "-"
		}
		log.Printf("[LIKAAN] websocket client connected path=%s remote=%s ua=%q x_iot_client=%q", req.URL.Path, req.RemoteAddr, req.UserAgent(), clientTag)

		go w.keepReadLoop(conn, req.RemoteAddr)
	}
}

func (w *Worker) keepReadLoop(conn *websocket.Conn, remote string) {
	defer func() {
		w.removeClient(conn)
		_ = conn.Close()
		log.Printf("[LIKAAN] websocket client disconnected remote=%s", remote)
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (w *Worker) addClient(conn *websocket.Conn) {
	w.clientsMu.Lock()
	defer w.clientsMu.Unlock()
	w.clients[conn] = struct{}{}
}

func (w *Worker) removeClient(conn *websocket.Conn) {
	w.clientsMu.Lock()
	defer w.clientsMu.Unlock()
	delete(w.clients, conn)
}

func (w *Worker) closeAllClients() {
	w.clientsMu.Lock()
	defer w.clientsMu.Unlock()
	for conn := range w.clients {
		_ = conn.Close()
		delete(w.clients, conn)
	}
}

func (w *Worker) broadcastAlarm(cfg ConfigItem, data *base.DeviceData, rawPayload json.RawMessage) {
	msg := map[string]interface{}{
		"type":         "likaan_alarm",
		"device_id":    data.DeviceID,
		"unique_id":    data.UniqueID,
		"device_type":  data.DeviceType,
		"data_type":    data.DataType,
		"timestamp":    data.Timestamp,
		"compony_name": cfg.ComponyName,
		"payload":      json.RawMessage(rawPayload),
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[LIKAAN] websocket broadcast marshal failed devId=%s err=%v", data.DeviceID, err)
		return
	}

	w.clientsMu.RLock()
	clients := make([]*websocket.Conn, 0, len(w.clients))
	for conn := range w.clients {
		clients = append(clients, conn)
	}
	w.clientsMu.RUnlock()

	if len(clients) == 0 {
		log.Printf("[LIKAAN] websocket broadcast skipped: no clients, devId=%s type=%s", data.DeviceID, data.DataType)
		return
	}

	sent := 0
	failed := 0
	for _, conn := range clients {
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			failed++
			log.Printf("[LIKAAN] websocket write failed devId=%s type=%s err=%v", data.DeviceID, data.DataType, err)
			w.removeClient(conn)
			_ = conn.Close()
			continue
		}
		sent++
	}

	log.Printf("[LIKAAN] websocket broadcast done devId=%s type=%s clients=%d sent=%d failed=%d", data.DeviceID, data.DataType, len(clients), sent, failed)
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

func validateSharedWebSocketConfig(prevCfg ConfigItem, cfg ConfigItem, prevIndex int, currIndex int) error {
	if !strings.EqualFold(strings.TrimSpace(prevCfg.WebSocketAuthHeader), strings.TrimSpace(cfg.WebSocketAuthHeader)) {
		return fmt.Errorf("config[%d] websocket_path %q conflicts with config[%d]: websocket_auth_header mismatch", currIndex, cfg.WebSocketPath, prevIndex)
	}
	if strings.TrimSpace(prevCfg.WebSocketAuthToken) != strings.TrimSpace(cfg.WebSocketAuthToken) {
		return fmt.Errorf("config[%d] websocket_path %q conflicts with config[%d]: websocket_auth_token mismatch", currIndex, cfg.WebSocketPath, prevIndex)
	}
	return nil
}

func init() {
	plugin.Register("likaan_push_listener", func() base.IWorker {
		return &Worker{}
	})
}
