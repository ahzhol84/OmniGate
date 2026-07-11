// Package device_data_receiver 是一个通用的设备数据接收插件。
// 功能：接收设备通过 GPRS 网络上传的数据，支持 GET 和 POST 两种方式。
//
// GET 方式：解析 URL Query 参数，按 action 区分数据类型，返回字符串 "1"。
// POST 方式：读取 body 数据，适用于心电图原始波形、微聊语音等特殊数据。
//
// 配置示例（config.yaml）：
//
//	- name: "device_data_receiver"
//	  enabled: true
//	  configs:
//	    - listen_addr: ":8091"
//	      receive_path: "/devices/receivedata"
//	      compony_name: "某公司"
//	      device_type: "DEVICE_DATA_RECEIVER"
//	      data_type: "DEVICE_DATA_UPLINK"
//	      default_device_id: "DEFAULT_DEVICE"
//	      default_unique_id: "ddr|default|DEVICE_DATA_RECEIVER|DEFAULT"
//	      auth_header: ""
//	      auth_token: ""
//	      request_limit_bytes: 2097152
package device_data_receiver

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

// ConfigItem 单条路由配置。
type ConfigItem struct {
	ListenAddr       string `json:"listen_addr"`        // 监听地址，例如 :8091
	ReceivePath      string `json:"receive_path"`       // 接收路径，例如 /devices/receivedata
	ComponyName      string `json:"compony_name"`       // 厂商/公司名，写入 DeviceData
	DeviceType       string `json:"device_type"`        // 设备类型，写入 DeviceData
	DataType         string `json:"data_type"`          // 数据类型，写入 DeviceData
	DefaultDeviceID  string `json:"default_device_id"`  // 默认设备ID，当请求中未提供时使用
	DefaultUniqueID  string `json:"default_unique_id"`  // 默认唯一ID
	AuthHeader       string `json:"auth_header"`        // 鉴权头名称，为空则不校验
	AuthToken        string `json:"auth_token"`          // 鉴权 token，为空则不校验
	RequestLimit     int64  `json:"request_limit_bytes"` // 请求 body 大小限制
}

// Worker 实现 base.IWorker 接口。
type Worker struct {
	configs    []ConfigItem
	server     *http.Server
	listenAddr string
}

func init() {
	plugin.Register("device_data_receiver", func() base.IWorker {
		return &Worker{}
	})
}

// Init 解析并校验插件配置。
func (w *Worker) Init(configs []json.RawMessage) error {
	if len(configs) == 0 {
		return fmt.Errorf("配置为空")
	}

	pathIndex := make(map[string]int)

	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("配置[%d]解析失败: %w", i, err)
		}

		// 默认值
		if strings.TrimSpace(cfg.ListenAddr) == "" {
			cfg.ListenAddr = ":8091"
		}
		if strings.TrimSpace(cfg.ReceivePath) == "" {
			cfg.ReceivePath = "/devices/receivedata"
		}
		cfg.ReceivePath = normalizePath(cfg.ReceivePath)
		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "DEVICE_DATA_RECEIVER"
		}
		if strings.TrimSpace(cfg.DataType) == "" {
			cfg.DataType = "DEVICE_DATA_UPLINK"
		}
		if strings.TrimSpace(cfg.DefaultDeviceID) == "" {
			cfg.DefaultDeviceID = "DEFAULT_DEVICE"
		}
		if strings.TrimSpace(cfg.DefaultUniqueID) == "" {
			cfg.DefaultUniqueID = cfg.DefaultDeviceID
		}
		if cfg.RequestLimit <= 0 {
			cfg.RequestLimit = 2 << 20 // 2 MB
		}

		if prev, exists := pathIndex[cfg.ReceivePath]; exists {
			return fmt.Errorf("配置[%d]的接收路径 %q 与配置[%d]冲突", i, cfg.ReceivePath, prev)
		}
		pathIndex[cfg.ReceivePath] = i

		if w.listenAddr == "" {
			w.listenAddr = cfg.ListenAddr
		} else if w.listenAddr != cfg.ListenAddr {
			return fmt.Errorf("所有配置必须使用相同的监听地址，当前为 %s 和 %s", w.listenAddr, cfg.ListenAddr)
		}

		w.configs = append(w.configs, cfg)
	}

	return nil
}

// Start 启动 HTTP 服务，挂载路由。
func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	if len(w.configs) == 0 {
		log.Printf("[DEVICE_DATA_RECEIVER] 跳过启动：无配置项")
		return
	}

	handlers := make(map[string]http.HandlerFunc, len(w.configs))
	for i := range w.configs {
		cfg := w.configs[i]
		handlers[cfg.ReceivePath] = w.buildHandler(cfg, out)
		log.Printf("[DEVICE_DATA_RECEIVER] 路由已挂载 path=%s listen=%s", cfg.ReceivePath, w.listenAddr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
		path := normalizePath(req.URL.Path)
		if h, ok := handlers[path]; ok {
			h(rw, req)
			return
		}
		log.Printf("[DEVICE_DATA_RECEIVER] 未匹配路由 method=%s path=%s remote=%s", req.Method, req.URL.Path, req.RemoteAddr)
		http.NotFound(rw, req)
	})

	w.server = &http.Server{Addr: w.listenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.server.Shutdown(shutdownCtx)
	}()

	log.Printf("[DEVICE_DATA_RECEIVER] 正在监听 %s", w.listenAddr)
	if err := w.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[DEVICE_DATA_RECEIVER] 服务异常退出: %v", err)
	}
}

// buildHandler 构建单条配置对应的 HTTP handler。
// 支持 GET 和 POST 两种方式：
//   - GET：解析 URL Query 参数，按 action 区分数据类型，返回字符串 "1"
//   - POST：读取 body 数据，适用于心电图原始波形、微聊语音等特殊数据
func (w *Worker) buildHandler(cfg ConfigItem, out chan<- *base.DeviceData) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		// 鉴权校验（可选）
		if !matchAuth(req, cfg.AuthHeader, cfg.AuthToken) {
			http.Error(rw, "未授权", http.StatusUnauthorized)
			return
		}

		switch req.Method {
		case http.MethodGet:
			w.handleGET(rw, req, cfg, out)
		case http.MethodPost:
			w.handlePOST(rw, req, cfg, out)
		default:
			http.Error(rw, "请求方法不允许", http.StatusMethodNotAllowed)
		}
	}
}

// handleGET 处理 GET 请求，解析 URL Query 参数。
// 所有 GET 请求的数据都通过 URL Query 参数传递。
// 返回：成功返回字符串 "1"，失败返回非 200 状态码。
func (w *Worker) handleGET(rw http.ResponseWriter, req *http.Request, cfg ConfigItem, out chan<- *base.DeviceData) {
	query := req.URL.Query()

	// 获取 action 参数，用于区分数据类型
	action := strings.TrimSpace(query.Get("action"))
	if action == "" {
		log.Printf("[DEVICE_DATA_RECEIVER] GET 请求缺少 action 参数 remote=%s", req.RemoteAddr)
		http.Error(rw, "missing action", http.StatusBadRequest)
		return
	}

	// 获取设备 ID
	deviceID := strings.TrimSpace(query.Get("deviceid"))
	if deviceID == "" {
		deviceID = strings.TrimSpace(cfg.DefaultDeviceID)
	}

	// 构建 payload，包含所有 URL 参数
	payloadMap := make(map[string]interface{})
	for key, values := range query {
		if len(values) > 0 {
			payloadMap[key] = values[0]
		}
	}

	payloadBytes, err := json.Marshal(payloadMap)
	if err != nil {
		log.Printf("[DEVICE_DATA_RECEIVER] payload 序列化失败: %v", err)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}

	// 根据 action 确定 data_type 子类型
	dataType := buildDataType(cfg.DataType, action)

	msg := &base.DeviceData{
		DeviceID:    deviceID,
		UniqueID:    deviceID,
		DeviceType:  strings.TrimSpace(cfg.DeviceType),
		DataType:    dataType,
		Timestamp:   time.Now(),
		Payload:     json.RawMessage(payloadBytes),
		ComponyName: strings.TrimSpace(cfg.ComponyName),
	}

	// 投递到主通道
	select {
	case out <- msg:
		// 成功：返回字符串 "1"
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("1"))
		log.Printf("[DEVICE_DATA_RECEIVER] GET 数据已接收 action=%s deviceID=%s", action, msg.DeviceID)
	case <-time.After(3 * time.Second):
		http.Error(rw, "服务繁忙", http.StatusServiceUnavailable)
		log.Printf("[DEVICE_DATA_RECEIVER] 通道繁忙，丢弃数据 action=%s deviceID=%s", action, msg.DeviceID)
	}
}

// handlePOST 处理 POST 请求。
// 适用于：
//   - 心电图数据（action=submitecg）：URL 参数 + body 原始波形数据
//   - 微聊语音数据（action=chat）：JSON body 包含 base64 音频
//   - 睡眠垫实时数据（action=submitsleep）：JSON body
//   - 睡眠垫睡眠报告：JSON body
func (w *Worker) handlePOST(rw http.ResponseWriter, req *http.Request, cfg ConfigItem, out chan<- *base.DeviceData) {
	query := req.URL.Query()
	action := strings.TrimSpace(query.Get("action"))

	// 读取 body
	body, err := io.ReadAll(io.LimitReader(req.Body, cfg.RequestLimit))
	if err != nil {
		http.Error(rw, "读取请求体失败", http.StatusBadRequest)
		return
	}
	_ = req.Body.Close()

	// 获取设备 ID：优先从 URL 参数获取，其次从 body 获取，最后使用默认值
	deviceID := strings.TrimSpace(query.Get("deviceid"))
	if deviceID == "" {
		// 尝试从 body 中提取
		var bodyObj map[string]interface{}
		if err := json.Unmarshal(body, &bodyObj); err == nil && bodyObj != nil {
			if v, ok := bodyObj["deviceid"]; ok {
				deviceID = strings.TrimSpace(fmt.Sprintf("%v", v))
			} else if v, ok := bodyObj["imei"]; ok {
				deviceID = strings.TrimSpace(fmt.Sprintf("%v", v))
			}
		}
	}
	if deviceID == "" {
		deviceID = strings.TrimSpace(cfg.DefaultDeviceID)
	}

	// 构建 payload
	payloadMap := make(map[string]interface{})

	// 添加 URL 参数到 payload
	for key, values := range query {
		if len(values) > 0 {
			payloadMap[key] = values[0]
		}
	}

	// 尝试解析 body 为 JSON，如果是 JSON 则合并到 payload
	// 如果是原始二进制数据（如心电图波形），则作为 raw_body 字段
	var bodyObj map[string]interface{}
	if err := json.Unmarshal(body, &bodyObj); err == nil && bodyObj != nil {
		for k, v := range bodyObj {
			payloadMap[k] = v
		}
	} else {
		// 非 JSON body（如心电图原始16进制波形数据），以 base64 或原始字符串形式保存
		payloadMap["raw_body"] = string(body)
		payloadMap["body_size"] = len(body)
	}

	payloadBytes, err := json.Marshal(payloadMap)
	if err != nil {
		log.Printf("[DEVICE_DATA_RECEIVER] payload 序列化失败: %v", err)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}

	// 根据 action 确定 data_type 子类型
	dataType := buildDataType(cfg.DataType, action)

	msg := &base.DeviceData{
		DeviceID:    deviceID,
		UniqueID:    deviceID,
		DeviceType:  strings.TrimSpace(cfg.DeviceType),
		DataType:    dataType,
		Timestamp:   time.Now(),
		Payload:     json.RawMessage(payloadBytes),
		ComponyName: strings.TrimSpace(cfg.ComponyName),
	}

	// 投递到主通道
	select {
	case out <- msg:
		// 成功：返回字符串 "1"
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("1"))
		log.Printf("[DEVICE_DATA_RECEIVER] POST 数据已接收 action=%s deviceID=%s body_size=%d", action, msg.DeviceID, len(body))
	case <-time.After(3 * time.Second):
		http.Error(rw, "服务繁忙", http.StatusServiceUnavailable)
		log.Printf("[DEVICE_DATA_RECEIVER] 通道繁忙，丢弃数据 action=%s deviceID=%s", action, msg.DeviceID)
	}
}

// buildDataType 根据基础 data_type 和 action 构建具体的数据类型。
func buildDataType(baseType, action string) string {
	baseType = strings.TrimSpace(baseType)
	action = strings.TrimSpace(action)
	if action == "" {
		return baseType
	}
	return baseType + "_" + strings.ToUpper(action)
}

// normalizePath 确保路径以 "/" 开头并标准化。
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
		if p == "" {
			p = "/"
		}
	}
	return p
}

// matchAuth 校验请求头中的 token 是否匹配（恒定时间比较）。
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

// 确保 strconv 被使用（用于后续可能的类型转换扩展）
var _ = strconv.Itoa
