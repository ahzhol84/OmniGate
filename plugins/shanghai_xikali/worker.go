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

	ComponyName  string `json:"compony_name"`
	DeviceType   string `json:"device_type"`
	DataType     string `json:"data_type"`
	UniquePrefix string `json:"unique_prefix"`
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
					log.Printf("[XIKALI-%d] PANIC recover: %v", idx, r)
				}
				w.wg.Done()
			}()
			w.runConsumer(c, out, idx, manager)
		}(i, mgr, childCtx)
	}
	<-ctx.Done()
	log.Printf("[XIKALI] context cancelled, closing connections...")
	for i, mgr := range w.managers {
		if w.cancelFuncs[i] != nil {
			w.cancelFuncs[i]()
		}
		if mgr != nil {
			mgr.close()
		}
	}
	log.Printf("[XIKALI] waiting for consumer goroutines (max 10s)...")
	waitCh := make(chan struct{})
	go func() {
		w.wg.Wait()
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
			log.Printf("%s context done, exiting consumer", pluginTag)
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
			log.Printf("%s context done, exiting receive loop", pluginTag)
			return
		default:
		}
		msg, err := m.receiver.Receive(ctx)
		if err != nil {
			log.Printf("%s receive error: %v", pluginTag, err)
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
	// log.Printf("%s recv topic=%s payload=%s", pluginTag, topic, payload)
	var rawObj map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &rawObj); err != nil {
		log.Printf("%s json parse fail: %v", pluginTag, err)
		m.sendToChannel(out, deviceDataFromRaw(cfg, topic, payload, ""), pluginTag)
		return
	}
	deviceID := extractDeviceID(rawObj)
	if deviceID == "" {
		deviceID = extractDeviceIDFromTopic(topic)
	}
	if deviceID == "" {
		deviceID = "UNKNOWN"
	}
	nickname := extractNickname(rawObj)
	// 设备过滤：只放行指定 nickname 的设备
	allowedNicknames := map[string]bool{
		"X1LTE_S01B03N127": true,
	}
	if nickname != "" && !allowedNicknames[nickname] {
		// log.Printf("%s SKIP device=%s nickname=%s (filtered)", pluginTag, deviceID, nickname)
		return
	}
	flatPayload := flattenAliYunItems(rawObj)
	if flatPayload == nil {
		m.sendToChannel(out, deviceDataFromRaw(cfg, topic, payload, deviceID), pluginTag)
		return
	}
	flatPayload["_topic"] = topic
	if method, ok := rawObj["method"]; ok {
		flatPayload["_method"] = method
	}
	if id, ok := rawObj["id"]; ok {
		flatPayload["_id"] = id
	}
	if version, ok := rawObj["version"]; ok {
		flatPayload["_version"] = version
	}
	uniqueID := common.ResolveDeviceUniqueID(
		deviceID, cfg.ComponyName, "shanghai_xikali", cfg.DeviceType, deviceID,
	)
	payloadBytes, err := json.Marshal(flatPayload)
	if err != nil {
		log.Printf("%s marshal fail: %v", pluginTag, err)
		m.sendToChannel(out, deviceDataFromRaw(cfg, topic, payload, deviceID), pluginTag)
		return
	}
	deviceType := strings.TrimSpace(cfg.DeviceType)
	dataType := buildDataType(cfg.DataType, rawObj)
	data := &base.DeviceData{
		DeviceID: deviceID, UniqueID: uniqueID, DeviceType: deviceType,
		DataType: dataType, Timestamp: time.Now(), Payload: payloadBytes,
		ComponyName: cfg.ComponyName,
	}
	if deviceID != "" && deviceID != "UNKNOWN" {
		if err := common.UpsertDeviceIDMapping("shanghai_xikali", uniqueID, deviceID,
			cfg.ComponyName, deviceType, dataType); err != nil {
			log.Printf("%s mapping upsert err unique=%s dev=%s err=%v", pluginTag, uniqueID, deviceID, err)
		}
	}
	log.Printf("%s OK device=%s nickname=%s unique=%s", pluginTag, deviceID, nickname, uniqueID)
	select {
	case out <- data:
	case <-time.After(3 * time.Second):
		log.Printf("%s channel busy, drop device=%s nickname=%s", pluginTag, deviceID, nickname)
	}
}

func (m *amqpManager) sendToChannel(out chan<- *base.DeviceData, data *base.DeviceData, pluginTag string) {
	if data == nil {
		return
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if m.receiver != nil {
		_ = m.receiver.Close(ctx)
		m.receiver = nil
	}
	if m.session != nil {
		_ = m.session.Close(ctx)
		m.session = nil
	}
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
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

func init() {
	plugin.Register("shanghai_xikali", func() base.IWorker {
		return &Worker{}
	})
}
