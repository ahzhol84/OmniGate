package huanjing_jiankong_v3

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
	"strings"
	"sync"
	"time"
)

// ConfigItem 是綜合環境監控云平台 3.0 的单条配置。
type ConfigItem struct {
	BaseURL        string         `json:"base_url"`        // 平台基础URL，例如 http://www.0531yun.com
	LoginName      string         `json:"login_name"`      // 登录用户名
	Password       string         `json:"password"`        // 登录密码
	ComponyName    string         `json:"compony_name"`    // 公司名称，写入 DeviceData.ComponyName
	PollIntervalM  int            `json:"poll_interval_m"` // 轮询间隔（分钟），0表示禁用轮询
	Devices        []DeviceConfig `json:"devices"`         // 要监控的设备列表
	CommandTimeout int            `json:"command_timeout"` // 命令超时时间（秒），默认10
	ErrorRetryMax  int            `json:"error_retry_max"` // 错误重试次数，默认3
}

// DeviceConfig 表示一个要监控的设备及其因子。
type DeviceConfig struct {
	DeviceAddr int    `json:"device_addr"` // 设备地址
	DeviceName string `json:"device_name"` // 设备名称
	NodeID     int    `json:"node_id"`     // 节点ID（用于寻址数据）
	RegisterID int    `json:"register_id"` // 寄存器ID（用于寻址数据）
	FactorName string `json:"factor_name"` // 因子名称，例如 光照
	Unit       string `json:"unit"`        // 单位，例如 lux
}

// Worker 是綜合環境監控云平台 3.0 的插件主体。
// 职责：
// 1) Start 负责"正向隧道"——定时轮询平台数据并写入 out
// 2) SendCommand 负责"反向隧道"——认领并处理继电器控制命令
type Worker struct {
	configs    []ConfigItem
	httpClient *http.Client
}

// tokenRedisKey 生成 token 在 redis 中的 key
func (w *Worker) tokenRedisKey(baseURL, loginName string) string {
	base := strings.ToLower(strings.TrimSpace(baseURL))
	base = strings.TrimPrefix(base, "http://")
	base = strings.TrimPrefix(base, "https://")
	base = strings.ReplaceAll(base, "/", "_")
	return fmt.Sprintf("iot:token:huanjing_v3:%s:%s", base, strings.TrimSpace(loginName))
}

// Init 解析并校验配置。
func (w *Worker) Init(configs []json.RawMessage) error {
	if len(configs) == 0 {
		return fmt.Errorf("empty configs")
	}

	if w.httpClient == nil {
		w.httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}

	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d] parse failed: %w", i, err)
		}

		// 填充默认值
		cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
		if cfg.BaseURL == "" {
			cfg.BaseURL = "http://www.0531yun.com"
		}
		cfg.LoginName = strings.TrimSpace(cfg.LoginName)
		if cfg.LoginName == "" {
			return fmt.Errorf("config[%d] login_name required", i)
		}
		cfg.Password = strings.TrimSpace(cfg.Password)
		if cfg.Password == "" {
			return fmt.Errorf("config[%d] password required", i)
		}
		if cfg.ComponyName == "" {
			cfg.ComponyName = "綜合環境監控"
		}
		if cfg.PollIntervalM == 0 {
			cfg.PollIntervalM = 5 // 默认5分钟
		}
		if cfg.CommandTimeout == 0 {
			cfg.CommandTimeout = 10
		}
		if cfg.ErrorRetryMax == 0 {
			cfg.ErrorRetryMax = 3
		}

		if len(cfg.Devices) == 0 {
			log.Printf("[HUANJING] config[%d] warning: no devices configured", i)
		}

		w.configs = append(w.configs, cfg)
	}

	log.Printf("[HUANJING] init: loaded %d config(s)", len(w.configs))
	return nil
}

// Start 启动轮询服务（正向隧道入口）。
// 每个 config 都会单独启动一个轮询 goroutine。
func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	if len(w.configs) == 0 {
		log.Printf("[HUANJING] skip start: no configs")
		return
	}

	var wg sync.WaitGroup
	for i, cfg := range w.configs {
		wg.Add(1)
		go func(idx int, item ConfigItem) {
			defer wg.Done()
			w.pollWorker(ctx, item, idx, out)
		}(i, cfg)
	}

	log.Printf("[HUANJING] started %d poller(s)", len(w.configs))
	<-ctx.Done()
	wg.Wait()
}

// pollWorker 是单个配置的轮询工作循环。
func (w *Worker) pollWorker(ctx context.Context, cfg ConfigItem, cfgIdx int, out chan<- *base.DeviceData) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[HUANJING] config[%d] poller panic recovered: %v", cfgIdx, r)
		}
	}()

	if cfg.PollIntervalM <= 0 {
		log.Printf("[HUANJING] config[%d] polling disabled (interval=%d)", cfgIdx, cfg.PollIntervalM)
		return
	}

	interval := time.Duration(cfg.PollIntervalM) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 立即执行一次轮询
	w.doPoll(ctx, cfg, cfgIdx, out)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[HUANJING] config[%d] poller stopped", cfgIdx)
			return
		case <-ticker.C:
			w.doPoll(ctx, cfg, cfgIdx, out)
		}
	}
}

// doPoll 执行一次完整的数据拉取周期，包含错误重试。
func (w *Worker) doPoll(ctx context.Context, cfg ConfigItem, cfgIdx int, out chan<- *base.DeviceData) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[HUANJING] config[%d] doPoll panic recovered: %v", cfgIdx, r)
		}
	}()

	maxRetries := cfg.ErrorRetryMax
	if maxRetries <= 0 {
		maxRetries = 3
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		lastErr = w.doPollOnce(ctx, cfg, cfgIdx, out)
		if lastErr == nil {
			return // 成功
		}

		// 失败，准备重试
		if attempt < maxRetries {
			backoffDuration := time.Duration(attempt) * 5 * time.Second
			log.Printf("[HUANJING] config[%d] poll attempt %d failed, retrying in %v: %v",
				cfgIdx, attempt, backoffDuration, lastErr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoffDuration):
				// 继续重试
			}
		}
	}

	if lastErr != nil {
		log.Printf("[HUANJING] config[%d] poll failed after %d attempts: %v", cfgIdx, maxRetries, lastErr)
	}
}

// doPollOnce 执行一次轮询流程（不含重试）。
func (w *Worker) doPollOnce(ctx context.Context, cfg ConfigItem, cfgIdx int, out chan<- *base.DeviceData) error {
	// 1) 获取或刷新token
	token, err := w.getOrRefreshToken(ctx, cfg)
	if err != nil {
		return fmt.Errorf("get token failed: %w", err)
	}

	// 2) 构建设备地址列表
	var deviceAddrs []string
	for _, dev := range cfg.Devices {
		deviceAddrs = append(deviceAddrs, fmt.Sprintf("%d", dev.DeviceAddr))
	}
	if len(deviceAddrs) == 0 {
		return fmt.Errorf("no devices configured")
	}
	devAddrStr := strings.Join(deviceAddrs, ",")

	// 3) 调用 getRealTimeData 接口
	data, err := w.getRealTimeData(ctx, cfg, token, devAddrStr)
	if err != nil {
		return fmt.Errorf("getRealTimeData failed: %w", err)
	}

	// 4) 解析响应并投递 DeviceData
	w.processRealTimeData(cfg, cfgIdx, data, out)
	return nil
}

// getOrRefreshToken 获取或刷新登录token，优先从 redis 读取。
func (w *Worker) getOrRefreshToken(ctx context.Context, cfg ConfigItem) (string, error) {
	key := w.tokenRedisKey(cfg.BaseURL, cfg.LoginName)

	// 1) 如果 redis 可用，先尝试从 redis 读取
	if common.RDB != nil {
		val, err := common.RDB.Get(ctx, key).Result()
		if err == nil && strings.TrimSpace(val) != "" {
			// 检查 redis 中的 key 剩余 TTL
			ttl, errTTL := common.RDB.TTL(ctx, key).Result()
			if errTTL == nil && ttl > 30*time.Second {
				log.Printf("[HUANJING] token from redis, remaining TTL: %v", ttl)
				return val, nil
			}
		}
	}

	// 2) redis 中没有有效 token，去 API 重新登录
	loginURL := fmt.Sprintf("%s/api/getToken?loginName=%s&password=%s",
		cfg.BaseURL, cfg.LoginName, cfg.Password)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse login response: %w", err)
	}

	// 检查返回码
	code, ok := result["code"].(float64)
	if !ok || code != 1000 {
		return "", fmt.Errorf("login failed: code=%v", result["code"])
	}

	// 提取token和过期时间
	data, ok := result["data"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid login response: no data field")
	}

	token, ok := data["token"].(string)
	if !ok || token == "" {
		return "", fmt.Errorf("invalid login response: no token")
	}

	// 计算过期时间
	var ttl time.Duration = 3600 * time.Second // 默认3600秒
	if expMs, ok := data["expiration"].(float64); ok {
		expiration := time.UnixMilli(int64(expMs))
		ttl = time.Until(expiration)
		if ttl <= 0 {
			ttl = 3600 * time.Second
		}
	}

	// 3) 保存到 redis
	if common.RDB != nil {
		if err := common.RDB.Set(ctx, key, token, ttl).Err(); err != nil {
			log.Printf("[HUANJING] failed to cache token in redis: %v", err)
		} else {
			log.Printf("[HUANJING] token cached in redis, TTL: %v", ttl)
		}
	}

	log.Printf("[HUANJING] new token acquired, expires in %v", ttl)
	return token, nil
}

// getRealTimeData 调用平台的 getRealTimeData 接口。
func (w *Worker) getRealTimeData(ctx context.Context, cfg ConfigItem, token string, deviceAddrs string) ([]map[string]interface{}, error) {
	url := fmt.Sprintf("%s/api/data/getRealTimeData?deviceAddrs=%s", cfg.BaseURL, deviceAddrs)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", token)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// 检查返回码
	code, ok := result["code"].(float64)
	if !ok || code != 1000 {
		return nil, fmt.Errorf("getRealTimeData failed: code=%v, message=%v",
			result["code"], result["message"])
	}

	// 提取data数组
	rawData, ok := result["data"]
	if !ok {
		return nil, fmt.Errorf("invalid response: no data field")
	}

	dataArray, ok := rawData.([]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid response: data is not array")
	}

	var dataList []map[string]interface{}
	for _, item := range dataArray {
		if m, ok := item.(map[string]interface{}); ok {
			dataList = append(dataList, m)
		}
	}

	return dataList, nil
}

// processRealTimeData 解析实时数据并投递到 out 通道。
func (w *Worker) processRealTimeData(cfg ConfigItem, cfgIdx int, dataList []map[string]interface{}, out chan<- *base.DeviceData) {
	for _, item := range dataList {
		deviceAddr, _ := item["deviceAddr"].(float64)
		deviceName, _ := item["deviceName"].(string)
		timeStampMs, _ := item["timeStamp"].(float64)

		deviceID := fmt.Sprintf("%d", int64(deviceAddr))
		uniqueID := deviceID

		// 解析设备的所有因子数据
		dataItems, ok := item["dataItem"].([]interface{})
		if !ok {
			continue
		}

		for _, dataItemRaw := range dataItems {
			dataItem, ok := dataItemRaw.(map[string]interface{})
			if !ok {
				continue
			}

			// 寻找注册表数据
			registerItems, ok := dataItem["registerItem"].([]interface{})
			if !ok {
				continue
			}

			for _, regItemRaw := range registerItems {
				regItem, ok := regItemRaw.(map[string]interface{})
				if !ok {
					continue
				}

				// 提取目标字段
				data, _ := regItem["data"].(string)
				registerName, _ := regItem["registerName"].(string)
				unit, _ := regItem["unit"].(string)

				// 按配置过滤设备（只保存配置里声明的因子）
				shouldInclude := false
				for _, devCfg := range cfg.Devices {
					if devCfg.DeviceAddr == int(deviceAddr) &&
						devCfg.FactorName == registerName {
						shouldInclude = true
						break
					}
				}
				if !shouldInclude {
					continue
				}

				// 构建 payload
				payload := map[string]interface{}{
					"device_addr":   int64(deviceAddr),
					"device_name":   deviceName,
					"register_name": registerName,
					"data":          data,
					"value":         regItem["value"],
					"alarm_level":   regItem["alarmLevel"],
					"alarm_info":    regItem["alarmInfo"],
					"unit":          unit,
					"device_status": item["deviceStatus"],
				}
				payloadBytes, _ := json.Marshal(payload)

				// 计算时间戳
				ts := time.Now()
				if timeStampMs > 0 {
					ts = time.UnixMilli(int64(timeStampMs))
				}

				msg := &base.DeviceData{
					DeviceID:    deviceID,
					UniqueID:    uniqueID,
					DeviceType:  "HUANJING_JIANKONG_V3",
					DataType:    "HUANJING_REALTIME",
					Timestamp:   ts,
					Payload:     payloadBytes,
					ComponyName: cfg.ComponyName,
				}

				select {
				case out <- msg:
					log.Printf("[HUANJING] data sent: device=%s factor=%s value=%s",
						deviceID, registerName, data)
				case <-time.After(2 * time.Second):
					log.Printf("[HUANJING] warning: channel busy, dropping data for device=%s", deviceID)
				}
			}
		}
	}
}

func init() {
	plugin.Register("huanjing_jiankong_v3", func() base.IWorker {
		return &Worker{}
	})
}
