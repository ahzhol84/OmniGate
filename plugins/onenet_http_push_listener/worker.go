package onenet_http_push_listener

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/plugin"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
)

type ConfigItem struct {
	ListenAddr         string `json:"listen_addr"`
	ReceivePath        string `json:"receive_path"`
	ComponyName        string `json:"compony_name"`
	DeviceType         string `json:"device_type"`
	DataType           string `json:"data_type"`
	VerifyToken        string `json:"verify_token"`
	EnableTokenVerify  bool   `json:"enable_token_verify"`
	AuthHeader         string `json:"auth_header"`
	AuthToken          string `json:"auth_token"`
	RequestLimit       int64  `json:"request_limit_bytes"`
	ResponseOnVerified string `json:"response_on_verified"`
}

type Worker struct {
	configs    []ConfigItem
	server     *http.Server
	listenAddr string
}

// Init 解析插件配置列表并完成默认值填充与合法性校验。
// 入参：configs 为每个监听路由的 JSON 配置原文。
// 处理：逐项反序列化、设置默认值、标准化路径、校验 path/listen_addr 冲突并写入 w.configs。
// 出参：返回 nil 表示初始化成功；返回 error 表示配置为空、解析失败或配置冲突。
func (w *Worker) Init(configs []json.RawMessage) error {
	if len(configs) == 0 {
		return fmt.Errorf("empty configs")
	}

	pathIndex := make(map[string]int)

	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d] parse failed: %w", i, err)
		}

		if strings.TrimSpace(cfg.ListenAddr) == "" {
			cfg.ListenAddr = ":8087"
		}
		if strings.TrimSpace(cfg.ReceivePath) == "" {
			cfg.ReceivePath = "/push/onenet"
		}
		cfg.ReceivePath = normalizePath(cfg.ReceivePath)

		if strings.TrimSpace(cfg.DeviceType) == "" {
			cfg.DeviceType = "ONENET"
		}
		if strings.TrimSpace(cfg.DataType) == "" {
			cfg.DataType = "ONENET_PUSH"
		}
		if strings.TrimSpace(cfg.AuthHeader) == "" {
			cfg.AuthHeader = "token"
		}
		if cfg.RequestLimit <= 0 {
			cfg.RequestLimit = 2 << 20
		}

		if prev, exists := pathIndex[cfg.ReceivePath]; exists {
			return fmt.Errorf("config[%d] receive_path %q conflicts with config[%d]", i, cfg.ReceivePath, prev)
		}
		pathIndex[cfg.ReceivePath] = i

		if w.listenAddr == "" {
			w.listenAddr = cfg.ListenAddr
		} else if w.listenAddr != cfg.ListenAddr {
			return fmt.Errorf("all configs must use the same listen_addr, got %s and %s", w.listenAddr, cfg.ListenAddr)
		}

		w.configs = append(w.configs, cfg)
	}

	return nil
}

// Start 启动 HTTP 服务并按配置挂载路由处理器。
// 入参：ctx 用于控制服务优雅退出；out 用于输出解析后的设备数据。
// 处理：为每个 receive_path 构建 handler，监听请求并按路径分发；ctx 取消时触发 Shutdown。
// 出参：无显式返回值；运行期间通过日志输出状态并将数据写入 out。
func (w *Worker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	handlers := make(map[string]http.HandlerFunc, len(w.configs))
	for i := range w.configs {
		cfg := w.configs[i]
		handlers[cfg.ReceivePath] = w.buildHandler(cfg, out)
		log.Printf("[ONENET] route mounted path=%s listen=%s", cfg.ReceivePath, w.listenAddr)
	}

	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		path := normalizePath(req.URL.Path)
		if h, ok := handlers[path]; ok {
			h(rw, req)
			return
		}
		log.Printf("[ONENET] unmatched route method=%s path=%s remote=%s", req.Method, req.URL.Path, req.RemoteAddr)
		http.NotFound(rw, req)
	})

	w.server = &http.Server{Addr: w.listenAddr, Handler: handler}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.server.Shutdown(shutdownCtx)
	}()

	log.Printf("[ONENET] listening at %s", w.listenAddr)
	if err := w.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[ONENET] server exited with error: %v", err)
	}
}

// buildHandler 基于单条配置构建路由处理函数。
// 入参：cfg 为当前路由配置；out 为设备数据输出通道。
// 处理：GET 请求走 URL 验证流程，POST 请求走数据接收流程，其他方法返回 405。
// 出参：返回一个可直接注册到路由的 http.HandlerFunc。
func (w *Worker) buildHandler(cfg ConfigItem, out chan<- *base.DeviceData) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			w.handleURLVerify(rw, req, cfg)
		case http.MethodPost:
			w.handlePushData(rw, req, cfg, out)
		default:
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// handleURLVerify 处理 OneNET 的 URL 验证请求。
// 入参：rw/req 为 HTTP 上下文；cfg 为当前路由配置。
// 处理：先做鉴权，再按配置校验 token/nonce/msg/signature，校验通过后回写 msg 或默认响应文本。
// 出参：无显式返回值；通过 rw 输出 200/4xx 响应。
func (w *Worker) handleURLVerify(rw http.ResponseWriter, req *http.Request, cfg ConfigItem) {
	msg := strings.TrimSpace(req.URL.Query().Get("msg"))
	nonce := strings.TrimSpace(req.URL.Query().Get("nonce"))
	signature := strings.TrimSpace(req.URL.Query().Get("signature"))

	if cfg.EnableTokenVerify {
		token := strings.TrimSpace(cfg.VerifyToken)
		if token == "" {
			http.Error(rw, "verify_token required when enable_token_verify=true", http.StatusBadRequest)
			return
		}
		if msg == "" || nonce == "" || signature == "" {
			http.Error(rw, "missing msg/nonce/signature", http.StatusBadRequest)
			return
		}
		if !verifyOneNETSignature(token, nonce, msg, signature) {
			http.Error(rw, "signature mismatch", http.StatusUnauthorized)
			return
		}
	}

	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	if msg != "" {
		_, _ = rw.Write([]byte(msg))
	} else if strings.TrimSpace(cfg.ResponseOnVerified) != "" {
		_, _ = rw.Write([]byte(strings.TrimSpace(cfg.ResponseOnVerified)))
	} else {
		_, _ = rw.Write([]byte("ok"))
	}

	log.Printf("[ONENET] url verify path=%s remote=%s token_verify=%t success", cfg.ReceivePath, req.RemoteAddr, cfg.EnableTokenVerify)
}

// handlePushData 处理 OneNET 推送数据并转换为 DeviceData。
// 入参：rw/req 为 HTTP 上下文；cfg 为当前路由配置；out 为设备数据输出通道。
// 处理：鉴权、限制读取并解析 JSON、尝试解析内层 msg、提取 deviceID、组装 DeviceData 后写入 out。
// 出参：无显式返回值；成功返回业务 JSON，通道阻塞或数据非法时返回对应错误状态。
func (w *Worker) handlePushData(rw http.ResponseWriter, req *http.Request, cfg ConfigItem, out chan<- *base.DeviceData) {
	if !matchAuth(req, cfg.AuthHeader, cfg.AuthToken) {
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, cfg.RequestLimit))
	if err != nil {
		http.Error(rw, "read body failed", http.StatusBadRequest)
		return
	}
	_ = req.Body.Close()

	trimmedBody := strings.TrimSpace(string(body))
	if trimmedBody == "" {
		http.Error(rw, "empty body", http.StatusBadRequest)
		return
	}

	var payloadObj map[string]interface{}
	if err := json.Unmarshal(body, &payloadObj); err != nil {
		http.Error(rw, "invalid json body", http.StatusBadRequest)
		return
	}

	prettyBody, _ := json.MarshalIndent(payloadObj, "", "  ")
	log.Printf("[ONENET] recv path=%s remote=%s payload=\n%s", cfg.ReceivePath, req.RemoteAddr, string(prettyBody))

	innerMsgObj, hasInner := parseInnerMsg(payloadObj)
	if hasInner {
		prettyInner, _ := json.MarshalIndent(innerMsgObj, "", "  ")
		log.Printf("[ONENET] parsed inner msg path=%s remote=%s msg=\n%s", cfg.ReceivePath, req.RemoteAddr, string(prettyInner))
	}

	flatPayloadObj := flattenPayload(payloadObj, innerMsgObj, hasInner)
	flatPayload, err := json.Marshal(flatPayloadObj)
	if err != nil {
		http.Error(rw, "flatten payload failed", http.StatusBadRequest)
		return
	}
	prettyFlat, _ := json.MarshalIndent(flatPayloadObj, "", "  ")
	log.Printf("[ONENET] flattened payload path=%s remote=%s payload=\n%s", cfg.ReceivePath, req.RemoteAddr, string(prettyFlat))

	deviceID := pickFirstNonEmpty(payloadObj, "device_id", "deviceId", "devId", "imei", "sn")
	if deviceID == "" && hasInner {
		deviceID = pickFirstNonEmpty(innerMsgObj, "deviceName", "device_id", "deviceId", "devId", "imei", "sn")
	}
	if deviceID == "" {
		deviceID = "UNKNOWN_ONENET_DEVICE"
	}

	dataType := strings.TrimSpace(cfg.DataType)
	if dataType == "" {
		dataType = "ONENET_PUSH"
	}

	deviceData := &base.DeviceData{
		DeviceID:    deviceID,
		UniqueID:    deviceID,
		DeviceType:  strings.TrimSpace(cfg.DeviceType),
		Timestamp:   time.Now(),
		Payload:     json.RawMessage(flatPayload),
		ComponyName: strings.TrimSpace(cfg.ComponyName),
		DataType:    dataType,
	}

	select {
	case out <- deviceData:
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"code":0,"message":"ok"}`))
	case <-time.After(2 * time.Second):
		http.Error(rw, "busy", http.StatusServiceUnavailable)
		log.Printf("[ONENET] channel busy, drop path=%s devId=%s", cfg.ReceivePath, deviceID)
	}
}

// verifyOneNETSignature 校验 OneNET 回调签名是否匹配。
// 入参：token/nonce/msg 为签名原始要素，signature 为请求方提供的签名值。
// 处理：优先按 token+nonce+msg 计算本地摘要，并兼容历史排序算法及 URL 解码前后形式。
// 出参：true 表示签名有效；false 表示签名不匹配。
func verifyOneNETSignature(token, nonce, msg, signature string) bool {
	digests := []string{
		calcOneNETDigest(token, nonce, msg),
		calcOneNETDigestSorted(token, nonce, msg),
	}

	for _, base64Digest := range digests {
		if subtle.ConstantTimeCompare([]byte(base64Digest), []byte(signature)) == 1 {
			return true
		}
		decodedSig, err := url.QueryUnescape(signature)
		if err == nil && subtle.ConstantTimeCompare([]byte(base64Digest), []byte(decodedSig)) == 1 {
			return true
		}
		decodedDigest, err := url.QueryUnescape(base64Digest)
		if err == nil && subtle.ConstantTimeCompare([]byte(decodedDigest), []byte(signature)) == 1 {
			return true
		}
	}
	return false
}

// calcOneNETDigest 计算 OneNET 签名摘要（token+nonce+msg 拼接后 MD5，再做 Base64 编码）。
// 入参：token、nonce、msg 为签名参与字段。
// 处理：按 token、nonce、msg 固定顺序拼接并计算摘要。
// 出参：返回 Base64 编码的摘要字符串。
func calcOneNETDigest(token, nonce, msg string) string {
	raw := token + nonce + msg
	sum := md5.Sum([]byte(raw))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// calcOneNETDigestSorted 兼容历史排序拼接算法，避免影响已接入环境。
// 入参：token、nonce、msg 为签名参与字段。
// 处理：对三段字符串排序后拼接并计算摘要。
// 出参：返回 Base64 编码的摘要字符串。
func calcOneNETDigestSorted(token, nonce, msg string) string {
	parts := []string{token, nonce, msg}
	sort.Strings(parts)
	raw := strings.Join(parts, "")
	sum := md5.Sum([]byte(raw))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// pickFirstNonEmpty 从 payload 中按 keys 顺序提取首个非空字段值。
// 入参：payload 为待检索对象；keys 为候选字段名列表。
// 处理：依次读取字段并转为字符串，去空白后遇到首个非空即返回。
// 出参：找到则返回字段值；未找到返回空字符串。
func pickFirstNonEmpty(payload map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := payload[key]; ok {
			trimmed := strings.TrimSpace(fmt.Sprintf("%v", v))
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// parseInnerMsg 解析 payload.msg 内部对象，兼容字符串 JSON 或对象类型。
// 入参：payload 为外层请求体对象。
// 处理：读取 msg 字段；若是 JSON 字符串则反序列化，若已是对象则直接返回。
// 出参：返回解析后的内层对象及是否成功解析标记。
func parseInnerMsg(payload map[string]interface{}) (map[string]interface{}, bool) {
	rawMsg, ok := payload["msg"]
	if !ok || rawMsg == nil {
		return nil, false
	}

	switch typed := rawMsg.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil, false
		}
		var inner map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &inner); err != nil {
			return nil, false
		}
		return inner, true
	case map[string]interface{}:
		return typed, true
	default:
		return nil, false
	}
}

// flattenPayload 生成单层 JSON：优先拍平 innerMsg，若不存在则拍平外层 payload。
// 入参：payload 为外层对象；innerMsg 为解析出的 msg 对象；hasInner 表示 innerMsg 是否可用。
// 处理：递归展开所有嵌套字段，键名使用下划线拼接；遇到包含 value 的对象仅保留 value。
// 出参：返回无嵌套结构的 map，可直接序列化为下游 Payload。
func flattenPayload(payload map[string]interface{}, innerMsg map[string]interface{}, hasInner bool) map[string]interface{} {
	source := payload
	if hasInner {
		source = innerMsg
	}

	flat := make(map[string]interface{})
	for key, value := range source {
		flattenValue(key, value, flat)
	}
	return flat
}

// flattenValue 递归拍平任意值到 out 中。
// 入参：prefix 为当前键前缀；value 为待处理值；out 为输出结果容器。
// 处理：map 继续下钻，slice 按索引拍平，若 map 包含 value 字段则直接取 value。
// 出参：无，结果写入 out。
func flattenValue(prefix string, value interface{}, out map[string]interface{}) {
	if extracted, ok := extractValueField(value); ok {
		out[prefix] = extracted
		return
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		out[prefix] = nil
		return
	}

	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			out[prefix] = nil
			return
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Map:
		if rv.Len() == 0 {
			out[prefix] = value
			return
		}
		for _, key := range rv.MapKeys() {
			child := rv.MapIndex(key)
			nextPrefix := fmt.Sprintf("%v", key.Interface())
			if prefix != "" {
				nextPrefix = prefix + "_" + nextPrefix
			}
			flattenValue(nextPrefix, child.Interface(), out)
		}
	case reflect.Slice, reflect.Array:
		if rv.Len() == 0 {
			out[prefix] = value
			return
		}
		for index := 0; index < rv.Len(); index++ {
			nextPrefix := fmt.Sprintf("%s_%d", prefix, index)
			flattenValue(nextPrefix, rv.Index(index).Interface(), out)
		}
	default:
		out[prefix] = value
	}
}

// extractValueField 判断对象是否可按 {time,value} 语义简化为 value。
// 入参：obj 为待判断对象。
// 处理：存在 value 字段即返回其值，忽略 time 等其他字段。
// 出参：第一个返回值为提取出的 value；第二个返回值表示是否成功提取。
func extractValueField(obj interface{}) (interface{}, bool) {
	rv := reflect.ValueOf(obj)
	if !rv.IsValid() {
		return nil, false
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Map {
		return nil, false
	}

	for _, key := range rv.MapKeys() {
		if fmt.Sprintf("%v", key.Interface()) == "value" {
			value := rv.MapIndex(key)
			if !value.IsValid() {
				return nil, true
			}
			return value.Interface(), true
		}
	}

	return nil, false
}

// matchAuth 校验请求头中的令牌是否与配置值一致。
// 入参：req 为当前请求；headerName 为令牌头名；expectedToken 为期望令牌。
// 处理：当 expectedToken 为空时放行；否则读取对应请求头并做常量时间比较。
// 出参：true 表示认证通过；false 表示认证失败。
func matchAuth(req *http.Request, headerName string, expectedToken string) bool {
	expectedToken = strings.TrimSpace(expectedToken)
	if expectedToken == "" {
		return true
	}

	candidates := make([]string, 0, 8)
	if h := strings.TrimSpace(headerName); h != "" {
		candidates = append(candidates, req.Header.Get(h))
		candidates = append(candidates, req.URL.Query().Get(h))
	}

	candidates = append(candidates,
		req.Header.Get("token"),
		req.Header.Get("Token"),
		req.Header.Get("X-OneNET-Token"),
		req.Header.Get("X-Token"),
		req.URL.Query().Get("token"),
		req.URL.Query().Get("access_token"),
	)

	authorization := strings.TrimSpace(req.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		candidates = append(candidates, strings.TrimSpace(authorization[7:]))
	} else if authorization != "" {
		candidates = append(candidates, authorization)
	}

	for _, candidate := range candidates {
		got := strings.TrimSpace(candidate)
		if got == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(expectedToken)) == 1 {
			return true
		}
	}

	return false
}

// normalizePath 统一路径格式，保证前缀、重复斜杠和末尾斜杠处理一致。
// 入参：path 为待规范化的原始路径。
// 处理：去空白、补全前导斜杠、合并连续斜杠并清理非根路径末尾斜杠。
// 出参：返回标准化后的路径字符串。
func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
		if path == "" {
			path = "/"
		}
	}
	return path
}

// init 在插件加载时注册 onenet_http_push_listener 的 Worker 工厂。
// 入参：无。
// 处理：调用 plugin.Register 绑定插件名到 Worker 构造函数。
// 出参：无。
func init() {
	plugin.Register("onenet_http_push_listener", func() base.IWorker {
		return &Worker{}
	})
}
