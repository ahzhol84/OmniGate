package shanghai_xikali

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	"pack.ag/amqp"
)

// ============================================================
// 上海希卡利（ShangHai XiKaLi）AMQP 消费插件
// 阿里云物联网平台 AMQP 服务端订阅
// ============================================================

// ConfigItem 单条 AMQP 连接配置。
type ConfigItem struct {
	Host               string `json:"host"`
	Port               int    `json:"port"`
	IoTInstanceID      string `json:"iot_instance_id"`
	ConsumerGroupID    string `json:"consumer_group_id"`
	ClientID           string `json:"client_id"`
	SignMethod         string `json:"sign_method"`
	AccessKey          string `json:"access_key"`
	AccessSecret       string `json:"access_secret"`
	LinkCredit         uint32 `json:"link_credit"`
	ReconnectMaxWait   int    `json:"reconnect_max_wait"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`

	ComponyName      string   `json:"compony_name"`
	DeviceType       string   `json:"device_type"`
	DataType         string   `json:"data_type"`
	UniquePrefix     string   `json:"unique_prefix"`
	BlockedNicknames []string `json:"blocked_nicknames"`
}

// Worker 是上海希卡利 AMQP 消费插件主体。
type Worker struct {
	configs     []ConfigItem
	managers    []*amqpManager
	cancelFuncs []context.CancelFunc
	wg          sync.WaitGroup
}

// amqpManager 管理单条 AMQP 连接的声明周期。
type amqpManager struct {
	cfg      ConfigItem
	address  string
	userName string
	password string
	client   *amqp.Client
	session  *amqp.Session
	receiver *amqp.Receiver
	mu       sync.Mutex
}

// Init 解析并校验配置。
func (w *Worker) Init(configs []json.RawMessage) error {
	if len(configs) == 0 {
		return fmt.Errorf("empty configs")
	}
	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d] parse failed: %w", i, err)
		}
		if strings.TrimSpace(cfg.Host) == "" {
			return fmt.Errorf("config[%d] host is required", i)
		}
		if cfg.Port <= 0 {
			cfg.Port = 5671
		}
		if strings.TrimSpace(cfg.IoTInstanceID) == "" {
			return fmt.Errorf("config[%d] iot_instance_id is required", i)
		}
		if strings.TrimSpace(cfg.ConsumerGroupID) == "" {
			return fmt.Errorf("config[%d] consumer_group_id is required", i)
		}
		if strings.TrimSpace(cfg.ClientID) == "" {
			cfg.ClientID = fmt.Sprintf("go-amqp-%s-%d", cfg.IoTInstanceID, i)
		}
		if strings.TrimSpace(cfg.SignMethod) == "" {
			cfg.SignMethod = "hmacsha1"
		}
		if cfg.LinkCredit <= 0 {
			cfg.LinkCredit = 200
		}
		if cfg.ReconnectMaxWait <= 0 {
			cfg.ReconnectMaxWait = 20
		}
		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "SHANGHAI_XIKALI"
		}
		if strings.TrimSpace(cfg.DataType) == "" {
			cfg.DataType = "ALIYUN_AMQP"
		}
		if strings.TrimSpace(cfg.UniquePrefix) == "" {
			cfg.UniquePrefix = strings.TrimSpace(cfg.ComponyName)
		}
		w.configs = append(w.configs, cfg)
		log.Printf("[XIKALI] config[%d] host=%s:%d instance=%s group=%s client=%s",
			i, cfg.Host, cfg.Port, cfg.IoTInstanceID, cfg.ConsumerGroupID, cfg.ClientID)
	}
	return nil
}

// Start 启动所有 AMQP 消费连接。
func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	if len(w.configs) == 0 {
		log.Printf("[XIKALI] skip start: no configs")
		return
	}
	w.managers = make([]*amqpManager, len(w.configs))
	w.cancelFuncs = make([]context.CancelFunc, len(w.configs))
	for i, cfg := range w.configs {
		childCtx, cancel := context.WithCancel(ctx)
		w.cancelFuncs[i] = cancel
		mgr := w.buildManager(cfg)
		w.managers[i] = mgr
		w.wg.Add(1)
		go func(idx int, manager *amqpManager, c context.Context) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[XIKALI-%d] PANIC recover: %v\nstacktrace:\n%s", idx, r, getStack())
				}
				log.Printf("[XIKALI-%d] consumer goroutine exiting, wg.Done() called", idx)
				w.wg.Done()
			}()
			w.runConsumer(c, out, idx, manager)
		}(i, mgr, childCtx)
	}
	<-ctx.Done()
	log.Printf("[XIKALI] context cancelled (%d managers), closing connections...", len(w.managers))
	for i, mgr := range w.managers {
		log.Printf("[XIKALI] closing manager[%d]...", i)
		if w.cancelFuncs[i] != nil {
			w.cancelFuncs[i]()
			log.Printf("[XIKALI] cancelFunc[%d] called", i)
		}
		if mgr != nil {
			mgr.close()
			log.Printf("[XIKALI] manager[%d] close() returned", i)
		}
	}
	log.Printf("[XIKALI] waiting for consumer goroutines (max 10s), wg=%d...", len(w.configs))
	waitCh := make(chan struct{})
	go func() {
		log.Printf("[XIKALI] wg.Wait() starting...")
		w.wg.Wait()
		log.Printf("[XIKALI] wg.Wait() completed")
		close(waitCh)
	}()
	select {
	case <-waitCh:
		log.Printf("[XIKALI] all connections closed gracefully")
	case <-time.After(10 * time.Second):
		log.Printf("[XIKALI] WARN: timeout waiting for goroutines, forcing exit")
	}
}

func (w *Worker) buildManager(cfg ConfigItem) *amqpManager {
	timestamp := time.Now().UnixMilli()
	userName := fmt.Sprintf("%s|authMode=aksign,signMethod=%s,consumerGroupId=%s,authId=%s,iotInstanceId=%s,timestamp=%d|",
		strings.TrimSpace(cfg.ClientID),
		strings.TrimSpace(cfg.SignMethod),
		strings.TrimSpace(cfg.ConsumerGroupID),
		strings.TrimSpace(cfg.AccessKey),
		strings.TrimSpace(cfg.IoTInstanceID),
		timestamp,
	)
	stringToSign := fmt.Sprintf("authId=%s&timestamp=%d",
		strings.TrimSpace(cfg.AccessKey), timestamp)
	mac := hmac.New(sha1.New, []byte(strings.TrimSpace(cfg.AccessSecret)))
	mac.Write([]byte(stringToSign))
	password := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	address := fmt.Sprintf("amqps://%s:%d",
		strings.TrimSpace(cfg.Host), cfg.Port)
	return &amqpManager{
		cfg: cfg, address: address, userName: userName, password: password,
	}
}

func (w *Worker) runConsumer(ctx context.Context, out chan<- *base.DeviceData, index int, mgr *amqpManager) {
	cfg := mgr.cfg
	pluginTag := fmt.Sprintf("[XIKALI-%d]", index)
	log.Printf("%s starting consumer host=%s:%d", pluginTag, cfg.Host, cfg.Port)
	for {
		select {
		case <-ctx.Done():
			log.Printf("%s context done (ctx.Err=%v), exiting consumer", pluginTag, ctx.Err())
			return
		default:
		}
		if err := mgr.connect(ctx); err != nil {
			log.Printf("%s connect failed: %v", pluginTag, err)
			mgr.backoffRetry(ctx, pluginTag)
			continue
		}
		log.Printf("%s AMQP connection established, starting receive loop", pluginTag)
		mgr.receiveLoop(ctx, out, index, pluginTag)
		log.Printf("%s receiveLoop returned, will reconnect if context not done", pluginTag)
	}
}

func (m *amqpManager) connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeLocked()
	tlsConfig := &tls.Config{InsecureSkipVerify: m.cfg.InsecureSkipVerify}
	client, err := amqp.Dial(m.address,
		amqp.ConnSASLPlain(m.userName, m.password),
		amqp.ConnTLSConfig(tlsConfig),
		amqp.ConnConnectTimeout(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	m.client = client
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return fmt.Errorf("new session failed: %w", err)
	}
	m.session = session
	receiver, err := session.NewReceiver(
		amqp.LinkSourceAddress("/queue-name"),
		amqp.LinkCredit(m.cfg.LinkCredit),
	)
	if err != nil {
		_ = session.Close(ctx)
		_ = client.Close()
		return fmt.Errorf("new receiver failed: %w", err)
	}
	m.receiver = receiver
	return nil
}

func (m *amqpManager) receiveLoop(ctx context.Context, out chan<- *base.DeviceData, index int, pluginTag string) {
	cfg := m.cfg
	for {
		select {
		case <-ctx.Done():
			log.Printf("%s context done (ctx.Err=%v), exiting receive loop", pluginTag, ctx.Err())
			return
		default:
		}
		msg, err := m.receiver.Receive(ctx)
		if err != nil {
			log.Printf("%s receive error: %v (ctx.Err=%v)", pluginTag, err, ctx.Err())
			return
		}
		go func(message *amqp.Message) {
			m.processMessage(message, out, index, cfg, pluginTag)
			_ = message.Accept()
		}(msg)
	}
}

func (m *amqpManager) processMessage(msg *amqp.Message, out chan<- *base.DeviceData, index int, cfg ConfigItem, pluginTag string) {
	var topic string
	if msg.Properties != nil {
		topic = msg.Properties.To
	}
	if msg.ApplicationProperties != nil {
		if v, ok := msg.ApplicationProperties["topic"]; ok {
			if s, ok := v.(string); ok {
				topic = s
			}
		}
	}
	payload := string(msg.GetData())
	if strings.TrimSpace(payload) == "" {
		log.Printf("%s empty payload, skip", pluginTag)
		return
	}

	var rawObj map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &rawObj); err != nil {
		log.Printf("%s json parse fail: %v", pluginTag, err)
		m.sendToChannel(out, deviceDataFromRaw(cfg, topic, payload, ""), pluginTag)
		return
	}

	// ============================================================
	// 使用结构化解析器将原始 AMQP JSON 解析为人类可读数据
	// 原始 JSON 不保留，只存解析后的结构化数据
	//
	// 消息分类策略：
	//   - msgDataUpdate（/user/update）→ 结构化解析后广播给 wshub，不落库
	//   - msgWarning（/user/warning）→ 解析成通知结构，广播给 wshub，不落库
	//   - msgUnknown → 回退提取基础信息，可落库
	// ============================================================
	parsed, deviceID, msgCat, isParsed := ParsePayload(rawObj, topic)

	if deviceID == "" {
		deviceID = extractDeviceIDFromTopic(topic)
	}
	if deviceID == "" {
		deviceID = "UNKNOWN"
	}

	// 获取设备备注名用于黑名单判断
	nickname := parsed.DeviceName
	if nickname == "" {
		nickname = extractNickname(rawObj)
	}

	// 黑名单模式：名单中的设备只打印日志，不存储到数据库
	isBlocked := false
	if len(cfg.BlockedNicknames) > 0 && nickname != "" {
		for _, n := range cfg.BlockedNicknames {
			if n == nickname {
				isBlocked = true
				break
			}
		}
	}

	uniqueID := common.ResolveDeviceUniqueID(
		deviceID, cfg.ComponyName, "shanghai_xikali", cfg.DeviceType, deviceID,
	)

	// 将结构化解析结果序列化为 JSON → 作为 Payload 存储
	parsedBytes, err := json.Marshal(parsed)
	if err != nil {
		log.Printf("%s marshal parsed payload fail: %v", pluginTag, err)
		m.sendToChannel(out, deviceDataFromRaw(cfg, topic, payload, deviceID), pluginTag)
		return
	}

	// ============================================================
	// DataType 标记策略：
	//   - 正常数据上报 / 通知消息 → DataType 末尾加 "_NODB" 标记，
	//     上游 DataWriter 检测到此标记跳过落库，但通道照常广播
	//   - 无法识别的消息 → 正常落库
	// ============================================================
	deviceType := strings.TrimSpace(cfg.DeviceType)

	// 计算基础 DataType：优先使用 parsed.Method，其次从 rawObj 取
	baseDataType := cfg.DataType
	if parsed.Method != "" {
		baseDataType = fmt.Sprintf("%s_%s", cfg.DataType, parsed.Method)
	}

	// 高频数据不上报 / 通知消息 / 平台结构化输出 → 加 _NODB 标记让上游跳过落库
	shouldStoreDB := true
	switch msgCat {
	case msgDataUpdate:
		// 数据上报（高频）：必须广播给 wshub，但不需要落库
		shouldStoreDB = false
		baseDataType = baseDataType + "_NODB"
	case msgWarning:
		// 通知消息：广播，不落库
		shouldStoreDB = false
		baseDataType = "NOTIFICATION_NODB"
	case msgPlatformOutput:
		// 平台结构化输出（高频）：广播给 wshub，不落库
		shouldStoreDB = false
		baseDataType = baseDataType + "_NODB"
	}

	data := &base.DeviceData{
		DeviceID:    deviceID,
		UniqueID:    uniqueID,
		DeviceType:  deviceType,
		DataType:    baseDataType,
		Timestamp:   time.Now(),
		Payload:     parsedBytes, // 只存解析后的结构化数据，不存原始 JSON
		ComponyName: cfg.ComponyName,
	}

	if shouldStoreDB && deviceID != "" && deviceID != "UNKNOWN" {
		if err := common.UpsertDeviceIDMapping("shanghai_xikali", uniqueID, deviceID,
			cfg.ComponyName, deviceType, baseDataType); err != nil {
			log.Printf("%s mapping upsert err unique=%s dev=%s err=%v", pluginTag, uniqueID, deviceID, err)
		}
	}

	if isBlocked {
		log.Printf("%s BLOCKED device=%s nickname=%s (blacklisted, print only, not stored)", pluginTag, deviceID, nickname)
	} else {
		storeTag := ""
		if !shouldStoreDB {
			storeTag = " [NODB]"
		}
		if isParsed {
			log.Printf("%s OK%s device=%s type=%s msgCat=%d nickname=%s unique=%s",
				pluginTag, storeTag, deviceID, parsed.DeviceType, msgCat, nickname, uniqueID)
		} else {
			log.Printf("%s OK%s (fallback) device=%s msgCat=%d nickname=%s unique=%s",
				pluginTag, storeTag, deviceID, msgCat, nickname, uniqueID)
		}
		// 使用 recover 防止向已关闭 channel 发送时 panic
		defer func() {
			if r := recover(); r != nil {
				log.Printf("%s PANIC recover at send to out channel: %v\nstacktrace:\n%s", pluginTag, r, getStack())
			}
		}()
		select {
		case out <- data:
		case <-time.After(3 * time.Second):
			log.Printf("%s channel busy, drop device=%s nickname=%s", pluginTag, deviceID, nickname)
		}
	}
}

func (m *amqpManager) sendToChannel(out chan<- *base.DeviceData, data *base.DeviceData, pluginTag string) {
	if data == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s PANIC recover at sendToChannel: %v\nstacktrace:\n%s", pluginTag, r, getStack())
		}
	}()
	select {
	case out <- data:
		log.Printf("%s (raw) sent device=%s", pluginTag, data.DeviceID)
	case <-time.After(3 * time.Second):
		log.Printf("%s channel busy, drop (raw) device=%s", pluginTag, data.DeviceID)
	}
}

func (m *amqpManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeLocked()
}

func (m *amqpManager) closeLocked() {
	log.Printf("[XIKALI] closeLocked: receiver=%v session=%v client=%v",
		m.receiver != nil, m.session != nil, m.client != nil)

	// 每个 close 步骤设置单独的 goroutine + 短超时保护，防止卡死
	closeWithTimeout := func(name string, closeFn func(context.Context) error, closeFnNoCtx func() error) {
		done := make(chan struct{}, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[XIKALI] PANIC in close(%s): %v\nstacktrace:\n%s", name, r, getStack())
				}
				done <- struct{}{}
			}()
			if closeFn != nil {
				// 带 context 的 close
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				if err := closeFn(ctx); err != nil {
					log.Printf("[XIKALI] %s.Close error: %v (ignored during shutdown)", name, err)
				}
			} else if closeFnNoCtx != nil {
				if err := closeFnNoCtx(); err != nil {
					log.Printf("[XIKALI] %s.Close error: %v (ignored during shutdown)", name, err)
				}
			}
		}()
		select {
		case <-done:
			log.Printf("[XIKALI] %s closed", name)
		case <-time.After(3 * time.Second):
			log.Printf("[XIKALI] WARN: %s.Close timed out after 3s, force proceeding", name)
		}
	}

	if m.receiver != nil {
		log.Printf("[XIKALI] closing receiver...")
		r := m.receiver
		m.receiver = nil
		closeWithTimeout("receiver", r.Close, nil)
	}
	if m.session != nil {
		log.Printf("[XIKALI] closing session...")
		s := m.session
		m.session = nil
		closeWithTimeout("session", s.Close, nil)
	}
	if m.client != nil {
		log.Printf("[XIKALI] closing client...")
		c := m.client
		m.client = nil
		closeWithTimeout("client", nil, c.Close)
	}
}

func (m *amqpManager) backoffRetry(ctx context.Context, pluginTag string) {
	maxWait := m.cfg.ReconnectMaxWait
	if maxWait <= 0 {
		maxWait = 20
	}
	duration := 1 * time.Second
	maxDuration := time.Duration(maxWait) * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		log.Printf("%s retry in %v...", pluginTag, duration)
		select {
		case <-ctx.Done():
			return
		case <-time.After(duration):
		}
		if duration < maxDuration {
			duration *= 2
			if duration > maxDuration {
				duration = maxDuration
			}
		}
		if err := m.connect(ctx); err == nil {
			log.Printf("%s reconnected successfully", pluginTag)
			return
		} else {
			log.Printf("%s reconnect failed: %v", pluginTag, err)
		}
	}
}

// ============================================================
// 辅助函数
// ============================================================

func extractDeviceID(obj map[string]interface{}) string {
	items, ok := obj["items"].(map[string]interface{})
	if !ok {
		return ""
	}
	deviceIDField, ok := items["DeviceID"]
	if !ok {
		return ""
	}
	deviceIDObj, ok := deviceIDField.(map[string]interface{})
	if !ok {
		return ""
	}
	value, ok := deviceIDObj["value"]
	if !ok {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

func extractDeviceIDFromTopic(topic string) string {
	parts := strings.Split(strings.TrimSpace(topic), "/")
	if len(parts) >= 3 {
		return strings.TrimSpace(parts[2])
	}
	return ""
}

func extractNickname(obj map[string]interface{}) string {
	items, ok := obj["items"].(map[string]interface{})
	if !ok {
		return ""
	}
	nicknameField, ok := items["Nickname"]
	if !ok {
		return ""
	}
	nicknameObj, ok := nicknameField.(map[string]interface{})
	if !ok {
		return ""
	}
	value, ok := nicknameObj["value"]
	if !ok {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

// flattenAliYunItems 展开阿里云物联网平台标准 items 嵌套结构。
//
// 对于 payload 中包含 items 字段的消息（标准阿里云 AMQP 订阅格式），
// 该函数将 { "DeviceID": { "value": "xxx" }, "Temperature": { "value": 25.5 } }
// 展开为 { "DeviceID": "xxx", "Temperature": 25.5 } 的扁平 map。
//
// 在 processMessage 中已通过 if !hasItems { return } 过滤掉了非 items 格式的消息。
// 如需对非 items 格式也进行存储，可注释 processMessage 中对应的 if 块，
// 并在此函数中补充对应格式的解析逻辑。
func flattenAliYunItems(obj map[string]interface{}) map[string]interface{} {
	items, ok := obj["items"].(map[string]interface{})
	if !ok || len(items) == 0 {
		return nil
	}
	flat := make(map[string]interface{}, len(items))
	for key, val := range items {
		valObj, ok := val.(map[string]interface{})
		if !ok {
			flat[key] = val
			continue
		}
		if v, exists := valObj["value"]; exists {
			switch typed := v.(type) {
			case json.Number:
				if f, err := typed.Float64(); err == nil {
					flat[key] = f
				} else {
					flat[key] = typed.String()
				}
			default:
				flat[key] = v
			}
		} else {
			flat[key] = valObj
		}
	}
	return flat
}

func buildDataType(baseType string, obj map[string]interface{}) string {
	method, _ := obj["method"].(string)
	method = strings.TrimSpace(method)
	if method != "" {
		return fmt.Sprintf("%s_%s", baseType, method)
	}
	return baseType
}

func deviceDataFromRaw(cfg ConfigItem, topic, payload, deviceID string) *base.DeviceData {
	if deviceID == "" {
		deviceID = extractDeviceIDFromTopic(topic)
	}
	if deviceID == "" {
		deviceID = "UNKNOWN"
	}
	uniqueID := common.ResolveDeviceUniqueID(
		deviceID, cfg.ComponyName, "shanghai_xikali", cfg.DeviceType, deviceID,
	)
	return &base.DeviceData{
		DeviceID: deviceID, UniqueID: uniqueID,
		DeviceType: cfg.DeviceType, DataType: cfg.DataType,
		Timestamp: time.Now(), Payload: json.RawMessage(payload),
		ComponyName: cfg.ComponyName,
	}
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// getStack 获取当前 goroutine 的调用栈信息，用于 panic 日志。
func getStack() string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

func init() {
	plugin.Register("shanghai_xikali", func() base.IWorker {
		return &Worker{}
	})
}
