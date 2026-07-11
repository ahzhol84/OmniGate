package teaching_device_scaffold

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

// ConfigItem 是教学插件的单条配置。
// 这个结构体故意保留“最小可用字段”，方便教学时快速看懂。
// 一个 config 对应一个接入端点（一个监听路径）。
type ConfigItem struct {
	ListenAddr   string `json:"listen_addr"`    // 监听地址，例如 :8099
	ReceivePath  string `json:"receive_path"`   // 上行接收路径，例如 /push/teaching
	ComponyName  string `json:"compony_name"`   // 厂商/公司名，写入 DeviceData.ComponyName
	DeviceType   string `json:"device_type"`    // 设备类型，写入 DeviceData.DeviceType
	DataType     string `json:"data_type"`      // 数据类型，写入 DeviceData.DataType
	AuthHeader   string `json:"auth_header"`    // 鉴权头名称，例如 X-Iot-Token
	AuthToken    string `json:"auth_token"`     // 鉴权 token；为空时表示不校验
	DemoDeviceID string `json:"demo_device_id"` // 当上行 body 不带 device_id 时使用该默认值
	DemoUniqueID string `json:"demo_unique_id"` // 当上行 body 不带 unique_id 时使用该默认值
}

// Worker 是教学插件主体。
// 说明：
// 1) Start 负责“正向隧道”——接收 HTTP 数据并写入 out 通道。
// 2) SendCommand（在 sendcommand.go）负责“反向隧道”——认领并处理下行命令。
type Worker struct {
	configs    []ConfigItem
	listenAddr string
	server     *http.Server
}

// Init 解析并校验配置。
// 教学重点：
// - 多配置项支持
// - 默认值填充
// - 基本冲突校验（同一路径不能重复）
func (w *Worker) Init(configs []json.RawMessage) error {
	if len(configs) == 0 {
		return fmt.Errorf("empty configs")
	}

	pathSeen := make(map[string]struct{}, len(configs))
	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d] parse failed: %w", i, err)
		}

		if strings.TrimSpace(cfg.ListenAddr) == "" {
			cfg.ListenAddr = ":8099"
		}
		if strings.TrimSpace(cfg.ReceivePath) == "" {
			cfg.ReceivePath = "/push/teaching"
		}
		cfg.ReceivePath = normalizePath(cfg.ReceivePath)
		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "TEACHING_DEVICE"
		}
		if strings.TrimSpace(cfg.DataType) == "" {
			cfg.DataType = "TEACHING_UPLINK"
		}
		if strings.TrimSpace(cfg.AuthHeader) == "" {
			cfg.AuthHeader = "X-Iot-Token"
		}

		if _, exists := pathSeen[cfg.ReceivePath]; exists {
			return fmt.Errorf("config[%d] receive_path %q duplicated", i, cfg.ReceivePath)
		}
		pathSeen[cfg.ReceivePath] = struct{}{}

		if w.listenAddr == "" {
			w.listenAddr = cfg.ListenAddr
		} else if w.listenAddr != cfg.ListenAddr {
			return fmt.Errorf("all configs must use same listen_addr, got %s and %s", w.listenAddr, cfg.ListenAddr)
		}

		w.configs = append(w.configs, cfg)
	}
	return nil
}

// Start 启动上行接入服务（正向隧道入口）。
// 流程：
// 1) 按配置挂载路由
// 2) 接收 HTTP POST 数据
// 3) 组装统一 DeviceData
// 4) 投递到 out（主程序会继续广播+落库）
func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	if len(w.configs) == 0 {
		log.Printf("[TEACHING] 跳过启动：无配置项")
		return
	}

	handlers := make(map[string]http.HandlerFunc, len(w.configs))
	for i := range w.configs {
		cfg := w.configs[i]
		handlers[cfg.ReceivePath] = w.buildUplinkHandler(cfg, out)
		log.Printf("[TEACHING] 路由已挂载 path=%s listen=%s", cfg.ReceivePath, w.listenAddr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
		path := normalizePath(req.URL.Path)
		h, ok := handlers[path]
		if !ok {
			http.NotFound(rw, req)
			return
		}
		h(rw, req)
	})

	w.server = &http.Server{Addr: w.listenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.server.Shutdown(shutdownCtx)
	}()

	log.Printf("[TEACHING] 正在监听 %s", w.listenAddr)
	if err := w.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[TEACHING] 服务器退出异常: %v", err)
	}
}

// buildUplinkHandler 构建单条配置对应的上行处理器。
// 教学目标：用最少代码展示“请求接入 -> 标准化 -> 投递传送带”。
func (w *Worker) buildUplinkHandler(cfg ConfigItem, out chan<- *base.DeviceData) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !matchAuth(req, cfg.AuthHeader, cfg.AuthToken) {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
		if err != nil {
			http.Error(rw, "read body failed", http.StatusBadRequest)
			return
		}
		_ = req.Body.Close()

		payloadObj := map[string]interface{}{}
		_ = json.Unmarshal(body, &payloadObj)

		deviceID := firstNonEmpty(
			asString(payloadObj["device_id"]),
			asString(payloadObj["deviceId"]),
			strings.TrimSpace(cfg.DemoDeviceID),
			"TEACHING_DEVICE_001",
		)
		uniqueID := firstNonEmpty(
			asString(payloadObj["unique_id"]),
			asString(payloadObj["uniqueId"]),
			strings.TrimSpace(cfg.DemoUniqueID),
			deviceID,
		)

		msg := &base.DeviceData{
			DeviceID:    deviceID,
			UniqueID:    uniqueID,
			DeviceType:  strings.TrimSpace(cfg.DeviceType),
			DataType:    strings.TrimSpace(cfg.DataType),
			Timestamp:   time.Now(),
			Payload:     json.RawMessage(body),
			ComponyName: strings.TrimSpace(cfg.ComponyName),
		}

		select {
		case out <- msg:
			writeJSON(rw, http.StatusOK, map[string]interface{}{
				"code":      0,
				"message":   "teaching uplink accepted",
				"device_id": msg.DeviceID,
				"unique_id": msg.UniqueID,
			})
		case <-time.After(2 * time.Second):
			http.Error(rw, "uplink channel busy", http.StatusGatewayTimeout)
		}
	}
}

func normalizePath(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

func matchAuth(req *http.Request, headerName string, expectedToken string) bool {
	expectedToken = strings.TrimSpace(expectedToken)
	if expectedToken == "" {
		return true
	}
	got := strings.TrimSpace(req.Header.Get(strings.TrimSpace(headerName)))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedToken)) == 1
}

func writeJSON(rw http.ResponseWriter, code int, data interface{}) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(data)
}

func asString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", v))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func init() {
	plugin.Register("teaching_device_scaffold", func() base.IWorker {
		return &Worker{}
	})
}
