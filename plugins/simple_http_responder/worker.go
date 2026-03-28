// plugins/simple_http_responder/worker.go
package simple_http_responder

import (
	"context"
	"encoding/json"
	"fmt"
	"iot-middleware/pkg/auth"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	defaultListenAddr = ":8080"
	defaultUsername   = "admin"
)

// 单个配置项结构
type ConfigItem struct {
	ListenAddr string `json:"listen_addr"`
	Username   string `json:"username"` // 可选，默认 admin
	Password   string `json:"password"` // 明文密码（配置中写）
	AuthKey    string `json:"auth_key"` // 共享密钥，用于 HMAC
}

type SimpleHTTPResponder struct {
	configs []ConfigItem
	servers []*http.Server
}

func (w *SimpleHTTPResponder) Init(configs []json.RawMessage) error {
	log.Printf("[RESPONDER] 开始初始化，配置项数量: %d", len(configs))

	for i, config := range configs {
		var item ConfigItem
		if err := json.Unmarshal(config, &item); err != nil {
			return fmt.Errorf("第%d个配置解析失败: %v", i+1, err)
		}

		// 设置默认值
		if item.ListenAddr == "" {
			item.ListenAddr = defaultListenAddr
		}
		if item.Username == "" {
			item.Username = defaultUsername
		}

		// 校验必要字段
		if item.Password == "" {
			return fmt.Errorf("第%d个配置缺少 'password'", i+1)
		}
		if item.AuthKey == "" {
			return fmt.Errorf("第%d个配置缺少 'auth_key'", i+1)
		}

		w.configs = append(w.configs, item)
		log.Printf("[RESPONDER] 配置%d: 地址=%s, 用户=%s", i+1, item.ListenAddr, item.Username)
	}

	return nil
}

func (w *SimpleHTTPResponder) Start(ctx context.Context, out chan<- *base.DeviceData) {
	var wg sync.WaitGroup

	log.Printf("[RESPONDER] 启动插件，共 %d 个服务实例", len(w.configs))

	for i, config := range w.configs {
		wg.Add(1)
		go func(idx int, cfg ConfigItem) {
			defer wg.Done()
			w.startSingleService(ctx, out, cfg, idx)
		}(i, config)
	}

	// 等待所有服务结束
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		log.Println("[RESPONDER] 收到停止信号，正在关闭所有服务...")
		// 关闭所有服务
		for _, server := range w.servers {
			if server != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				server.Shutdown(shutdownCtx)
				cancel()
			}
		}
		<-done
	case <-done:
		log.Println("[RESPONDER] 所有服务实例已正常结束")
	}
}

func (w *SimpleHTTPResponder) startSingleService(ctx context.Context, out chan<- *base.DeviceData, config ConfigItem, index int) {
	_ = out

	validator, err := auth.NewValidator(auth.AuthConfig{
		Username: config.Username,
		Password: config.Password,
		AuthKey:  config.AuthKey,
		TTL:      12 * time.Hour,
	}, common.RDB, fmt.Sprintf("responder-%d", index+1))
	if err != nil {
		log.Printf("[RESPONDER-%d] 鉴权初始化失败: %v", index+1, err)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/login", validator.LoginHandler())
	mux.HandleFunc("/hello", validator.AuthMiddleware(func(res http.ResponseWriter, req *http.Request) {
		res.Header().Set("Content-Type", "text/plain; charset=utf-8")
		res.WriteHeader(http.StatusOK)
		//使用common.db连接数据库获取db_name中device_data表中的数据第一个并返回
		res.Write([]byte(fmt.Sprintf("hello world")))
		log.Printf("[RESPONDER-%d] 请求来自 %s，鉴权通过", index+1, req.RemoteAddr)
	}))

	server := &http.Server{
		Addr:    config.ListenAddr,
		Handler: mux,
	}
	w.servers = append(w.servers, server)

	log.Printf("[RESPONDER-%d] 🚀 响应服务已在 %s 开启...", index+1, config.ListenAddr)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[RESPONDER-%d] [FATAL] 响应服务异常退出: %v", index+1, err)
		}
	}()

	<-ctx.Done()
	log.Printf("[RESPONDER-%d] 服务已优雅关闭", index+1)
}

func init() {
	plugin.Register("simple_http_responder", func() base.IWorker {
		return &SimpleHTTPResponder{}
	})
}
