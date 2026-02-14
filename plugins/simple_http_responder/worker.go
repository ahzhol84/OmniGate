// plugins/simple_http_responder/worker.go
package simple_http_responder

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	tokenTTL          = 12 * time.Hour
	cleanupInterval   = 1 * time.Hour
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

func (w *SimpleHTTPResponder) generateToken() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
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
	// 计算预期的 credential（HMAC-SHA256 + base64）
	h := hmac.New(sha256.New, []byte(config.AuthKey))
	h.Write([]byte(config.Password))
	expectedCredential := base64.StdEncoding.EncodeToString(h.Sum(nil))

	mux := http.NewServeMux()

	// 登录接口
	mux.HandleFunc("/login", func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(res, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var payload struct {
			Credential string `json:"credential"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(res, "invalid json", http.StatusBadRequest)
			return
		}

		if payload.Credential != expectedCredential {
			http.Error(res, "invalid credential", http.StatusUnauthorized)
			return
		}

		// 提取IP（处理IPv6和Port）
		ip, _, err := net.SplitHostPort(req.RemoteAddr)
		if err != nil {
			ip = req.RemoteAddr // 兜底
		}

		token := w.generateToken()
		ipKey := "responder:ip:" + ip
		tokenKey := "responder:token:" + token

		// 原子操作：查旧token -> 删旧token -> 写新token
		pipe := common.RDB.Pipeline()

		// 获取该IP已有的token
		oldToken, _ := common.RDB.Get(ctx, ipKey).Result()
		if oldToken != "" {
			pipe.Del(ctx, "responder:token:"+oldToken) // 顶掉旧token
			log.Printf("[RESPONDER-%d] IP %s 顶掉旧token", index+1, ip)
		}

		// 写新映射：IP->token, token->username
		pipe.Set(ctx, ipKey, token, tokenTTL)
		pipe.Set(ctx, tokenKey, config.Username, tokenTTL)

		_, err = pipe.Exec(ctx)
		if err != nil {
			log.Printf("[RESPONDER-%d] [ERROR] Redis pipeline失败: %v", index+1, err)
			http.Error(res, "internal error", http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		json.NewEncoder(res).Encode(map[string]interface{}{
			"code":       0,
			"msg":        "ok",
			"token":      token,
			"expires_in": int(tokenTTL.Seconds()),
		})
		log.Printf("[RESPONDER-%d] IP %s 登录成功，token %s，有效期 %v", index+1, ip, token, tokenTTL)
	})

	// 业务接口
	mux.HandleFunc("/hello", func(res http.ResponseWriter, req *http.Request) {
		token := req.Header.Get("X-Token")
		if token == "" {
			http.Error(res, "missing X-Token header", http.StatusUnauthorized)
			return
		}

		val, err := common.RDB.Get(req.Context(), "responder:token:"+token).Result()
		if err == redis.Nil {
			http.Error(res, "invalid or expired token", http.StatusUnauthorized)
			return
		}
		if err != nil {
			log.Printf("[RESPONDER-%d] [ERROR] Redis 查询 token 失败: %v", index+1, err)
			http.Error(res, "internal error", http.StatusInternalServerError)
			return
		}
		// val 是用户名（可选用于日志）
		log.Printf("[RESPONDER-%d] [USERNAME] %s", index+1, val)

		res.Header().Set("Content-Type", "text/plain; charset=utf-8")
		res.WriteHeader(http.StatusOK)
		res.Write([]byte("hello world"))
		log.Printf("[RESPONDER-%d] 请求来自 %s，使用有效 token", index+1, req.RemoteAddr)
	})

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
