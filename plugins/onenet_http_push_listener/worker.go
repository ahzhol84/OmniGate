package onenet_http_push_listener

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/plugin"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type ConfigItem struct {
	ListenAddr         string `json:"listen_addr"`
	ReceivePath        string `json:"receive_path"`
	ComponyName        string `json:"compony_name"`
	DeviceType         string `json:"device_type"`
	DataType           string `json:"data_type"`
	VerifyToken        string `json:"verify_token"`
	EnableTokenVerify  bool   `json:"enable_token_verify"`
	AuthHeader         string `json:"auth_header"`
	AuthToken          string `json:"auth_token"`
	RequestLimit       int64  `json:"request_limit_bytes"`
	ResponseOnVerified string `json:"response_on_verified"`
}

type Worker struct {
	configs    []ConfigItem
	server     *http.Server
	listenAddr string
}

func (w *Worker) Init(configs []json.RawMessage) error {
	if len(configs) == 0 {
		return fmt.Errorf("empty configs")
	}

	pathIndex := make(map[string]int)

	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d] parse failed: %w", i, err)
		}

		if strings.TrimSpace(cfg.ListenAddr) == "" {
			cfg.ListenAddr = ":8087"
		}
		if strings.TrimSpace(cfg.ReceivePath) == "" {
			cfg.ReceivePath = "/push/onenet"
		}
		cfg.ReceivePath = normalizePath(cfg.ReceivePath)

		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "ONENET"
		}
		if strings.TrimSpace(cfg.DataType) == "" {
			cfg.DataType = "ONENET_PUSH"
		}
		if strings.TrimSpace(cfg.AuthHeader) == "" {
			cfg.AuthHeader = "X-OneNET-Token"
		}
		if cfg.RequestLimit <= 0 {
			cfg.RequestLimit = 2 << 20
		}

		if prev, exists := pathIndex[cfg.ReceivePath]; exists {
			return fmt.Errorf("config[%d] receive_path %q conflicts with config[%d]", i, cfg.ReceivePath, prev)
		}
		pathIndex[cfg.ReceivePath] = i

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
	handlers := make(map[string]http.HandlerFunc, len(w.configs))
	for i := range w.configs {
		cfg := w.configs[i]
		handlers[cfg.ReceivePath] = w.buildHandler(cfg, out)
		log.Printf("[ONENET] route mounted path=%s listen=%s", cfg.ReceivePath, w.listenAddr)
	}

	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		path := normalizePath(req.URL.Path)
		if h, ok := handlers[path]; ok {
			h(rw, req)
			return
		}
		log.Printf("[ONENET] unmatched route method=%s path=%s remote=%s", req.Method, req.URL.Path, req.RemoteAddr)
		http.NotFound(rw, req)
	})

	w.server = &http.Server{Addr: w.listenAddr, Handler: handler}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.server.Shutdown(shutdownCtx)
	}()

	log.Printf("[ONENET] listening at %s", w.listenAddr)
	if err := w.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[ONENET] server exited with error: %v", err)
	}
}

func (w *Worker) buildHandler(cfg ConfigItem, out chan<- *base.DeviceData) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			w.handleURLVerify(rw, req, cfg)
		case http.MethodPost:
			w.handlePushData(rw, req, cfg, out)
		default:
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func (w *Worker) handleURLVerify(rw http.ResponseWriter, req *http.Request, cfg ConfigItem) {
	if !matchAuth(req, cfg.AuthHeader, cfg.AuthToken) {
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}

	msg := strings.TrimSpace(req.URL.Query().Get("msg"))
	nonce := strings.TrimSpace(req.URL.Query().Get("nonce"))
	signature := strings.TrimSpace(req.URL.Query().Get("signature"))

	if cfg.EnableTokenVerify {
		token := strings.TrimSpace(cfg.VerifyToken)
		if token == "" {
			http.Error(rw, "verify_token required when enable_token_verify=true", http.StatusBadRequest)
			return
		}
		if msg == "" || nonce == "" || signature == "" {
			http.Error(rw, "missing msg/nonce/signature", http.StatusBadRequest)
			return
		}
		if !verifyOneNETSignature(token, nonce, msg, signature) {
			http.Error(rw, "signature mismatch", http.StatusUnauthorized)
			return
		}
	}

	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	if msg != "" {
		_, _ = rw.Write([]byte(msg))
	} else if strings.TrimSpace(cfg.ResponseOnVerified) != "" {
		_, _ = rw.Write([]byte(strings.TrimSpace(cfg.ResponseOnVerified)))
	} else {
		_, _ = rw.Write([]byte("ok"))
	}

	log.Printf("[ONENET] url verify path=%s remote=%s token_verify=%t success", cfg.ReceivePath, req.RemoteAddr, cfg.EnableTokenVerify)
}

func (w *Worker) handlePushData(rw http.ResponseWriter, req *http.Request, cfg ConfigItem, out chan<- *base.DeviceData) {
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

	trimmedBody := strings.TrimSpace(string(body))
	if trimmedBody == "" {
		http.Error(rw, "empty body", http.StatusBadRequest)
		return
	}

	var payloadObj map[string]interface{}
	if err := json.Unmarshal(body, &payloadObj); err != nil {
		http.Error(rw, "invalid json body", http.StatusBadRequest)
		return
	}

	prettyBody, _ := json.MarshalIndent(payloadObj, "", "  ")
	log.Printf("[ONENET] recv path=%s remote=%s payload=\n%s", cfg.ReceivePath, req.RemoteAddr, string(prettyBody))

	innerMsgObj, hasInner := parseInnerMsg(payloadObj)
	if hasInner {
		prettyInner, _ := json.MarshalIndent(innerMsgObj, "", "  ")
		log.Printf("[ONENET] parsed inner msg path=%s remote=%s msg=\n%s", cfg.ReceivePath, req.RemoteAddr, string(prettyInner))
	}

	deviceID := pickFirstNonEmpty(payloadObj, "device_id", "deviceId", "devId", "imei", "sn")
	if deviceID == "" && hasInner {
		deviceID = pickFirstNonEmpty(innerMsgObj, "deviceName", "device_id", "deviceId", "devId", "imei", "sn")
	}
	if deviceID == "" {
		deviceID = "UNKNOWN_ONENET_DEVICE"
	}

	dataType := strings.TrimSpace(cfg.DataType)
	if dataType == "" {
		dataType = "ONENET_PUSH"
	}

	deviceData := &base.DeviceData{
		DeviceID:    deviceID,
		UniqueID:    deviceID,
		DeviceType:  strings.TrimSpace(cfg.DeviceType),
		Timestamp:   time.Now(),
		Payload:     json.RawMessage(body),
		ComponyName: strings.TrimSpace(cfg.ComponyName),
		DataType:    dataType,
	}

	select {
	case out <- deviceData:
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"code":0,"message":"ok"}`))
	case <-time.After(2 * time.Second):
		http.Error(rw, "busy", http.StatusServiceUnavailable)
		log.Printf("[ONENET] channel busy, drop path=%s devId=%s", cfg.ReceivePath, deviceID)
	}
}

func verifyOneNETSignature(token, nonce, msg, signature string) bool {
	base64Digest := calcOneNETDigest(token, nonce, msg)
	if subtle.ConstantTimeCompare([]byte(base64Digest), []byte(signature)) == 1 {
		return true
	}
	decodedSig, err := url.QueryUnescape(signature)
	if err == nil && subtle.ConstantTimeCompare([]byte(base64Digest), []byte(decodedSig)) == 1 {
		return true
	}
	decodedDigest, err := url.QueryUnescape(base64Digest)
	if err == nil && subtle.ConstantTimeCompare([]byte(decodedDigest), []byte(signature)) == 1 {
		return true
	}
	return false
}

func calcOneNETDigest(token, nonce, msg string) string {
	parts := []string{token, nonce, msg}
	sort.Strings(parts)
	raw := strings.Join(parts, "")
	sum := md5.Sum([]byte(raw))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func pickFirstNonEmpty(payload map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := payload[key]; ok {
			trimmed := strings.TrimSpace(fmt.Sprintf("%v", v))
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func parseInnerMsg(payload map[string]interface{}) (map[string]interface{}, bool) {
	rawMsg, ok := payload["msg"]
	if !ok || rawMsg == nil {
		return nil, false
	}

	switch typed := rawMsg.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil, false
		}
		var inner map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &inner); err != nil {
			return nil, false
		}
		return inner, true
	case map[string]interface{}:
		return typed, true
	default:
		return nil, false
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

func init() {
	plugin.Register("onenet_http_push_listener", func() base.IWorker {
		return &Worker{}
	})
}
