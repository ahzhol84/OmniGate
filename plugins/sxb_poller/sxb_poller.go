package sxb

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/auth"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ==================== 配置结构 ====================

// 单个配置项结构
type ConfigItem struct {
	ComponyName  string `json:"compony_name"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	DeviceID     string `json:"device_id"`
	BaseURL      string `json:"base_url"`
	PollInterval int    `json:"poll_interval"` // 轮询间隔秒，默认60

	// 被动接收配置
	ListenAddr string `json:"listen_addr"` // 为空则不开启HTTP服务

	// 新增：鉴权配置（可选，为空则不开启鉴权）
	AuthUsername string `json:"auth_username"` // 默认 admin
	AuthPassword string `json:"auth_password"` // 明文密码
	AuthKey      string `json:"auth_key"`      // HMAC 密钥
}

type SXBWorker struct {
	configs []ConfigItem
	servers []*http.Server
}

type SetNotifyUrl4CReq struct {
	SleepReportNotifyUrl string `json:"sleepReportNotifyUrl"`
	AlarmNotifyUrl       string `json:"alarmNotifyUrl"`
}

type BindUserBodyReq struct {
	DevID    string `json:"devID"`
	Name     string `json:"name"`
	Birthday string `json:"birthday"`
	Gender   string `json:"gender"`
	Height   int    `json:"height"`
	Weight   int    `json:"weight"`
}

type GetDeviceNetworkStateReq struct {
	DevID []string `json:"devID"`
}

type SetDeviceSettingsReq struct {
	DevID                       string `json:"devID"`
	EnableHrThresholdAutoAdjust bool   `json:"enableHrThresholdAutoAdjust"`
	EnableSleepReportAssign     bool   `json:"enableSleepReportAssign"`
	SleepReportGenerateTime     string `json:"sleepReportGenerateTime"`
}

type ApifoxModel struct {
	// 睡眠报告通知接收地址，多个用逗号分隔
	ReportNotifyURL string `json:"reportNotifyUrl"`
}

// ==================== 签名生成（复用核心） ====================

func (w *SXBWorker) generateSignParams(clientSecret, clientID string) string {
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	nonce := uuid.New().String()

	mac := hmac.New(sha1.New, []byte(clientSecret))
	mac.Write([]byte(nonce))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	params := map[string]string{
		"format":           "JSON",
		"version":          "2023-10-18",
		"signatureMethod":  "HMAC-SHA1",
		"signatureVersion": "1.0",
		"signatureNonce":   nonce,
		"timestamp":        timestamp,
		"clientID":         clientID,
		"signature":        signature,
	}

	var keys []string
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString("&")
		}
		sb.WriteString(k + "=" + url.QueryEscape(params[k]))
	}
	return sb.String()
}

// ==================== HTTP 客户端（主动轮询用） ====================

func (w *SXBWorker) doGet(config ConfigItem, endpoint string) ([]byte, error) {
	signParams := w.generateSignParams(config.ClientSecret, config.ClientID)
	fullURL := fmt.Sprintf("%s%s?devID=%s&%s", config.BaseURL, endpoint, config.DeviceID, signParams)

	resp, err := http.Get(fullURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (w *SXBWorker) doPost(config ConfigItem, endpoint string, body interface{}) ([]byte, error) {
	signParams := w.generateSignParams(config.ClientSecret, config.ClientID)
	fullURL := fmt.Sprintf("%s%s?%s", config.BaseURL, endpoint, signParams)

	var bodyReader io.Reader
	if body != nil {
		jsonBytes, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(jsonBytes)
	}

	req, err := http.NewRequest(http.MethodPost, fullURL, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// 业务接口-------------------------------------------------------------------------------------------------------
func (w *SXBWorker) GetDeviceCurrentState(config ConfigItem) ([]byte, error) { // 获取设备当前状态
	return w.doGet(config, "/device/getDeviceCurrentState")
}

func (w *SXBWorker) GetDeviceInfo(config ConfigItem) ([]byte, error) { // 获取设备信息
	return w.doGet(config, "/device/getDeviceInfo")
}

func (w *SXBWorker) GetUserBodyInfo(config ConfigItem) ([]byte, error) { // 获取用户体信息
	return w.doGet(config, "/user/getUserBodyInfo")
}

func (w *SXBWorker) GetClientNotifyUrl(config ConfigItem) ([]byte, error) { // 获取设备通知地址
	return w.doGet(config, "/config/getClientNotifyUrl")
}

func (w *SXBWorker) GetAlarmSetting(config ConfigItem) ([]byte, error) { // 获取报警设置
	return w.doGet(config, "/alarm/getAlarmSetting")
}

func (w *SXBWorker) Unbind(config ConfigItem) ([]byte, error) { // 解除绑定
	return w.doGet(config, "/device/unbind")
}

func (w *SXBWorker) BindUserBodyInfo(config ConfigItem, req *BindUserBodyReq) ([]byte, error) { // 绑定用户体信息
	return w.doPost(config, "/user/bindUserBodyInfo", req)
}

func (w *SXBWorker) SetNotifyUrl4C(config ConfigItem, req *SetNotifyUrl4CReq) ([]byte, error) { // 设置通知地址
	return w.doPost(config, "/config/setNotifyUrl4C", req)
}

func (w *SXBWorker) GetDeviceNetworkState(config ConfigItem, req *GetDeviceNetworkStateReq) ([]byte, error) {
	return w.doPost(config, "/device/getDeviceNetworkState", req)
}

func (w *SXBWorker) GetDeviceWorkState(config ConfigItem, req *GetDeviceNetworkStateReq) ([]byte, error) {
	return w.doPost(config, "/device/getDeviceWorkState", req)
}

func (w *SXBWorker) GetDeviceUsage(config ConfigItem, req *GetDeviceNetworkStateReq) ([]byte, error) {
	return w.doPost(config, "/api/device/getDeviceUsage", req)
}

func (w *SXBWorker) SetDeviceSettings(config ConfigItem, req *SetDeviceSettingsReq) ([]byte, error) {
	return w.doPost(config, "/config/device/save", req)
}

func (w *SXBWorker) setSettings(config ConfigItem, req *ApifoxModel) ([]byte, error) {
	return w.doPost(config, "/config/setReportNotifyUrl", req)
}

// ==================== 主动轮询逻辑 ====================

func (w *SXBWorker) startPoller(ctx context.Context, out chan<- *base.DeviceData, config ConfigItem, index int) {
	if config.PollInterval <= 0 {
		config.PollInterval = 60
	}
	ticker := time.NewTicker(time.Duration(config.PollInterval) * time.Second)
	defer ticker.Stop()

	// 立即执行一次
	w.pollOnce(out, config, index)

	for {
		select {
		case <-ticker.C:
			w.pollOnce(out, config, index)
		case <-ctx.Done():
			log.Printf("[SXB-POLLER-%d] 轮询服务已停止", index+1)
			return
		}
	}
}

func (w *SXBWorker) pollOnce(out chan<- *base.DeviceData, config ConfigItem, index int) {
	// 辅助函数：包装调用并自动记录来源
	call := func(fn func() ([]byte, error), dataType string) {
		resp, err := fn()
		if err != nil {
			log.Printf("[SXB-POLLER-%d] %s 失败: %v", index+1, dataType, err)
			return
		}

		data := &base.DeviceData{
			DeviceID:    config.DeviceID,
			DeviceType:  "SXB_SLEEP_MONITOR",
			DataType:    dataType, // ← 自动填充
			Timestamp:   time.Now(),
			Payload:     resp,
			ComponyName: config.ComponyName,
		}

		select {
		case out <- data:
			log.Printf("[SXB-Poller] %s 数据已推送, size=%d", dataType, len(resp))
		case <-time.After(2 * time.Second):
			log.Printf("[SXB-Poller] %s 通道拥挤，丢弃数据", dataType)
		}
	}

	call(func() ([]byte, error) { return w.GetDeviceCurrentState(config) }, "GetDeviceCurrentState")
	call(func() ([]byte, error) { return w.GetDeviceInfo(config) }, "GetDeviceInfo")
	call(func() ([]byte, error) { return w.GetClientNotifyUrl(config) }, "GetClientNotifyUrl")
	call(func() ([]byte, error) { return w.GetUserBodyInfo(config) }, "GetUserBodyInfo")
	call(func() ([]byte, error) { return w.GetAlarmSetting(config) }, "GetAlarmSetting")
}

// ==================== 被动HTTP服务（等对方POST推送） ====================

func (w *SXBWorker) startListener(ctx context.Context, out chan<- *base.DeviceData, config ConfigItem, index int) {
	mux := http.NewServeMux()

	// 初始化鉴权器（如果配置了鉴权参数）
	var validator *auth.Validator
	var authEnabled bool

	if config.AuthPassword != "" && config.AuthKey != "" {
		var err error
		validator, err = auth.NewValidator(auth.AuthConfig{
			Username: config.AuthUsername,
			Password: config.AuthPassword,
			AuthKey:  config.AuthKey,
		}, common.RDB, fmt.Sprintf("sxb-%d", index+1))

		if err != nil {
			log.Printf("[SXB-LISTENER-%d] 鉴权器初始化失败: %v", index+1, err)
			// 鉴权配置错误，直接退出该监听服务
			return
		}
		authEnabled = true

		// 注册登录接口
		mux.HandleFunc("/login", validator.LoginHandler())
		log.Printf("[SXB-LISTENER-%d] 鉴权已启用，访问 /login 获取 token", index+1)
	}

	// 路由：GetAlarmSetting - 获取报警设置（支持鉴权）
	getAlarmHandler := func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			res.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		resp, err := w.GetAlarmSetting(config)
		if err != nil {
			log.Printf("[SXB-Listener] GetAlarmSetting 失败: %v", err)
			res.WriteHeader(http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		res.Write(resp)
		log.Printf("[SXB-Listener] GetAlarmSetting 已返回: %s", string(resp))
	}

	// 根据是否启用鉴权包装 handler
	if authEnabled {
		mux.HandleFunc("/GetAlarmSetting", validator.AuthMiddleware(getAlarmHandler))
	} else {
		mux.HandleFunc("/GetAlarmSetting", getAlarmHandler)
	}

	// 路由：GetUserBodyInfo - 获取用户体信息（支持鉴权）
	getUserBodyHandler := func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			res.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		resp, err := w.GetUserBodyInfo(config)
		if err != nil {
			log.Printf("[SXB-Listener] GetUserBodyInfo 失败: %v", err)
			res.WriteHeader(http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		res.Write(resp)
		log.Printf("[SXB-Listener] GetUserBodyInfo 已返回: %s", string(resp))
	}

	if authEnabled {
		mux.HandleFunc("/GetUserBodyInfo", validator.AuthMiddleware(getUserBodyHandler))
	} else {
		mux.HandleFunc("/GetUserBodyInfo", getUserBodyHandler)
	}

	// 路由：BindUserBodyInfo - 绑定用户体信息（POST，从 body 取参数）
	bindUserHandler := func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(res, `{"code":-1,"msg":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// 从请求体解析参数
		var bodyReq BindUserBodyReq
		if err := json.NewDecoder(req.Body).Decode(&bodyReq); err != nil {
			http.Error(res, `{"code":-1,"msg":"invalid json"}`, http.StatusBadRequest)
			return
		}
		defer req.Body.Close()

		// 如果请求体没传 DevID，使用配置中的 DeviceID
		if bodyReq.DevID == "" {
			bodyReq.DevID = config.DeviceID
		}

		resp, err := w.BindUserBodyInfo(config, &bodyReq)
		if err != nil {
			log.Printf("[SXB-Listener] BindUserBodyInfo 失败: %v", err)
			http.Error(res, `{"code":-1,"msg":"upstream error"}`, http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		res.Write(resp)
		log.Printf("[SXB-Listener] BindUserBodyInfo 已处理: devID=%s", bodyReq.DevID)
	}

	if authEnabled {
		mux.HandleFunc("/BindUserBodyInfo", validator.AuthMiddleware(bindUserHandler))
	} else {
		mux.HandleFunc("/BindUserBodyInfo", bindUserHandler)
	}

	// 路由：SetNotifyUrl4C - 设置通知地址（POST，从 body 取参数）
	setNotifyHandler := func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(res, `{"code":-1,"msg":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var bodyReq SetNotifyUrl4CReq
		if err := json.NewDecoder(req.Body).Decode(&bodyReq); err != nil {
			http.Error(res, `{"code":-1,"msg":"invalid json"}`, http.StatusBadRequest)
			return
		}
		defer req.Body.Close()

		resp, err := w.SetNotifyUrl4C(config, &bodyReq)
		if err != nil {
			log.Printf("[SXB-Listener] SetNotifyUrl4C 失败: %v", err)
			http.Error(res, `{"code":-1,"msg":"upstream error"}`, http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		res.Write(resp)
		log.Printf("[SXB-Listener] SetNotifyUrl4C 已处理")
	}

	if authEnabled {
		mux.HandleFunc("/SetNotifyUrl4C", validator.AuthMiddleware(setNotifyHandler))
	} else {
		mux.HandleFunc("/SetNotifyUrl4C", setNotifyHandler)
	}

	// 路由：SetDeviceSettings - 设置设备参数（POST，从 body 取参数）
	setSettingsHandler := func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(res, `{"code":-1,"msg":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var bodyReq SetDeviceSettingsReq
		if err := json.NewDecoder(req.Body).Decode(&bodyReq); err != nil {
			http.Error(res, `{"code":-1,"msg":"invalid json"}`, http.StatusBadRequest)
			return
		}
		defer req.Body.Close()

		// 如果请求体没传 DevID，使用配置中的 DeviceID
		if bodyReq.DevID == "" {
			bodyReq.DevID = config.DeviceID
		}

		resp, err := w.SetDeviceSettings(config, &bodyReq)
		if err != nil {
			log.Printf("[SXB-Listener] SetDeviceSettings 失败: %v", err)
			http.Error(res, `{"code":-1,"msg":"upstream error"}`, http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		res.Write(resp)
		log.Printf("[SXB-Listener] SetDeviceSettings 已处理: devID=%s", bodyReq.DevID)
	}

	if authEnabled {
		mux.HandleFunc("/SetDeviceSettings", validator.AuthMiddleware(setSettingsHandler))
	} else {
		mux.HandleFunc("/SetDeviceSettings", setSettingsHandler)
	}

	// 路由：GetDeviceNetworkState - 批量获取设备网络状态（POST，从 body 取 devID 数组）
	getNetworkStateHandler := func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(res, `{"code":-1,"msg":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var bodyReq GetDeviceNetworkStateReq
		if err := json.NewDecoder(req.Body).Decode(&bodyReq); err != nil {
			http.Error(res, `{"code":-1,"msg":"invalid json"}`, http.StatusBadRequest)
			return
		}
		defer req.Body.Close()

		// 如果请求体没传 DevID 或为空数组，使用配置中的 DeviceID
		if len(bodyReq.DevID) == 0 {
			bodyReq.DevID = []string{config.DeviceID}
		}

		resp, err := w.GetDeviceNetworkState(config, &bodyReq)
		if err != nil {
			log.Printf("[SXB-Listener] GetDeviceNetworkState 失败: %v", err)
			http.Error(res, `{"code":-1,"msg":"upstream error"}`, http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		res.Write(resp)
		log.Printf("[SXB-Listener] GetDeviceNetworkState 已处理: devIDs=%v", bodyReq.DevID)
	}

	if authEnabled {
		mux.HandleFunc("/GetDeviceNetworkState", validator.AuthMiddleware(getNetworkStateHandler))
	} else {
		mux.HandleFunc("/GetDeviceNetworkState", getNetworkStateHandler)
	}

	// 路由：GetDeviceWorkState - 批量获取设备工作状态（POST，从 body 取 devID 数组）
	getWorkStateHandler := func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(res, `{"code":-1,"msg":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var bodyReq GetDeviceNetworkStateReq
		if err := json.NewDecoder(req.Body).Decode(&bodyReq); err != nil {
			http.Error(res, `{"code":-1,"msg":"invalid json"}`, http.StatusBadRequest)
			return
		}
		defer req.Body.Close()

		if len(bodyReq.DevID) == 0 {
			bodyReq.DevID = []string{config.DeviceID}
		}

		resp, err := w.GetDeviceWorkState(config, &bodyReq)
		if err != nil {
			log.Printf("[SXB-Listener] GetDeviceWorkState 失败: %v", err)
			http.Error(res, `{"code":-1,"msg":"upstream error"}`, http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		res.Write(resp)
		log.Printf("[SXB-Listener] GetDeviceWorkState 已处理: devIDs=%v", bodyReq.DevID)
	}

	if authEnabled {
		mux.HandleFunc("/GetDeviceWorkState", validator.AuthMiddleware(getWorkStateHandler))
	} else {
		mux.HandleFunc("/GetDeviceWorkState", getWorkStateHandler)
	}

	// 路由：GetDeviceUsage - 批量获取设备使用情况（POST，从 body 取 devID 数组）
	getUsageHandler := func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(res, `{"code":-1,"msg":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var bodyReq GetDeviceNetworkStateReq
		if err := json.NewDecoder(req.Body).Decode(&bodyReq); err != nil {
			http.Error(res, `{"code":-1,"msg":"invalid json"}`, http.StatusBadRequest)
			return
		}
		defer req.Body.Close()

		if len(bodyReq.DevID) == 0 {
			bodyReq.DevID = []string{config.DeviceID}
		}

		resp, err := w.GetDeviceUsage(config, &bodyReq)
		if err != nil {
			log.Printf("[SXB-Listener] GetDeviceUsage 失败: %v", err)
			http.Error(res, `{"code":-1,"msg":"upstream error"}`, http.StatusInternalServerError)
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		res.Write(resp)
		log.Printf("[SXB-Listener] GetDeviceUsage 已处理: devIDs=%v", bodyReq.DevID)
	}

	if authEnabled {
		mux.HandleFunc("/GetDeviceUsage", validator.AuthMiddleware(getUsageHandler))
	} else {
		mux.HandleFunc("/GetDeviceUsage", getUsageHandler)
	}

	// 路由：设备推送数据接收（原始 POST 推送，不鉴权，保持兼容）
	mux.HandleFunc("/sleep", func(res http.ResponseWriter, req *http.Request) {
		w.handlePush(res, req, out, config, "SLEEP_REPORT")
	})

	mux.HandleFunc("/alarm", func(res http.ResponseWriter, req *http.Request) {
		w.handlePush(res, req, out, config, "ALARM")
	})

	server := &http.Server{
		Addr:    config.ListenAddr,
		Handler: mux,
	}
	w.servers = append(w.servers, server)

	go func() {
		log.Printf("[SXB-LISTENER-%d] 推送接收服务启动: %s (鉴权=%v)", index+1, config.ListenAddr, authEnabled)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[SXB-LISTENER-%d] 服务异常: %v", index+1, err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdownCtx)
	log.Printf("[SXB-LISTENER-%d] 服务已关闭", index+1)
}

// handlePush 处理设备主动推送的数据
func (w *SXBWorker) handlePush(res http.ResponseWriter, req *http.Request, out chan<- *base.DeviceData, config ConfigItem, pushType string) {
	if req.Method != http.MethodPost {
		res.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, _ := io.ReadAll(req.Body)
	defer req.Body.Close()

	data := &base.DeviceData{
		DeviceID:    config.DeviceID,
		DeviceType:  "SXB_SLEEP_PUSH",
		DataType:    pushType,
		Timestamp:   time.Now(),
		Payload:     body,
		ComponyName: config.ComponyName,
	}

	select {
	case out <- data:
		res.WriteHeader(http.StatusOK)
		res.Write([]byte(`{"code":0,"msg":"ok"}`))
		log.Printf("[SXB-Push] 收到 %s: %s, size=%d bytes", pushType, req.RemoteAddr, len(body))
	case <-time.After(2 * time.Second):
		res.WriteHeader(http.StatusServiceUnavailable)
		res.Write([]byte(`{"code":-1,"msg":"busy"}`))
		log.Printf("[SXB-Push] 通道拥挤")
	}
}

// ==================== Worker 生命周期 ====================

func (w *SXBWorker) Init(configs []json.RawMessage) error {
	log.Printf("[SXB] 开始初始化，配置项数量: %d", len(configs))

	for i, config := range configs {
		var item ConfigItem
		if err := json.Unmarshal(config, &item); err != nil {
			return fmt.Errorf("第%d个配置解析失败: %v", i+1, err)
		}

		if item.BaseURL == "" {
			item.BaseURL = "https://openapi.sleepthing.com"
		}

		w.configs = append(w.configs, item)
		log.Printf("[SXB] 配置%d: 公司=%s, 设备=%s, 轮询间隔=%ds, 监听地址=%s, 鉴权=%v",
			i+1, item.ComponyName, item.DeviceID, item.PollInterval, item.ListenAddr,
			item.AuthPassword != "" && item.AuthKey != "")
	}

	return nil
}

func (w *SXBWorker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	var wg sync.WaitGroup
	serviceCount := 0

	log.Printf("[SXB] 启动插件，开始初始化服务...")

	// 为每个配置项启动相应的服务
	for i, config := range w.configs {
		// 模式1: 主动轮询（配置 poll_interval > 0 时启用）
		if config.PollInterval > 0 {
			wg.Add(1)
			serviceCount++
			go func(idx int, cfg ConfigItem) {
				defer wg.Done()
				w.startPoller(ctx, out, cfg, idx)
			}(i, config)
		}

		// 模式2: 被动监听（配置 listen_addr 时启用）
		if config.ListenAddr != "" {
			wg.Add(1)
			serviceCount++
			go func(idx int, cfg ConfigItem) {
				defer wg.Done()
				w.startListener(ctx, out, cfg, idx)
			}(i, config)
		}
	}

	log.Printf("[SXB] 共启动 %d 个服务实例", serviceCount)

	// 等待所有服务结束
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		log.Println("[SXB] 收到停止信号，正在关闭所有服务...")
		// 关闭所有HTTP服务
		for _, server := range w.servers {
			if server != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				server.Shutdown(shutdownCtx)
				cancel()
			}
		}
		<-done
	case <-done:
		log.Println("[SXB] 所有服务已正常结束")
	}
}

func init() {
	plugin.Register("sxb", func() base.IWorker {
		return &SXBWorker{}
	})
}
