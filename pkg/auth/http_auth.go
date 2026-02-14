package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	defaultTokenTTL = 12 * time.Hour
	tokenPrefix     = "auth:token:"
	ipPrefix        = "auth:ip:"
)

// RedisClient 解耦，common.RDB 直接满足
type RedisClient interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Pipeline() redis.Pipeliner
}

// AuthConfig 鉴权配置
type AuthConfig struct {
	Username string        // 默认 admin
	Password string        // 明文
	AuthKey  string        // HMAC 密钥
	TTL      time.Duration // 默认 12h，0 则使用默认
}

// Validator 鉴权器实例
type Validator struct {
	redis        RedisClient
	expectedCred string
	username     string
	ttl          time.Duration
	prefix       string // 实例前缀，用于日志或key隔离（可选）
}

// NewValidator 创建鉴权器
// prefix: 实例标识，如 "sxb-poller-1"，用于日志区分，也可用于key隔离
func NewValidator(cfg AuthConfig, redis RedisClient, prefix string) (*Validator, error) {
	if cfg.Password == "" || cfg.AuthKey == "" {
		return nil, fmt.Errorf("password and auth_key required")
	}
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	if cfg.TTL == 0 {
		cfg.TTL = defaultTokenTTL
	}

	h := hmac.New(sha256.New, []byte(cfg.AuthKey))
	h.Write([]byte(cfg.Password))

	return &Validator{
		redis:        redis,
		expectedCred: base64.StdEncoding.EncodeToString(h.Sum(nil)),
		username:     cfg.Username,
		ttl:          cfg.TTL,
		prefix:       prefix,
	}, nil
}

// LoginHandler 登录接口 Handler
// 插件使用: mux.HandleFunc("/login", validator.LoginHandler())
func (v *Validator) LoginHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"code":-1,"msg":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Credential string `json:"credential"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"code":-1,"msg":"invalid json"}`, http.StatusBadRequest)
			return
		}

		if req.Credential != v.expectedCred {
			http.Error(w, `{"code":-1,"msg":"invalid credential"}`, http.StatusUnauthorized)
			return
		}

		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip == "" {
			ip = r.RemoteAddr
		}

		token := v.genToken()
		ipKey := ipPrefix + ip
		tokenKey := tokenPrefix + token

		// 原子替换旧token
		pipe := v.redis.Pipeline()
		if oldToken, _ := v.redis.Get(r.Context(), ipKey).Result(); oldToken != "" {
			pipe.Del(r.Context(), tokenPrefix+oldToken)
		}
		pipe.Set(r.Context(), ipKey, token, v.ttl)
		pipe.Set(r.Context(), tokenKey, v.username, v.ttl)

		if _, err := pipe.Exec(r.Context()); err != nil {
			http.Error(w, `{"code":-1,"msg":"internal error"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code":       0,
			"msg":        "ok",
			"token":      token,
			"expires_in": int(v.ttl.Seconds()),
		})
	}
}

// AuthMiddleware 鉴权中间件
// 插件使用: mux.HandleFunc("/api/xxx", validator.AuthMiddleware(actualHandler))
func (v *Validator) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Token")
		if token == "" {
			http.Error(w, `{"code":-1,"msg":"missing token"}`, http.StatusUnauthorized)
			return
		}

		username, err := v.redis.Get(r.Context(), tokenPrefix+token).Result()
		if err == redis.Nil {
			http.Error(w, `{"code":-1,"msg":"invalid or expired token"}`, http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, `{"code":-1,"msg":"internal error"}`, http.StatusInternalServerError)
			return
		}

		// 注入 username 到 context，下游可取
		ctx := context.WithValue(r.Context(), "auth_user", username)
		next(w, r.WithContext(ctx))
	}
}

// CheckToken 非 HTTP 场景直接校验（返回 username, ok）
func (v *Validator) CheckToken(ctx context.Context, token string) (string, bool) {
	username, err := v.redis.Get(ctx, tokenPrefix+token).Result()
	if err != nil {
		return "", false
	}
	return username, true
}

func (v *Validator) genToken() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}
