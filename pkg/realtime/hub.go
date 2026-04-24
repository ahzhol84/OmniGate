package realtime

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/command"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	Enabled         bool
	ListenAddr      string
	Path            string
	AuthHeader      string
	AuthToken       string
	EventBufferSize int
	WriteTimeout    time.Duration
}

type Hub struct {
	cfg      Config
	server   *http.Server
	upgrader websocket.Upgrader

	clientsMu sync.RWMutex
	clients   map[*websocket.Conn]subscription

	events chan *base.DeviceData
	done   chan struct{}
}

// NewHub 创建并初始化 WebSocket Hub。
// 入参：cfg 为 Hub 配置。
// 处理：补齐默认监听地址、路径、鉴权头、缓冲区和写超时；初始化升级器、客户端表、事件通道与完成通道。
// 出参：返回可直接启动的 Hub 实例。
// @Author ahzhol
func NewHub(cfg Config) *Hub {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		cfg.ListenAddr = ":8088"
	}
	if strings.TrimSpace(cfg.Path) == "" {
		cfg.Path = "/ws/events"
	}
	if strings.TrimSpace(cfg.AuthHeader) == "" {
		cfg.AuthHeader = "X-Iot-Token"
	}
	if cfg.EventBufferSize <= 0 {
		cfg.EventBufferSize = 1024
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 2 * time.Second
	}

	return &Hub{
		cfg: cfg,
		// isMatch 判断订阅条件是否匹配当前设备数据。
		// 入参：data 为设备数据。
		// 处理：按设备类型、设备ID、唯一ID、数据类型四类过滤条件逐项匹配；任一条件不满足即返回 false。
		// 出参：匹配成功返回 true，否则返回 false。
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		clients: make(map[*websocket.Conn]subscription),
		events:  make(chan *base.DeviceData, cfg.EventBufferSize),
		done:    make(chan struct{}),
	}
}

type subscription struct {
	deviceTypes map[string]struct{}
	deviceIDs   map[string]struct{}
	uniqueIDs   map[string]struct{}
	dataTypes   map[string]struct{}
}

func (s subscription) isMatch(data *base.DeviceData) bool {
	if data == nil {
		return false
	}
	if len(s.deviceTypes) > 0 {
		if _, ok := s.deviceTypes[strings.ToUpper(strings.TrimSpace(data.DeviceType))]; !ok {
			return false
		}
	}
	if len(s.deviceIDs) > 0 {
		if _, ok := s.deviceIDs[strings.TrimSpace(data.DeviceID)]; !ok {
			return false
		}
	}
	if len(s.uniqueIDs) > 0 {
		if _, ok := s.uniqueIDs[strings.TrimSpace(data.UniqueID)]; !ok {
			return false
		}
	}
	if len(s.dataTypes) > 0 {
		if _, ok := s.dataTypes[strings.ToUpper(strings.TrimSpace(data.DataType))]; !ok {
			return false
		}
	}
	return true
}

// Start 启动 Hub 的 HTTP/WebSocket 服务。
// 入参：ctx 为生命周期上下文。
// 处理：注册路由、创建并启动 HTTP 服务与广播协程；监听 ctx 取消以优雅关停服务并关闭全部客户端。
// 出参：无。
// @Author ahzhol
func (h *Hub) Start(ctx context.Context) {
	defer close(h.done)

	mux := http.NewServeMux()
	mux.HandleFunc(h.cfg.Path, h.handleWebSocket)
	mux.HandleFunc("/device/command", h.handleDeviceCommand)

	h.server = &http.Server{Addr: h.cfg.ListenAddr, Handler: mux}

	go h.broadcastLoop(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.server.Shutdown(shutdownCtx)
		h.closeAllClients()
	}()

	log.Printf("[WS-HUB] listening at %s%s", h.cfg.ListenAddr, h.cfg.Path)
	if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[WS-HUB] server exited with error: %v", err)
	}
}

// Done 返回 Hub 停止完成信号通道。
// 入参：无。
// 处理：功能函数，直接返回内部完成通道。
// 出参：返回只读完成通道，关闭表示 Hub 已结束。
func (h *Hub) Done() <-chan struct{} {
	return h.done
}

// String 返回 Hub 当前配置的可读摘要。
// 入参：无。
// 处理：功能函数，委托配置对象生成字符串。
// 出参：返回 Hub 配置摘要字符串。
func (h *Hub) String() string {
	return h.cfg.String()
}

// Publish 向 Hub 投递设备事件。
// 入参：data 为设备事件数据。
// 处理：忽略空数据；以非阻塞方式写入事件通道，缓冲区满时丢弃并记录日志。
// 出参：无。
// @Author ahzhol
func (h *Hub) Publish(data *base.DeviceData) {
	if data == nil {
		return
	}
	select {
	case h.events <- data:
	default:
		log.Printf("[WS-HUB] drop event because buffer full, devId=%s type=%s", data.DeviceID, data.DataType)
	}
}

// handleWebSocket 处理 WebSocket 接入请求。
// 入参：rw 为响应写入器，req 为请求对象。
// 处理：校验方法与鉴权，升级连接，解析订阅条件并注册客户端，最后启动读循环保持连接活性。
// 出参：无。
// @Author ahzhol
func (h *Hub) handleWebSocket(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !matchAuth(req, h.cfg.AuthHeader, h.cfg.AuthToken) {
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := h.upgrader.Upgrade(rw, req, nil)
	if err != nil {
		log.Printf("[WS-HUB] upgrade failed path=%s remote=%s err=%v", req.URL.Path, req.RemoteAddr, err)
		return
	}

	sub := parseSubscription(req)
	h.addClient(conn, sub)
	clientTag := strings.TrimSpace(req.Header.Get("X-Iot-Client"))
	if clientTag == "" {
		clientTag = "-"
	}
	log.Printf("[WS-HUB] client connected remote=%s ua=%q x_iot_client=%q filter=%s", req.RemoteAddr, req.UserAgent(), clientTag, formatSubscription(sub))

	go h.keepReadLoop(conn, req.RemoteAddr)
}

// keepReadLoop 保持客户端读循环并在断开时清理资源。
// 入参：conn 为客户端连接，remote 为远端地址字符串。
// 处理：持续读取消息以感知连接状态；读取失败时移除客户端、关闭连接并记录断开日志。
// 出参：无。
// @Author ahzhol
func (h *Hub) keepReadLoop(conn *websocket.Conn, remote string) {
	defer func() {
		h.removeClient(conn)
		_ = conn.Close()
		log.Printf("[WS-HUB] client disconnected remote=%s", remote)
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// broadcastLoop 消费事件通道并执行广播。
// 入参：ctx 为生命周期上下文。
// 处理：循环监听上下文取消或新事件；收到事件后调用广播发送。
// 出参：无。
// @Author ahzhol
func (h *Hub) broadcastLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-h.events:
			h.broadcastDeviceData(data)
		}
	}
}

// broadcastDeviceData 向符合订阅条件的客户端广播设备数据。
// 入参：data 为设备数据。
// 处理：构造消息并序列化；筛选匹配客户端后逐个写入；写失败的连接会被移除并关闭。
// 出参：无。
// @Author ahzhol
func (h *Hub) broadcastDeviceData(data *base.DeviceData) {
	if data == nil {
		return
	}

	msg := map[string]interface{}{
		"type":         "device_realtime",
		"device_id":    data.DeviceID,
		"unique_id":    data.UniqueID,
		"device_type":  data.DeviceType,
		"data_type":    data.DataType,
		"timestamp":    data.Timestamp,
		"compony_name": data.ComponyName,
		"payload":      json.RawMessage(data.Payload),
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[WS-HUB] marshal failed devId=%s err=%v", data.DeviceID, err)
		return
	}

	h.clientsMu.RLock()
	clients := make([]*websocket.Conn, 0, len(h.clients))
	for conn, sub := range h.clients {
		if sub.isMatch(data) {
			clients = append(clients, conn)
		}
	}
	h.clientsMu.RUnlock()

	if len(clients) == 0 {
		return
	}

	failed := 0
	for _, conn := range clients {
		_ = conn.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout))
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			failed++
			h.removeClient(conn)
			_ = conn.Close()
		}
	}

	if failed > 0 {
		log.Printf("[WS-HUB] broadcast done devId=%s clients=%d failed=%d", data.DeviceID, len(clients), failed)
	}
}

// addClient 注册客户端及其订阅信息。
// 入参：conn 为客户端连接，sub 为订阅条件。
// 处理：在互斥锁保护下写入客户端映射。
// 出参：无。
// @Author ahzhol
func (h *Hub) addClient(conn *websocket.Conn, sub subscription) {
	h.clientsMu.Lock()
	defer h.clientsMu.Unlock()
	h.clients[conn] = sub
}

// removeClient 移除指定客户端。
// 入参：conn 为客户端连接。
// 处理：在互斥锁保护下从客户端映射中删除该连接。
// 出参：无。
// @Author ahzhol
func (h *Hub) removeClient(conn *websocket.Conn) {
	h.clientsMu.Lock()
	defer h.clientsMu.Unlock()
	delete(h.clients, conn)
}

// closeAllClients 关闭并清理所有客户端连接。
// 入参：无。
// 处理：在互斥锁保护下遍历全部连接并关闭，然后从映射中删除。
// 出参：无。
// @Author ahzhol
func (h *Hub) closeAllClients() {
	h.clientsMu.Lock()
	defer h.clientsMu.Unlock()
	for conn := range h.clients {
		_ = conn.Close()
		delete(h.clients, conn)
	}
}

// parseSubscription 解析请求中的订阅过滤条件。
// 入参：req 为 WebSocket 握手请求。
// 处理：从查询参数提取 device_type/device_id/unique_id/data_type，分别转换为集合。
// 出参：返回解析后的订阅对象，空条件字段可能为 nil。
// @Author ahzhol
func parseSubscription(req *http.Request) subscription {
	q := req.URL.Query()
	return subscription{
		deviceTypes: parseSetUpper(q.Get("device_type")),
		deviceIDs:   parseSetRaw(q.Get("device_id")),
		uniqueIDs:   parseSetRaw(q.Get("unique_id")),
		dataTypes:   parseSetUpper(q.Get("data_type")),
	}
}

// parseSetRaw 将逗号分隔字符串解析为原样集合。
// 入参：value 为逗号分隔文本。
// 处理：按逗号拆分并去首尾空格，过滤空项后写入集合。
// 出参：有元素时返回集合；无元素时返回 nil。
// @Author ahzhol
func parseSetRaw(value string) map[string]struct{} {
	items := strings.Split(value, ",")
	result := make(map[string]struct{})
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		result[trimmed] = struct{}{}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// parseSetUpper 将逗号分隔字符串解析为大写集合。
// 入参：value 为逗号分隔文本。
// 处理：按逗号拆分、去空格并转换为大写，过滤空项后写入集合。
// 出参：有元素时返回集合；无元素时返回 nil。
// @Author ahzhol
func parseSetUpper(value string) map[string]struct{} {
	items := strings.Split(value, ",")
	result := make(map[string]struct{})
	for _, item := range items {
		trimmed := strings.ToUpper(strings.TrimSpace(item))
		if trimmed == "" {
			continue
		}
		result[trimmed] = struct{}{}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// formatSubscription 生成订阅条件摘要字符串。
// 入参：sub 为订阅对象。
// 处理：统计各过滤集合数量并拼装为简洁文本；无过滤条件时返回 all。
// 出参：返回订阅摘要字符串。
// @Author ahzhol
func formatSubscription(sub subscription) string {
	parts := make([]string, 0, 4)
	if len(sub.deviceTypes) > 0 {
		parts = append(parts, fmt.Sprintf("device_type=%d", len(sub.deviceTypes)))
	}
	if len(sub.deviceIDs) > 0 {
		parts = append(parts, fmt.Sprintf("device_id=%d", len(sub.deviceIDs)))
	}
	if len(sub.uniqueIDs) > 0 {
		parts = append(parts, fmt.Sprintf("unique_id=%d", len(sub.uniqueIDs)))
	}
	if len(sub.dataTypes) > 0 {
		parts = append(parts, fmt.Sprintf("data_type=%d", len(sub.dataTypes)))
	}
	if len(parts) == 0 {
		return "all"
	}
	return strings.Join(parts, ";")
}

// matchAuth 校验请求头鉴权令牌。
// 入参：req 为请求对象，headerName 为鉴权头名，expectedToken 为期望令牌。
// 处理：若未配置期望令牌则直接放行；否则读取请求头并使用常量时间比较避免时序攻击。
// 出参：校验通过返回 true，否则返回 false。
// @Author ahzhol
func matchAuth(req *http.Request, headerName string, expectedToken string) bool {
	expectedToken = strings.TrimSpace(expectedToken)
	if expectedToken == "" {
		return true
	}
	got := strings.TrimSpace(req.Header.Get(headerName))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedToken)) == 1
}

// String 返回 Config 的可读摘要。
// 入参：无。
// 处理：功能函数，格式化输出启用状态、监听地址和路径。
// 出参：返回配置摘要字符串。
// @Author ahzhol
func (c Config) String() string {
	return fmt.Sprintf("enabled=%t listen=%s path=%s", c.Enabled, c.ListenAddr, c.Path)
}

// handleDeviceCommand 处理下行指令 POST /device/command。
// 请求格式: {"device_id":"xxx","request_id":"xxx","method":"property_set","identifier":"xxx","params":{...}}
// 鉴权: 与 WebSocket 共用同一 X-Iot-Token。
// @Author ahzhol
func (h *Hub) handleDeviceCommand(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(rw, `{"code":-1,"message":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if !matchAuth(req, h.cfg.AuthHeader, h.cfg.AuthToken) {
		http.Error(rw, `{"code":-1,"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var body struct {
		DeviceID string `json:"device_id"`
		base.DeviceCommand
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(rw, `{"code":-1,"message":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.DeviceID) == "" {
		http.Error(rw, `{"code":-1,"message":"device_id required"}`, http.StatusBadRequest)
		return
	}

	reply, err := command.Dispatch(req.Context(), body.DeviceID, &body.DeviceCommand)
	if err != nil {
		log.Printf("[COMMAND] dispatch failed device=%s req=%s err=%v", body.DeviceID, body.RequestID, err)
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusBadGateway)
		data, _ := json.Marshal(map[string]interface{}{
			"code":    -1,
			"message": err.Error(),
		})
		_, _ = rw.Write(data)
		return
	}

	log.Printf("[COMMAND] ok device=%s req=%s code=%d", body.DeviceID, body.RequestID, reply.Code)
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	data, _ := json.Marshal(reply)
	_, _ = rw.Write(data)
}
