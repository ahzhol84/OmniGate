package zixing_smart_home

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "http://43.143.186.19"

type ConfigItem struct {
	BaseURL        string   `json:"base_url"`
	Account        string   `json:"account"`
	Password       string   `json:"password"`
	ClaimPrefixes  []string `json:"claim_prefixes"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type Worker struct {
	configs    []ConfigItem
	httpClient *http.Client
	tokenMu    sync.Mutex
	tokens     map[int]tokenCache
}

type tokenCache struct {
	Token    string
	ExpireAt time.Time
}

type loginResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		UserInfo struct {
			Token     string `json:"token"`
			ExpireAt  int64  `json:"expiretime"`
			ExpiresIn int64  `json:"expires_in"`
		} `json:"userinfo"`
	} `json:"data"`
}

func (w *Worker) Init(configs []json.RawMessage) error {
	if len(configs) == 0 {
		return fmt.Errorf("zixing_smart_home missing configs")
	}
	w.configs = make([]ConfigItem, 0, len(configs))
	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("zixing_smart_home config[%d] unmarshal failed: %w", i, err)
		}
		cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if cfg.BaseURL == "" {
			cfg.BaseURL = defaultBaseURL
		}
		cfg.Account = strings.TrimSpace(cfg.Account)
		cfg.Password = strings.TrimSpace(cfg.Password)
		if cfg.Account == "" || cfg.Password == "" {
			return fmt.Errorf("zixing_smart_home config[%d] account/password are required", i)
		}
		if len(cfg.ClaimPrefixes) == 0 {
			cfg.ClaimPrefixes = []string{"zixing|", "zx|"}
		}
		for idx := range cfg.ClaimPrefixes {
			cfg.ClaimPrefixes[idx] = strings.ToLower(strings.TrimSpace(cfg.ClaimPrefixes[idx]))
		}
		if cfg.TimeoutSeconds <= 0 {
			cfg.TimeoutSeconds = 60
		}
		w.configs = append(w.configs, cfg)
		log.Printf("[ZIXING] config[%d] loaded base_url=%s account=%s claim_prefixes=%v timeout=%ds", i+1, cfg.BaseURL, cfg.Account, cfg.ClaimPrefixes, cfg.TimeoutSeconds)
	}
	timeoutSeconds := w.configs[0].TimeoutSeconds
	w.httpClient = &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second}
	w.tokens = make(map[int]tokenCache)
	return nil
}

func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	_ = out
	<-ctx.Done()
}

func init() {
	plugin.Register("zixing_smart_home", func() base.IWorker {
		return &Worker{}
	})
}

func (w *Worker) ensureToken(ctx context.Context, cfgIdx int, cfg ConfigItem) (string, error) {
	w.tokenMu.Lock()
	cached, exists := w.tokens[cfgIdx]
	if exists && cached.Token != "" && time.Until(cached.ExpireAt) > 30*time.Second {
		token := cached.Token
		w.tokenMu.Unlock()
		return token, nil
	}
	w.tokenMu.Unlock()

	if token, expireAt, ok := w.getTokenFromRedis(ctx, cfgIdx, cfg); ok {
		w.tokenMu.Lock()
		w.tokens[cfgIdx] = tokenCache{Token: token, ExpireAt: expireAt}
		w.tokenMu.Unlock()
		return token, nil
	}

	form := url.Values{}
	form.Set("account", cfg.Account)
	form.Set("password", cfg.Password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/index.php/api/user/login", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("zixing login http=%d body=%s", resp.StatusCode, string(body))
	}

	var lr loginResp
	if err := json.Unmarshal(body, &lr); err != nil {
		return "", fmt.Errorf("zixing login parse failed: %w", err)
	}
	if lr.Code != 1 {
		return "", fmt.Errorf("zixing login failed: %s", lr.Msg)
	}
	token := strings.TrimSpace(lr.Data.UserInfo.Token)
	if token == "" {
		return "", fmt.Errorf("zixing login token empty")
	}

	expireAt := time.Now().Add(20 * time.Minute)
	if lr.Data.UserInfo.ExpireAt > 0 {
		expireAt = time.Unix(lr.Data.UserInfo.ExpireAt, 0)
	} else if lr.Data.UserInfo.ExpiresIn > 0 {
		expireAt = time.Now().Add(time.Duration(lr.Data.UserInfo.ExpiresIn) * time.Second)
	}

	w.cacheToken(ctx, cfgIdx, cfg, token, expireAt)

	return token, nil
}

func (w *Worker) cacheToken(ctx context.Context, cfgIdx int, cfg ConfigItem, token string, expireAt time.Time) {
	w.tokenMu.Lock()
	w.tokens[cfgIdx] = tokenCache{Token: token, ExpireAt: expireAt}
	w.tokenMu.Unlock()

	if common.RDB == nil {
		return
	}
	key := tokenRedisKey(cfg)
	ttl := time.Until(expireAt)
	if ttl <= 0 {
		ttl = 20 * time.Minute
	}
	if err := common.RDB.Set(ctx, key, token, ttl).Err(); err != nil {
		log.Printf("[ZIXING] cache redis token failed key=%s err=%v", key, err)
	}
}

func (w *Worker) getTokenFromRedis(ctx context.Context, cfgIdx int, cfg ConfigItem) (string, time.Time, bool) {
	if common.RDB == nil {
		return "", time.Time{}, false
	}
	key := tokenRedisKey(cfg)
	val, err := common.RDB.Get(ctx, key).Result()
	if err != nil {
		return "", time.Time{}, false
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return "", time.Time{}, false
	}
	ttl, err := common.RDB.TTL(ctx, key).Result()
	if err != nil {
		return "", time.Time{}, false
	}
	if ttl <= 30*time.Second {
		_ = common.RDB.Del(ctx, key).Err()
		return "", time.Time{}, false
	}
	return val, time.Now().Add(ttl), true
}

func (w *Worker) invalidateToken(ctx context.Context, cfgIdx int, cfg ConfigItem) {
	w.tokenMu.Lock()
	delete(w.tokens, cfgIdx)
	w.tokenMu.Unlock()
	if common.RDB != nil {
		_ = common.RDB.Del(ctx, tokenRedisKey(cfg)).Err()
	}
}

func tokenRedisKey(cfg ConfigItem) string {
	base := strings.ToLower(strings.TrimSpace(cfg.BaseURL))
	base = strings.TrimPrefix(base, "http://")
	base = strings.TrimPrefix(base, "https://")
	base = strings.ReplaceAll(base, "/", "_")
	base = strings.ReplaceAll(base, ":", "_")
	acct := strings.TrimSpace(cfg.Account)
	if acct == "" {
		acct = "unknown"
	}
	return fmt.Sprintf("iot:zixing:token:%s:%s", base, acct)
}
