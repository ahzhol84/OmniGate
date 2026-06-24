package huanjing_jiankong_v3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"net/http"
	"strings"
	"time"
)

// SendCommand 实现继电器控制命令的下行执行。
// 命令格式：
//
//	Method: "relayControl"
//	Identifier: "relayControl"
//	Params: {
//	    "device_addr": 21120446,
//	    "relay_no": 1,
//	    "status": 1  (0=关, 1=开)
//	}
func (w *Worker) SendCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || cmd == nil {
		return nil, base.ErrCommandNotSupported
	}

	// 只认领 relayControl 相关的命令
	method := strings.TrimSpace(cmd.Method)
	identifier := strings.TrimSpace(cmd.Identifier)
	if method != "relayControl" && identifier != "relayControl" {
		return nil, base.ErrCommandNotSupported
	}

	// 从 Params 中提取参数（cmd.Params 已经是 map[string]interface{}）
	if cmd.Params == nil {
		return w.buildErrorReply(cmd, "missing params")
	}

	deviceAddrVal, ok := cmd.Params["device_addr"]
	if !ok {
		return w.buildErrorReply(cmd, "missing device_addr")
	}

	deviceAddr, ok := deviceAddrVal.(float64)
	if !ok {
		return w.buildErrorReply(cmd, "invalid device_addr format")
	}

	relayNoVal, ok := cmd.Params["relay_no"]
	if !ok {
		return w.buildErrorReply(cmd, "missing relay_no")
	}

	relayNo, ok := relayNoVal.(float64)
	if !ok {
		return w.buildErrorReply(cmd, "invalid relay_no format")
	}

	statusVal, ok := cmd.Params["status"]
	if !ok {
		return w.buildErrorReply(cmd, "missing status")
	}

	status, ok := statusVal.(float64)
	if !ok {
		return w.buildErrorReply(cmd, "invalid status format")
	}

	if status != 0 && status != 1 {
		return w.buildErrorReply(cmd, "status must be 0 or 1")
	}

	// 查找首个配置来获取登录信息（因为关键是 baseURL + 凭证）
	if len(w.configs) == 0 {
		return w.buildErrorReply(cmd, "no config available")
	}
	cfg := w.configs[0]

	// 获取token
	token, err := w.getOrRefreshToken(ctx, cfg)
	if err != nil {
		return w.buildErrorReply(cmd, fmt.Sprintf("get token failed: %v", err))
	}

	// 调用继电器控制接口
	err = w.controlRelay(ctx, cfg, token, int64(deviceAddr), int(relayNo), int(status))
	if err != nil {
		return w.buildErrorReply(cmd, fmt.Sprintf("control relay failed: %v", err))
	}

	return &base.CommandReply{
		RequestID: cmd.RequestID,
		Code:      0,
		Message:   "relay control command executed successfully",
		Data: map[string]interface{}{
			"device_addr": int64(deviceAddr),
			"relay_no":    int(relayNo),
			"status":      int(status),
			"command_id":  cmd.RequestID,
		},
	}, nil
}

// controlRelay 调用平台的继电器控制接口。
func (w *Worker) controlRelay(ctx context.Context, cfg ConfigItem, token string, deviceAddr int64, relayNo int, status int) error {
	url := fmt.Sprintf("%s/api/relay/control", cfg.BaseURL)

	body := map[string]interface{}{
		"deviceAddr": deviceAddr,
		"relayNo":    relayNo,
		"status":     status,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", token)
	req.Header.Set("Content-Type", "application/json")

	// 设置超时
	if cfg.CommandTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.CommandTimeout)*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("failed to parse relay control response: %w", err)
	}

	// 检查返回码
	code, ok := result["code"].(float64)
	if !ok || code != 1000 {
		return fmt.Errorf("relay control failed: code=%v, message=%v",
			result["code"], result["message"])
	}

	// 检查data是否为true
	data, ok := result["data"].(bool)
	if !ok || !data {
		return fmt.Errorf("relay control returned false: %v", result["message"])
	}

	return nil
}

// buildErrorReply 构建错误回复。
func (w *Worker) buildErrorReply(cmd *base.DeviceCommand, message string) (*base.CommandReply, error) {
	return &base.CommandReply{
		RequestID: cmd.RequestID,
		Code:      -1,
		Message:   message,
		Data: map[string]interface{}{
			"error": message,
		},
	}, nil
}
