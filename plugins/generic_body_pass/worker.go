// Package generic_body_pass 是一个极简的 HTTP 接收插件。
// 功能：将 POST 到指定路由的请求 body 原封不动放入 DeviceData.Payload 中。
// 适用于上游推送格式不确定，或无需解析直接透传的场景。
//
// 配置示例（config.yaml）：
//
//	- name: "generic_body_pass"
//	  enabled: true
//	  configs:
//	    - listen_addr: ":8100"
//	      receive_path: "/awjefil"
//	      compony_name: "某公司"
//	      device_type: "GENERIC_BODY_PASS"
//	      data_type: "GENERIC_BODY_PASS_UPLINK"
//	      device_id: "DEFAULT_DEVICE"
//	      unique_id: "gbp|default|GENERIC_BODY_PASS|DEFAULT"
//	      auth_header: ""
//	      auth_token: ""
//	      request_limit_bytes: 2097152
package generic_body_pass

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

// ConfigItem 单条路由配置。
type ConfigItem struct {
	ListenAddr   string `json:"listen_addr"`    // 监听地址，例如 :8100
	ReceivePath  string `json:"receive_path"`   // 接收路径，例如 /awjefil
	ComponyName  string `json:"compony_name"`   // 厂商/公司名，写入 DeviceData
	DeviceType   string `json:"device_type"`    // 设备类型，写入 DeviceData
	DataType     string `json:"data_type"`      // 数据类型，写入 DeviceData
	DeviceID     string `json:"device_id"`      // 设备ID，写入 DeviceData.DeviceID；若 body 内包含也自动提取
	UniqueID     string `json:"unique_id"`      // 唯一ID，写入 DeviceData.UniqueID
	AuthHeader   string `json:"auth_header"`    // 鉴权头名称，为空则不校验
	AuthToken    string `json:"auth_token"`      // 鉴权 token，为空则不校验
	RequestLimit int64  `json:"request_limit_bytes"` // 请求 body 大小限制
}

// Worker 实现 base.IWorker 接口。
type Worker struct {
	configs    []ConfigItem
	server     *http.Server
	listenAddr string
}

func init() {
	plugin.Register("generic_body_pass", func() base.IWorker {
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
			cfg.ListenAddr = ":8100"
		}
		if strings.TrimSpace(cfg.ReceivePath) == "" {
			cfg.ReceivePath = "/awjefil"
		}
		cfg.ReceivePath = normalizePath(cfg.ReceivePath)
		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "GENERIC_BODY_PASS"
		}
		if strings.TrimSpace(cfg.DataType) == "" {
			cfg.DataType = "GENERIC_BODY_PASS_UPLINK"
		}
		if strings.TrimSpace(cfg.DeviceID) == "" {
			cfg.DeviceID = "DEFAULT_DEVICE"
		}
		if strings.TrimSpace(cfg.UniqueID) == "" {
			cfg.UniqueID = cfg.DeviceID
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
	handlers := make(map[string]http.HandlerFunc, len(w.configs))
	for i := range w.configs {
		cfg := w.configs[i]
		handlers[cfg.ReceivePath] = w.buildHandler(cfg, out)
		log.Printf("[GENERIC_BODY_PASS] 路由已挂载 path=%s listen=%s", cfg.ReceivePath, w.listenAddr)
	}

	mux := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		path := normalizePath(req.URL.Path)
		if h, ok := handlers[path]; ok {
			h(rw, req)
			return
		}
		log.Printf("[GENERIC_BODY_PASS] 未匹配路由 method=%s path=%s remote=%s", req.Method, req.URL.Path, req.RemoteAddr)
		http.NotFound(rw, req)
	})

	w.server = &http.Server{Addr: w.listenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.server.Shutdown(shutdownCtx)
	}()

	log.Printf("[GENERIC_BODY_PASS] 正在监听 %s", w.listenAddr)
	if err := w.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[GENERIC_BODY_PASS] 服务异常退出: %v", err)
	}
}

// buildHandler 构建单条配置对应的 HTTP handler。
// 将 POST body 原封不动放入 DeviceData.Payload。
func (w *Worker) buildHandler(cfg ConfigItem, out chan<- *base.DeviceData) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(rw, "请求方法不允许", http.StatusMethodNotAllowed)
			return
		}

		// 鉴权校验（可选）
		if !matchAuth(req, cfg.AuthHeader, cfg.AuthToken) {
			http.Error(rw, "未授权", http.StatusUnauthorized)
			return
		}

		// 读取完整 body
		body, err := io.ReadAll(io.LimitReader(req.Body, cfg.RequestLimit))
		if err != nil {
			http.Error(rw, "读取请求体失败", http.StatusBadRequest)
			return
		}
		_ = req.Body.Close()

		// 从 body 中尝试提取 device_id/deviceId，若存在则覆盖配置中的 device_id
		deviceID := strings.TrimSpace(cfg.DeviceID)
		uniqueID := strings.TrimSpace(cfg.UniqueID)
		var bodyObj map[string]interface{}
		if err := json.Unmarshal(body, &bodyObj); err == nil && bodyObj != nil {
			if v, ok := bodyObj["device_id"]; ok {
				if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" {
					deviceID = s
				}
			} else if v, ok := bodyObj["deviceId"]; ok {
				if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" {
					deviceID = s
				}
			}
			if v, ok := bodyObj["unique_id"]; ok {
				if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" {
					uniqueID = s
				}
			} else if v, ok := bodyObj["uniqueId"]; ok {
				if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" {
					uniqueID = s
				}
			}

			// 移除 faceBitmapBase64 字段，避免照片数据撑爆 payload 列
			delete(bodyObj, "faceBitmapBase64")

			// 重新序列化，去掉 faceBitmapBase64 后的 body 作为最终的 Payload
			if cleaned, err := json.Marshal(bodyObj); err == nil {
				body = cleaned
			}
		}

		// 构造统一 DeviceData，body 原封不动放入 Payload
		msg := &base.DeviceData{
			DeviceID:    deviceID,
			UniqueID:    uniqueID,
			DeviceType:  strings.TrimSpace(cfg.DeviceType),
			DataType:    strings.TrimSpace(cfg.DataType),
			Timestamp:   time.Now(),
			Payload:     json.RawMessage(body),
			ComponyName: strings.TrimSpace(cfg.ComponyName),
		}

		// 投递到主通道
		select {
		case out <- msg:
			rw.Header().Set("Content-Type", "application/json; charset=utf-8")
			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"code":0,"message":"ok"}`))
			log.Printf("[GENERIC_BODY_PASS] 收到数据 path=%s deviceID=%s body_size=%d",
				cfg.ReceivePath, msg.DeviceID, len(body))
		case <-time.After(3 * time.Second):
			http.Error(rw, "服务繁忙", http.StatusServiceUnavailable)
			log.Printf("[GENERIC_BODY_PASS] 通道繁忙，丢弃数据 path=%s deviceID=%s", cfg.ReceivePath, msg.DeviceID)
		}
	}
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
