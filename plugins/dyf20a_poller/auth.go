// plugins/dyf20a_poller/auth.go
package dyf20a_poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"iot-middleware/pkg/common"
)

type TokenManager struct {
	username    string
	password    string
	baseURL     string
	accessToken string
	mu          sync.RWMutex
}

// NewTokenManager 只初始化结构体，不发起网络请求
func NewTokenManager(username, password, baseURL string) *TokenManager {
	return &TokenManager{
		username: username,
		password: password,
		baseURL:  baseURL,
	}
}

// GetToken 线程安全地获取带缓存的 token
func (tm *TokenManager) GetToken() string {
	ctx := context.Background()
	redisKey := fmt.Sprintf("iot:token:%s", tm.username)

	// 1. 先从 Redis 读
	if val, err := common.RDB.Get(ctx, redisKey).Result(); err == nil && val != "" {
		return val
	}

	// 2. Redis 没有，去 API 拿
	fmt.Printf("⚠️ Redis 中未找到 Token [%s]，准备请求 API...\n", tm.username)
	token, err := tm.requestNewToken()
	if err != nil {
		fmt.Printf("❌ 请求新 Token 失败: %v\n", err)
		return ""
	}

	// 3. 写回 Redis（11 小时过期）
	common.RDB.Set(ctx, redisKey, token, 11*time.Hour)
	return token
}

// requestNewToken 发起 HTTP 请求获取新 token
func (tm *TokenManager) requestNewToken() (string, error) {
	// apiURL := "https://api.dayufeng.cn/get/token"
	apiURL := tm.baseURL + "/get/token"
	data := url.Values{}
	data.Set("username", tm.username)
	data.Set("password", tm.password)
	data.Set("login_type", "1")

	resp, err := http.Post(apiURL, "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var res struct {
		Code int `json:"code"`
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("解析 token 响应失败: %w, body=%s", err, string(body))
	}

	if res.Code != 200 || res.Data.AccessToken == "" {
		return "", fmt.Errorf("API 返回错误: code=%d, msg=%s", res.Code, string(body))
	}

	tm.mu.Lock()
	tm.accessToken = res.Data.AccessToken
	tm.mu.Unlock()

	return tm.accessToken, nil
}
