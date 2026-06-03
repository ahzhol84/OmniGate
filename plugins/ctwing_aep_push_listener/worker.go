// Package ctwing_aep_push_listener 实现电信 CTWing AEP 平台设备数据订阅推送接收。
//
// 推送层级结构（三层嵌套 JSON）：
//
// 外层：{notifyType, requestId, deviceId, gatewayId, topic, payload(JSON字符串)}
// 中层 payload：{serviceId, serviceData(JSON字符串)}
// 内层 serviceData：实际设备字段，如 voltagevalue / alarmcode 等
//
// 仅处理 topic == "v1/up/ad19prof" 的推送，其余静默丢弃（回 200）。
package ctwing_aep_push_listener

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
	"time"
)

// ──────────────────────────────────────────────
// 配置
// ──────────────────────────────────────────────

// ConfigItem 单条路由配置。
type ConfigItem struct {
	ListenAddr   string `json:"listen_addr"`
	ReceivePath  string `json:"receive_path"`
	ComponyName  string `json:"compony_name"`
	DeviceType   string `json:"device_type"`
	AuthHeader   string `json:"auth_header"`
	AuthToken    string `json:"auth_token"`
	RequestLimit int64  `json:"request_limit_bytes"`
	// AllowedTopic 若非空，则只接收该 topic；默认过滤 v1/up/ad19prof
	AllowedTopic string `json:"allowed_topic"`
}

// ──────────────────────────────────────────────
// Worker
// ──────────────────────────────────────────────

// Worker 实现 base.IWorker 接口。
type Worker struct {
	configs    []ConfigItem
	server     *http.Server
	listenAddr string
}

func init() {
	plugin.Register("ctwing_aep_push_listener", func() base.IWorker { return &Worker{} })
}

// ──────────────────────────────────────────────
// CTWing 推送格式定义
// ──────────────────────────────────────────────

// outerPush 是 CTWing AEP 订阅推送的外层结构。
type outerPush struct {
	NotifyType string `json:"notifyType"` // e.g. "deviceDataChanged"
	RequestID  string `json:"requestId"`
	DeviceID   string `json:"deviceId"` // 设备 deviceId（AEP 平台侧）
	GatewayID  string `json:"gatewayId"`
	Topic      string `json:"topic"`   // 消息 topic，过滤用
	Payload    string `json:"payload"` // JSON 字符串，中层内容
}

// servicePayload 是 payload 字符串反序列化后的中层结构。
type servicePayload struct {
	ServiceID   string `json:"serviceId"`
	ServiceData string `json:"serviceData"` // JSON 字符串，内层设备属性
}

// ──────────────────────────────────────────────
// Init
// ──────────────────────────────────────────────

// Init 解析并校验插件配置。
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

		// 默认值
		if strings.TrimSpace(cfg.ListenAddr) == "" {
			cfg.ListenAddr = ":8089"
		}
		if strings.TrimSpace(cfg.ReceivePath) == "" {
			cfg.ReceivePath = "/push/ctwing"
		}
		cfg.ReceivePath = normalizePath(cfg.ReceivePath)

		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "CTWING"
		}
		if strings.TrimSpace(cfg.AuthHeader) == "" {
			cfg.AuthHeader = "Authorization"
		}
		if cfg.RequestLimit <= 0 {
			cfg.RequestLimit = 2 << 20 // 2 MB
		}
		if strings.TrimSpace(cfg.AllowedTopic) == "" {
			cfg.AllowedTopic = "v1/up/ad19prof"
		}

		// 路径冲突检查
		if prev, exists := pathIndex[cfg.ReceivePath]; exists {
			return fmt.Errorf("config[%d] receive_path %q conflicts with config[%d]", i, cfg.ReceivePath, prev)
		}
		pathIndex[cfg.ReceivePath] = i

		// listen_addr 唯一性
		if w.listenAddr == "" {
			w.listenAddr = cfg.ListenAddr
		} else if w.listenAddr != cfg.ListenAddr {
			return fmt.Errorf("all configs must use the same listen_addr, got %s and %s", w.listenAddr, cfg.ListenAddr)
		}

		w.configs = append(w.configs, cfg)
	}
	return nil
}

// ──────────────────────────────────────────────
// Start
// ──────────────────────────────────────────────

// Start 挂载路由并启动 HTTP 服务。
func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	handlers := make(map[string]http.HandlerFunc, len(w.configs))
	for i := range w.configs {
		cfg := w.configs[i]
		handlers[cfg.ReceivePath] = w.buildHandler(cfg, out)
		log.Printf("[CTWING] route mounted path=%s listen=%s allowed_topic=%s",
			cfg.ReceivePath, w.listenAddr, cfg.AllowedTopic)
	}

	mux := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		path := normalizePath(req.URL.Path)
		if h, ok := handlers[path]; ok {
			h(rw, req)
			return
		}
		log.Printf("[CTWING] unmatched route method=%s path=%s remote=%s",
			req.Method, req.URL.Path, req.RemoteAddr)
		http.NotFound(rw, req)
	})

	w.server = &http.Server{Addr: w.listenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.server.Shutdown(shutdownCtx)
	}()

	log.Printf("[CTWING] listening at %s", w.listenAddr)
	if err := w.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[CTWING] server exited with error: %v", err)
	}
}

// ──────────────────────────────────────────────
// Handler
// ──────────────────────────────────────────────

func (w *Worker) buildHandler(cfg ConfigItem, out chan<- *base.DeviceData) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
		_ = req.Body.Close()
		if err != nil {
			http.Error(rw, "read body failed", http.StatusBadRequest)
			return
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(body, &raw); err != nil {
			log.Printf("[CTWING-DEBUG] invalid json from %s: %v", req.RemoteAddr, err)
			http.Error(rw, "invalid json body", http.StatusBadRequest)
			return
		}

		topic := fmt.Sprintf("%v", raw["topic"])
		if cfg.AllowedTopic != "" && topic != cfg.AllowedTopic {
			log.Printf("[CTWING] skip topic=%q (want %q) remote=%s", topic, cfg.AllowedTopic, req.RemoteAddr)
			replyOK(rw)
			return
		}

		deviceID := fmt.Sprintf("%v", raw["deviceId"])
		// 兼容IMEI字段
		if deviceID == "" && raw["IMEI"] != nil {
			deviceID = fmt.Sprintf("%v", raw["IMEI"])
		}

		// 兼容payload为对象或数组
		payload := raw["payload"]
		var items []map[string]interface{}
		switch v := payload.(type) {
		case []interface{}:
			for _, elem := range v {
				if m, ok := elem.(map[string]interface{}); ok {
					items = append(items, m)
				}
			}
		case map[string]interface{}:
			items = append(items, v)
		default:
			log.Printf("[CTWING-DEBUG] unknown payload type from %s: %T", req.RemoteAddr, payload)
			replyOK(rw)
			return
		}

		for _, item := range items {
			serviceID := fmt.Sprintf("%v", item["serviceId"])
			serviceData := item["serviceData"]
			var fields map[string]interface{}
			switch sd := serviceData.(type) {
			case map[string]interface{}:
				fields = sd
			case string:
				_ = json.Unmarshal([]byte(sd), &fields)
			}
			if fields == nil {
				fields = make(map[string]interface{})
			}
			// 设备ID优先取serviceData.deviceid
			devID := deviceID
			if v, ok := fields["deviceid"]; ok {
				if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" {
					devID = s
				}
			}
			dataType := serviceIDToDataType(serviceID)
			// 按需求：保存原始请求 JSON，不做拍平。
			payloadBytes := body

			// 若后续需要恢复拍平逻辑，可取消以下注释：
			// flat := buildFlatPayload(outerPush{
			// 	NotifyType: fmt.Sprintf("%v", raw["messageType"]),
			// 	RequestID:  fmt.Sprintf("%v", raw["upPacketSN"]),
			// 	DeviceID:   deviceID,
			// 	GatewayID:  "",
			// 	Topic:      topic,
			// 	Payload:    "",
			// }, serviceID, fields)
			// payloadBytes, _ := json.Marshal(flat)
			deviceData := &base.DeviceData{
				DeviceID:    devID,
				UniqueID:    devID,
				DeviceType:  cfg.DeviceType,
				Timestamp:   time.Now(),
				Payload:     json.RawMessage(payloadBytes),
				ComponyName: cfg.ComponyName,
				DataType:    dataType,
			}
			select {
			case out <- deviceData:
				log.Printf("[CTWING] pushed devId=%s serviceId=%s dataType=%s", devID, serviceID, dataType)
			default:
				log.Printf("[CTWING] channel full, drop devId=%s serviceId=%s", devID, serviceID)
			}
		}
		replyOK(rw)
	}
}

// ──────────────────────────────────────────────
// 辅助函数
// ──────────────────────────────────────────────

// normalizePath 确保路径以 "/" 开头且末尾无多余斜杠（空路径保持 "/"）。
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// matchAuth 校验请求头中的 token 是否匹配（恒定时间比较）。
func matchAuth(req *http.Request, header, token string) bool {
	if token == "" {
		return true
	}
	got := strings.TrimSpace(req.Header.Get(header))
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// extractDeviceID 从 serviceData 字段或外层 deviceId 中提取设备标识。
// serviceData.deviceid 是 IMEI 后12位，优先使用；若为空则回退到外层 AEP deviceId。
func extractDeviceID(serviceFields map[string]interface{}, fallbackID string) string {
	for _, key := range []string{"deviceid", "deviceId", "device_id", "imei"} {
		if v, ok := serviceFields[key]; ok {
			if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" {
				return s
			}
		}
	}
	if s := strings.TrimSpace(fallbackID); s != "" {
		return s
	}
	return "UNKNOWN_CTWING_DEVICE"
}

// serviceIDToDataType 将 CTWing serviceId 映射为系统内部 DataType 标识。
func serviceIDToDataType(serviceID string) string {
	switch strings.TrimSpace(serviceID) {
	case "DeviceStatus":
		return "CTWING_DEVICE_STATUS"
	case "Alarm":
		return "CTWING_ALARM"
	case "Control":
		return "CTWING_CONTROL"
	case "Reset":
		return "CTWING_RESET"
	case "Settings":
		return "CTWING_SETTINGS"
	default:
		if serviceID != "" {
			return "CTWING_" + strings.ToUpper(serviceID)
		}
		return "CTWING_PUSH"
	}
}

// buildFlatPayload 将三层数据展平为单层 map 便于存储与查询。
// 优先级：serviceData 字段 > 中层字段(serviceId) > 外层字段。
func buildFlatPayload(outer outerPush, serviceID string, serviceFields map[string]interface{}) map[string]interface{} {
	flat := map[string]interface{}{
		"_notifyType": outer.NotifyType,
		"_requestId":  outer.RequestID,
		"_topic":      outer.Topic,
		"_serviceId":  serviceID,
	}
	// 外层 deviceId（AEP 侧）可能与 serviceData.deviceid（IMEI后12位）不同，都保留
	if outer.DeviceID != "" {
		flat["_aepDeviceId"] = outer.DeviceID
	}
	// serviceData 字段直接展平到顶层
	for k, v := range serviceFields {
		flat[k] = v
	}
	return flat
}

// replyOK 返回统一的 200 JSON 响应（电信平台要求回 200 以停止重试）。
func replyOK(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(`{"code":0,"message":"ok"}`))
}
