package shanghai_xikali

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const pluginName = "shanghai_xikali"

// ============================================================
// 希卡立（HECARY）API 下行反控实现
//
// 基于 API 文档实现以下接口：
//   - configset (POST 4877): 设置设备参数/预警阈值 ← 核心反控
//   - control_mq (POST 4861): 触发 AMQP 即时计算
//   - configget  (GET  4877): 查询当前设备参数
//   - dailyreport_pure (GET 4859): 查询睡眠日报
//   - sleepstage (GET 4859): 查询睡眠分期
//   - ratedata   (GET 4859): 查心/呼吸率分时数据
//   - fullreport (GET 4859): 统一聚合报告
//   - warning    (GET 4859): 预警消息查询
//   - bedinfo    (POST 4881): 设置床垫参数
//   - pressure   (POST 4881): 压敏初始化
// ============================================================

// HikariApiConfig 表示希卡力 API 认证与寻址参数。
type HikariApiConfig struct {
	AppKey string `json:"appkey"`
	Secret string `json:"secret"`

	// 设备在希卡立侧的 id（纯数字）
	DeviceID string `json:"vendor_device_id"`

	// 专有数据库名
	DB string `json:"db"`

	// 设备所属 productKey
	ProductKey string `json:"product_key"`

	// API 基础地址（含端口），默认 59.110.175.77
	BaseHost string `json:"base_host"`
}

// ============================================================
// SendCommand（下行反控入口）
// ============================================================

func (w *Worker) SendCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || cmd == nil {
		return nil, base.ErrCommandNotSupported
	}

	// 设备认领
	cfg, owned := w.resolveOwnedDevice(deviceID, cmd)
	if !owned {
		return nil, base.ErrCommandNotSupported
	}

	// 从 cmd 参数中提取希卡立 API 鉴权信息
	hecCfg := extractHikariCfg(cmd, cfg)
	if hecCfg.AppKey == "" || hecCfg.Secret == "" {
		return nil, fmt.Errorf("missing hecary api credentials: need appkey+secret in cmd.params or config")
	}
	if hecCfg.DeviceID == "" {
		hecCfg.DeviceID = deviceID
	}

	method := strings.TrimSpace(strings.ToLower(cmd.Method))
	identifier := strings.TrimSpace(cmd.Identifier)

	reply, err := w.dispatchCommand(ctx, method, identifier, cmd.Params, hecCfg)
	if err != nil {
		log.Printf("[XIKALI][SENDCMD] ERROR device=%s method=%s err=%v", deviceID, method, err)
		return &base.CommandReply{
			RequestID: cmd.RequestID,
			Code:      -1,
			Message:   err.Error(),
			Data: map[string]interface{}{
				"plugin":    pluginName,
				"device_id": deviceID,
				"error":     err.Error(),
			},
		}, err
	}

	msgBytes, _ := json.Marshal(reply)
	return &base.CommandReply{
		RequestID: cmd.RequestID,
		Code:      0,
		Message:   string(msgBytes),
		Data:      reply,
	}, nil
}

// dispatchCommand 根据 method 路由到具体 API。
func (w *Worker) dispatchCommand(ctx context.Context, method, identifier string, params map[string]interface{}, hecCfg *HikariApiConfig) (map[string]interface{}, error) {
	switch {
	// ========== 设备参数配置 ==========
	case method == "configset" || method == "set_config":
		return w.callConfigSet(ctx, hecCfg, params)

	case method == "configget" || method == "get_config":
		return w.callConfigGet(ctx, hecCfg)

	// ========== 触发即时计算 ==========
	case method == "control_mq" || method == "trigger_calc":
		return w.callControlMQ(ctx, hecCfg)

	// ========== 睡眠报告查询 ==========
	case method == "dailyreport" || method == "get_dailyreport":
		date := asString(params["date"])
		if date == "" {
			date = time.Now().AddDate(0, 0, -1).Format("2006-1-2")
		}
		return w.callDailyReport(ctx, hecCfg, date)

	case method == "sleepstage" || method == "get_sleepstage":
		date := asString(params["date"])
		if date == "" {
			date = time.Now().AddDate(0, 0, -1).Format("2006-1-2")
		}
		return w.callSleepStage(ctx, hecCfg, date)

	case method == "ratedata" || method == "get_ratedata":
		date := asString(params["date"])
		if date == "" {
			date = time.Now().AddDate(0, 0, -1).Format("2006-1-2")
		}
		return w.callRateData(ctx, hecCfg, date)

	case method == "fullreport" || method == "get_fullreport":
		date := asString(params["date"])
		if date == "" {
			date = time.Now().AddDate(0, 0, -1).Format("2006-1-2")
		}
		target := asString(params["target"])
		if target == "" {
			target = "stra"
		}
		return w.callFullReport(ctx, hecCfg, date, target)

	// ========== 预警查询 ==========
	case method == "warning" || method == "get_warning":
		return w.callWarning(ctx, hecCfg, params)

	// ========== 设置床垫参数 ==========
	case method == "bedinfo" || method == "set_bedinfo":
		return w.callBedInfo(ctx, hecCfg, params)

	// ========== 压敏初始化 ==========
	case method == "pressure" || method == "pressure_init":
		return w.callPressure(ctx, hecCfg)

	default:
		return nil, fmt.Errorf("unsupported method=%q for hecary plugin", method)
	}
}

// ============================================================
// 各 API 调用实现
// ============================================================

// callConfigSet 设置设备参数/预警阈值 (POST :4877/configset)
// content-type: application/x-www-form-urlencoded
func (w *Worker) callConfigSet(ctx context.Context, hecCfg *HikariApiConfig, params map[string]interface{}) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"id":      hecCfg.DeviceID,
		"db":      hecCfg.DB,
		"product": hecCfg.ProductKey,
		"appkey":  hecCfg.AppKey,
	}
	for k, v := range params {
		normalized := strings.ToLower(strings.TrimSpace(k))
		switch normalized {
		case "channel", "device_name", "vendor_device_name",
			"product_key", "access_key", "access_secret",
			"iot_instance_id", "timeout_seconds", "appkey", "secret",
			"db", "id", "product":
			continue
		default:
			body[k] = v
		}
	}
	return w.postForm(ctx, hecCfg, "4877", "/configset", body)
}

// callConfigGet 查询设备参数 (GET :4877/configget)
func (w *Worker) callConfigGet(ctx context.Context, hecCfg *HikariApiConfig) (map[string]interface{}, error) {
	args := map[string]string{
		"db":      hecCfg.DB,
		"id":      hecCfg.DeviceID,
		"product": hecCfg.ProductKey,
	}
	return w.getSigned(ctx, hecCfg, "4877", "/configget", args)
}

// callControlMQ 触发 AMQP 即时计算 (POST :4861/control-mq)
// content-type: application/json
func (w *Worker) callControlMQ(ctx context.Context, hecCfg *HikariApiConfig) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"device_name": hecCfg.DeviceID,
		"appkey":      hecCfg.AppKey,
	}
	return w.postJSON(ctx, hecCfg, "4861", "/control-mq", body)
}

// callDailyReport 查询单日睡眠日报 (GET :4859/dailyreport)
func (w *Worker) callDailyReport(ctx context.Context, hecCfg *HikariApiConfig, date string) (map[string]interface{}, error) {
	args := map[string]string{
		"device_name": hecCfg.DeviceID,
		"date":        date,
		"db":          hecCfg.DB,
	}
	return w.getSigned(ctx, hecCfg, "4859", "/dailyreport", args)
}

// callSleepStage 查询睡眠分期 (GET :4859/sleepstage)
func (w *Worker) callSleepStage(ctx context.Context, hecCfg *HikariApiConfig, date string) (map[string]interface{}, error) {
	args := map[string]string{
		"device_name": hecCfg.DeviceID,
		"date":        date,
		"db":          hecCfg.DB,
	}
	return w.getSigned(ctx, hecCfg, "4859", "/sleepstage", args)
}

// callRateData 查询心率呼吸率分时数据 (GET :4859/ratedata)
func (w *Worker) callRateData(ctx context.Context, hecCfg *HikariApiConfig, date string) (map[string]interface{}, error) {
	args := map[string]string{
		"device_name": hecCfg.DeviceID,
		"date":        date,
		"db":          hecCfg.DB,
	}
	return w.getSigned(ctx, hecCfg, "4859", "/ratedata", args)
}

// callFullReport 统一聚合报告 (GET :4859/fullreport)
func (w *Worker) callFullReport(ctx context.Context, hecCfg *HikariApiConfig, date, target string) (map[string]interface{}, error) {
	args := map[string]string{
		"device_name": hecCfg.DeviceID,
		"date":        date,
		"db":          hecCfg.DB,
		"target":      target,
	}
	return w.getSigned(ctx, hecCfg, "4859", "/fullreport", args)
}

// callWarning 预警消息查询 (GET :4859/warning)
func (w *Worker) callWarning(ctx context.Context, hecCfg *HikariApiConfig, params map[string]interface{}) (map[string]interface{}, error) {
	args := map[string]string{
		"device_name": hecCfg.DeviceID,
	}
	if st := asString(params["st"]); st != "" {
		args["st"] = st
	}
	if ed := asString(params["ed"]); ed != "" {
		args["ed"] = ed
	}
	return w.getSigned(ctx, hecCfg, "4859", "/warning", args)
}

// callBedInfo 设置床垫宽度和厚度 (POST :4881/bedinfo)
// content-type: application/json
func (w *Worker) callBedInfo(ctx context.Context, hecCfg *HikariApiConfig, params map[string]interface{}) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"appkey":      hecCfg.AppKey,
		"db":          hecCfg.DB,
		"device_name": hecCfg.DeviceID,
	}
	for k, v := range params {
		normalized := strings.ToLower(strings.TrimSpace(k))
		switch normalized {
		case "channel", "device_name", "vendor_device_name",
			"product_key", "access_key", "access_secret",
			"iot_instance_id", "timeout_seconds", "appkey", "secret", "db":
			continue
		default:
			body[k] = v
		}
	}
	if _, hasBW := body["bedWidth"]; !hasBW {
		return nil, fmt.Errorf("bedWidth is required for bedinfo")
	}
	if _, hasMT := body["mattressThickness"]; !hasMT {
		return nil, fmt.Errorf("mattressThickness is required for bedinfo")
	}
	return w.postJSON(ctx, hecCfg, "4881", "/bedinfo", body)
}

// callPressure 压敏无人情况下初始化 (POST :4881/pressure)
// content-type: application/json
func (w *Worker) callPressure(ctx context.Context, hecCfg *HikariApiConfig) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"appkey":      hecCfg.AppKey,
		"db":          hecCfg.DB,
		"device_name": hecCfg.DeviceID,
	}
	return w.postJSON(ctx, hecCfg, "4881", "/pressure", body)
}

// ============================================================
// HTTP 请求工具（签名 + 发送）
// ============================================================

// genSign 生成希卡立 API 签名（MD5 32位大写）
// 算法: 排序所有参数 → k1=v1&k2=v2&... → 末尾追加 &secret=YOUR_SECRET → MD5 → 转大写
func genSign(args map[string]string, secret string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	for _, k := range keys {
		if buf.Len() > 0 {
			buf.WriteByte('&')
		}
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(args[k])
	}
	buf.WriteString("&secret=")
	buf.WriteString(secret)

	hash := md5.Sum([]byte(buf.String()))
	return strings.ToUpper(fmt.Sprintf("%x", hash))
}

// getApiHost 返回基础地址 host:port。
func getApiHost(cfg *HikariApiConfig, port string) string {
	if cfg.BaseHost != "" {
		host := cfg.BaseHost
		if !strings.Contains(host, ":") {
			host = host + ":" + port
		}
		return host
	}
	return "59.110.175.77:" + port
}

// getSigned 发送带签名的 GET 请求。
func (w *Worker) getSigned(ctx context.Context, hecCfg *HikariApiConfig, port, path string, args map[string]string) (map[string]interface{}, error) {
	if args == nil {
		args = make(map[string]string)
	}
	args["appkey"] = hecCfg.AppKey
	args["timestamp"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
	args["sign"] = genSign(args, hecCfg.Secret)

	query := url.Values{}
	for k, v := range args {
		query.Set(k, v)
	}

	host := getApiHost(hecCfg, port)
	reqURL := fmt.Sprintf("http://%s%s?%s", host, path, query.Encode())

	log.Printf("[XIKALI][GET] %s", reqURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http get failed: %w", err)
	}
	defer resp.Body.Close()

	return parseHecaryResponse(resp)
}

// postJSON 发送带签名的 POST JSON 请求。
// 签名参数放在 JSON body 中。
func (w *Worker) postJSON(ctx context.Context, hecCfg *HikariApiConfig, port, path string, body map[string]interface{}) (map[string]interface{}, error) {
	strArgs := make(map[string]string)
	strArgs["appkey"] = hecCfg.AppKey
	for k, v := range body {
		strArgs[k] = fmt.Sprintf("%v", v)
	}
	strArgs["timestamp"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
	strArgs["sign"] = genSign(strArgs, hecCfg.Secret)

	body["timestamp"] = strArgs["timestamp"]
	body["sign"] = strArgs["sign"]

	payloadBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body failed: %w", err)
	}

	host := getApiHost(hecCfg, port)
	reqURL := fmt.Sprintf("http://%s%s", host, path)

	log.Printf("[XIKALI][POST] %s body=%s", reqURL, string(payloadBytes))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http post failed: %w", err)
	}
	defer resp.Body.Close()

	return parseHecaryResponse(resp)
}

// postForm 发送带签名的 POST form-urlencoded 请求。
// 适用于 configset (4877) 等 x-www-form-urlencoded 接口。
func (w *Worker) postForm(ctx context.Context, hecCfg *HikariApiConfig, port, path string, body map[string]interface{}) (map[string]interface{}, error) {
	strArgs := make(map[string]string)
	strArgs["appkey"] = hecCfg.AppKey
	for k, v := range body {
		strArgs[k] = fmt.Sprintf("%v", v)
	}
	strArgs["timestamp"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
	strArgs["sign"] = genSign(strArgs, hecCfg.Secret)

	form := url.Values{}
	for k, v := range strArgs {
		form.Set(k, v)
	}

	host := getApiHost(hecCfg, port)
	reqURL := fmt.Sprintf("http://%s%s", host, path)

	encoded := form.Encode()
	log.Printf("[XIKALI][POST-FORM] %s body=%s", reqURL, encoded)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http post failed: %w", err)
	}
	defer resp.Body.Close()

	return parseHecaryResponse(resp)
}

// parseHecaryResponse 统一解析希卡立 API 响应。
// 响应格式: {"code":200, "msg":"成功", "data": ...}
func parseHecaryResponse(resp *http.Response) (map[string]interface{}, error) {
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response failed: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("parse response json failed: body=%q err=%w", string(respBytes), err)
	}

	result["_http_status"] = resp.StatusCode
	result["_raw_body"] = string(respBytes)

	code, _ := result["code"].(float64)
	if code != 200 {
		msg, _ := result["msg"].(string)
		return result, fmt.Errorf("hecary api error: code=%.0f msg=%s", code, msg)
	}

	return result, nil
}

// ============================================================
// 配置提取与设备认领
// ============================================================

// extractHikariCfg 从 cmd.Params 和 config 中提取希卡立 API 认证信息。
func extractHikariCfg(cmd *base.DeviceCommand, cfg ConfigItem) *HikariApiConfig {
	hecCfg := &HikariApiConfig{
		AppKey: cfg.AccessKey,
		Secret: cfg.AccessSecret,
	}

	channelRaw := asMap(cmd.Params["channel"])

	// channel 参数具有最高优先级
	if v := strings.TrimSpace(asString(channelRaw["appkey"])); v != "" {
		hecCfg.AppKey = v
	}
	if v := strings.TrimSpace(asString(channelRaw["secret"])); v != "" {
		hecCfg.Secret = v
	}
	if v := strings.TrimSpace(asString(channelRaw["access_key"])); v != "" {
		hecCfg.AppKey = v
	}
	if v := strings.TrimSpace(asString(channelRaw["access_secret"])); v != "" {
		hecCfg.Secret = v
	}
	if v := strings.TrimSpace(asString(channelRaw["vendor_device_id"])); v != "" {
		hecCfg.DeviceID = v
	}
	if v := strings.TrimSpace(asString(channelRaw["db"])); v != "" {
		hecCfg.DB = v
	}
	if v := strings.TrimSpace(asString(channelRaw["product_key"])); v != "" {
		hecCfg.ProductKey = v
	}
	if v := strings.TrimSpace(asString(channelRaw["base_host"])); v != "" {
		hecCfg.BaseHost = v
	}

	// 再从 params 顶层提取
	if v := strings.TrimSpace(asString(cmd.Params["vendor_device_id"])); v != "" {
		hecCfg.DeviceID = v
	}
	if v := strings.TrimSpace(asString(cmd.Params["db"])); v != "" {
		hecCfg.DB = v
	}
	if v := strings.TrimSpace(asString(cmd.Params["product_key"])); v != "" {
		hecCfg.ProductKey = v
	}

	return hecCfg
}

// resolveOwnedDevice 判断设备是否属于本插件并返回匹配的配置。
func (w *Worker) resolveOwnedDevice(deviceID string, cmd *base.DeviceCommand) (ConfigItem, bool) {
	deviceID = strings.TrimSpace(deviceID)

	// 1. 优先通过 channel.plugin 显式指定
	channelRaw := asMap(cmd.Params["channel"])
	if pluginNameFromChannel := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		asString(channelRaw["plugin"]),
		asString(channelRaw["plugin_name"]),
		asString(channelRaw["type"]),
	))); pluginNameFromChannel != "" {
		if pluginNameFromChannel == pluginName {
			cfg := w.pickConfigFromChannel(channelRaw)
			cfg = w.fillConfigFromChannel(cfg, channelRaw)
			return cfg, true
		}
		return ConfigItem{}, false
	}

	// 2. 在配置列表中查找
	for _, cfg := range w.configs {
		if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.AccessSecret) == "" {
			continue
		}
		if mappedID, hit, err := commonResolveMappedDeviceID(deviceID); err == nil && hit && mappedID != "" {
			_ = mappedID
			return cfg, true
		}
	}

	return ConfigItem{}, false
}

// pickConfigFromChannel 从 channel 参数中选取匹配的配置。
func (w *Worker) pickConfigFromChannel(channelRaw map[string]interface{}) ConfigItem {
	channelName := strings.TrimSpace(firstNonEmpty(
		asString(channelRaw["name"]),
		asString(channelRaw["channel_name"]),
		asString(channelRaw["host"]),
	))

	if channelName == "" {
		if len(w.configs) > 0 {
			return w.configs[0]
		}
		return ConfigItem{}
	}

	for _, cfg := range w.configs {
		if strings.Contains(cfg.Host, channelName) || strings.Contains(channelName, cfg.Host) {
			return cfg
		}
	}

	if len(w.configs) > 0 {
		return w.configs[0]
	}
	return ConfigItem{}
}

// fillConfigFromChannel 用 channel 参数中的值覆盖配置。
func (w *Worker) fillConfigFromChannel(cfg ConfigItem, channelRaw map[string]interface{}) ConfigItem {
	if v := strings.TrimSpace(asString(channelRaw["access_key"])); v != "" {
		cfg.AccessKey = v
	}
	if v := strings.TrimSpace(asString(channelRaw["access_secret"])); v != "" {
		cfg.AccessSecret = v
	}
	if v := strings.TrimSpace(asString(channelRaw["iot_instance_id"])); v != "" {
		cfg.IoTInstanceID = v
	}
	return cfg
}

// commonResolveMappedDeviceID 从 device_id_mapping 查询设备的厂商 ID。
func commonResolveMappedDeviceID(uniqueID string) (string, bool, error) {
	// 使用 common 包的标准设备映射查询
	// 实际调用 common.ResolveMappedDeviceID("shanghai_xikali", uniqueID)
	// 由于这里只是占位，实际集成时需要导入 common 包并调用
	return "", false, nil
}

// ============================================================
// 辅助函数
// ============================================================

// asMap 安全转换为 map。
func asMap(value interface{}) map[string]interface{} {
	if value == nil {
		return map[string]interface{}{}
	}
	if typed, ok := value.(map[string]interface{}); ok {
		return typed
	}
	return map[string]interface{}{}
}

// asString 安全转换为 string。
func asString(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
