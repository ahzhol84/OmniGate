package zixing_smart_home

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type apiResp struct {
	Code int                    `json:"code"`
	Msg  string                 `json:"msg"`
	Time string                 `json:"time"`
	Data map[string]interface{} `json:"data"`
}

func (w *Worker) SendCommand(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	if cmd == nil {
		return nil, base.ErrCommandNotSupported
	}
	if len(w.configs) == 0 {
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: "zixing plugin not initialized"}, fmt.Errorf("zixing plugin not initialized")
	}

	targetID := strings.TrimSpace(deviceID)
	cfg, cfgIdx, targetType, ok := w.matchConfig(targetID, cmd)
	if !ok {
		return nil, base.ErrCommandNotSupported
	}

	identifier := strings.TrimSpace(cmd.Identifier)
	route := normalizeIdentifier(identifier, targetType, cmd)
	if route == "" {
		return nil, base.ErrCommandNotSupported
	}

	token, err := w.ensureToken(ctx, cfgIdx, cfg)
	if err != nil {
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: err.Error()}, err
	}

	form, err := buildForm(route, targetType, targetID, token, cmd)
	if err != nil {
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: err.Error()}, err
	}

	respBody, err := w.call(ctx, cfg.BaseURL+route, form)
	if err != nil && strings.Contains(err.Error(), "token") {
		// token 可能失效：清缓存(内存+redis)后重登重试一次
		w.invalidateToken(ctx, cfgIdx, cfg)
		newToken, loginErr := w.ensureToken(ctx, cfgIdx, cfg)
		if loginErr == nil {
			form.Set("token", newToken)
			respBody, err = w.call(ctx, cfg.BaseURL+route, form)
		}
	}
	if err != nil {
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: err.Error()}, err
	}

	var ar apiResp
	if err = json.Unmarshal(respBody, &ar); err != nil {
		return &base.CommandReply{RequestID: cmd.RequestID, Code: -1, Message: "invalid vendor response", Data: map[string]interface{}{"raw": string(respBody)}}, err
	}

	replyCode := 0
	if ar.Code != 1 {
		replyCode = -1
	}
	data := map[string]interface{}{
		"vendor_code": ar.Code,
		"vendor_msg":  ar.Msg,
		"vendor_time": ar.Time,
		"route":       route,
		"target_type": targetType,
		"request":     form,
		"raw":         string(respBody),
	}
	if ar.Data != nil {
		data["vendor_data"] = ar.Data
	}
	return &base.CommandReply{RequestID: cmd.RequestID, Code: replyCode, Message: ar.Msg, Data: data}, nil
}

func (w *Worker) matchConfig(targetID string, cmd *base.DeviceCommand) (ConfigItem, int, string, bool) {
	targetType := inferTargetType(cmd)
	for idx, cfg := range w.configs {
		// 1) unique_id / device_id 前缀认领
		if targetID != "" {
			lowerID := strings.ToLower(targetID)
			for _, p := range cfg.ClaimPrefixes {
				if p != "" && strings.HasPrefix(lowerID, p) {
					return cfg, idx, targetType, true
				}
			}
		}
		// 2) 若传了明显参数特征，也由本插件认领
		if targetType != "" {
			return cfg, idx, targetType, true
		}
		// 3) 显式 hint 认领
		hint := firstNonEmptyString(asString(cmd.Params["target_type"]), asString(cmd.Params["device_type"]))
		switch routeHint(strings.ToLower(strings.TrimSpace(hint))) {
		case "switch", "curtain", "doorlock":
			return cfg, idx, routeHint(strings.ToLower(strings.TrimSpace(hint))), true
		}
	}
	return ConfigItem{}, -1, "", false
}

func normalizeIdentifier(identifier string, targetType string, cmd *base.DeviceCommand) string {
	id := strings.TrimSpace(identifier)
	if strings.HasPrefix(id, "/index.php/") {
		return id
	}
	switch strings.ToLower(id) {
	case "setonoff", "switch_set", "switch_onoff":
		return "/index.php/api/household/setOnoff"
	case "readonoff", "switch_read":
		return "/index.php/api/household/readOnoff"
	case "open_close_curtain", "curtain_open_close":
		return "/index.php/api/household/open_close_curtain"
	case "set_curtainpercentage", "curtain_percentage":
		return "/index.php/api/household/set_curtainpercentage"
	case "open_door":
		return "/index.php/api/index/open_door"
	case "deliver_password":
		return "/index.php/api/index/deliver_password"
	case "delete_password":
		return "/index.php/api/index/delete_password"
	case "deliver_card":
		return "/index.php/api/index/deliver_card"
	case "del_card":
		return "/index.php/api/index/del_card"
	case "deliver_face":
		return "/index.php/api/index/deliver_face"
	case "del_face":
		return "/index.php/api/index/del_face"
	case "deliver_fingerprint":
		return "/index.php/api/index/deliver_fingerprint"
	case "del_fingerprint":
		return "/index.php/api/index/del_fingerprint"
	case "door_open_log":
		return "/index.php/api/index/door_open_log"
	case "add_bluetooth_log":
		return "/index.php/api/index/add_bluetooth_log"
	}

	// 未显式给 identifier 时，按入参自动推断
	if id == "" {
		if flag, ok := pickInt(cmd.Params, "open_door", "openDoor", "door_open", "doorOpen"); ok && flag > 0 {
			return "/index.php/api/index/open_door"
		}
		if _, ok := pickInt(cmd.Params, "Onoff", "onoff"); ok {
			return "/index.php/api/household/setOnoff"
		}
		if _, ok := pickInt(cmd.Params, "percentage"); ok {
			return "/index.php/api/household/set_curtainpercentage"
		}
		if _, ok := pickInt(cmd.Params, "type"); ok {
			return "/index.php/api/household/open_close_curtain"
		}
	}

	switch targetType {
	case "switch":
		return "/index.php/api/household/setOnoff"
	case "curtain":
		return "/index.php/api/household/open_close_curtain"
	case "doorlock":
		return "/index.php/api/index/open_door"
	default:
		return ""
	}
}

func buildForm(route string, targetType string, targetID string, token string, cmd *base.DeviceCommand) (url.Values, error) {
	form := url.Values{}
	form.Set("token", token)
	resolvedID := resolveDeviceID(targetID, cmd.Params)
	resolvedSN := resolveDoorLockSN(targetID, cmd.Params)

	switch route {
	case "/index.php/api/household/setOnoff":
		deviceID := resolvedID
		onoff, ok := pickInt(cmd.Params, "Onoff", "onoff", "value")
		if !ok {
			onoff = 1
		}
		epID := firstNonEmptyString(asString(cmd.Params["epid"]), asString(cmd.Params["ep_id"]), asString(cmd.Params["endpoint"]))
		if deviceID == "" {
			return nil, fmt.Errorf("setOnoff missing device_id")
		}
		form.Set("device_id", deviceID)
		form.Set("Onoff", strconv.Itoa(onoff))
		if epID != "" {
			form.Set("epid", epID)
		}
	case "/index.php/api/household/readOnoff":
		deviceID := resolvedID
		if deviceID == "" {
			return nil, fmt.Errorf("readOnoff missing device_id")
		}
		form.Set("device_id", deviceID)
	case "/index.php/api/household/open_close_curtain":
		deviceID := resolvedID
		typeVal, ok := pickInt(cmd.Params, "type", "value")
		if !ok {
			typeVal = 1
		}
		if deviceID == "" {
			return nil, fmt.Errorf("open_close_curtain missing device_id")
		}
		form.Set("device_id", deviceID)
		form.Set("type", strconv.Itoa(typeVal))
	case "/index.php/api/household/set_curtainpercentage":
		deviceID := resolvedID
		percentage, ok := pickInt(cmd.Params, "percentage", "value")
		if !ok {
			return nil, fmt.Errorf("set_curtainpercentage missing percentage")
		}
		if deviceID == "" {
			return nil, fmt.Errorf("set_curtainpercentage missing device_id")
		}
		form.Set("device_id", deviceID)
		form.Set("percentage", strconv.Itoa(percentage))
	case "/index.php/api/index/open_door", "/index.php/api/index/door_open_log", "/index.php/api/index/delete_password", "/index.php/api/index/del_card", "/index.php/api/index/del_face", "/index.php/api/index/del_fingerprint":
		sn := resolvedSN
		if sn == "" {
			return nil, fmt.Errorf("%s missing sn", route)
		}
		form.Set("sn", sn)
		copyOptional(form, cmd.Params, "password", "cardNo", "faceNo", "fingerprintNo", "id")
	case "/index.php/api/index/deliver_password":
		sn := resolvedSN
		if sn == "" {
			return nil, fmt.Errorf("deliver_password missing sn")
		}
		form.Set("sn", sn)
		required := []string{"start_time", "end_time", "mobile", "password_type"}
		for _, k := range required {
			v := firstNonEmptyString(asString(cmd.Params[k]))
			if v == "" {
				return nil, fmt.Errorf("deliver_password missing %s", k)
			}
			form.Set(k, v)
		}
		password := firstNonEmptyString(asString(cmd.Params["password"]))
		if password != "" {
			form.Set("password", password)
		}
		sendMsg := firstNonEmptyString(asString(cmd.Params["send_msg"]))
		if sendMsg != "" {
			form.Set("send_msg", sendMsg)
		}
	case "/index.php/api/index/deliver_card", "/index.php/api/index/deliver_face", "/index.php/api/index/deliver_fingerprint":
		sn := resolvedSN
		if sn == "" {
			return nil, fmt.Errorf("%s missing sn", route)
		}
		form.Set("sn", sn)
		for key, value := range cmd.Params {
			s := strings.TrimSpace(asString(value))
			if s == "" {
				continue
			}
			if key == "token" {
				continue
			}
			form.Set(key, s)
		}
	case "/index.php/api/index/add_bluetooth_log":
		for key, value := range cmd.Params {
			s := strings.TrimSpace(asString(value))
			if s == "" {
				continue
			}
			if key == "token" {
				continue
			}
			form.Set(key, s)
		}
		if form.Get("sn") == "" {
			sn := resolvedSN
			if sn != "" {
				form.Set("sn", sn)
			}
		}
	default:
		if targetType == "switch" {
			form.Set("device_id", resolvedID)
		}
	}
	return form, nil
}

func (w *Worker) call(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vendor http status=%d body=%s", resp.StatusCode, string(body))
	}
	return body, nil
}

func pickInt(params map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := params[k]
		if !ok {
			continue
		}
		s := strings.TrimSpace(asString(v))
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		return n, true
	}
	return 0, false
}

func copyOptional(form url.Values, params map[string]interface{}, keys ...string) {
	for _, k := range keys {
		v := strings.TrimSpace(asString(params[k]))
		if v != "" {
			form.Set(k, v)
		}
	}
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func asString(value interface{}) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if float64(int64(v)) == v {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", value)
	}
}

func routeHint(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	switch raw {
	case "switch", "light", "onoff":
		return "switch"
	case "curtain", "blind":
		return "curtain"
	case "doorlock", "lock":
		return "doorlock"
	default:
		return ""
	}
}

func inferTargetType(cmd *base.DeviceCommand) string {
	if cmd == nil {
		return ""
	}
	id := strings.ToLower(strings.TrimSpace(cmd.Identifier))
	if strings.Contains(id, "curtain") {
		return "curtain"
	}
	if strings.Contains(id, "door") || strings.Contains(id, "lock") || strings.Contains(id, "fingerprint") || strings.Contains(id, "card") || strings.Contains(id, "face") || strings.Contains(id, "password") {
		return "doorlock"
	}
	if strings.Contains(id, "onoff") || strings.Contains(id, "switch") {
		return "switch"
	}

	if _, ok := pickInt(cmd.Params, "Onoff", "onoff"); ok {
		return "switch"
	}
	if flag, ok := pickInt(cmd.Params, "open_door", "openDoor", "door_open", "doorOpen"); ok && flag > 0 {
		return "doorlock"
	}
	if _, ok := pickInt(cmd.Params, "type"); ok {
		return "curtain"
	}
	if _, ok := pickInt(cmd.Params, "percentage"); ok {
		return "curtain"
	}
	if firstNonEmptyString(asString(cmd.Params["sn"])) != "" {
		return "doorlock"
	}
	return ""
}

func resolveDeviceID(targetID string, params map[string]interface{}) string {
	candidate := firstNonEmptyString(asString(params["device_id"]), asString(params["target_device_id"]))
	if candidate != "" {
		return candidate
	}
	if strings.HasPrefix(strings.ToLower(targetID), "zixing|") || strings.HasPrefix(strings.ToLower(targetID), "zx|") {
		parts := strings.Split(targetID, "|")
		if len(parts) > 0 {
			last := strings.TrimSpace(parts[len(parts)-1])
			if last != "" {
				return last
			}
		}
	}
	return strings.TrimSpace(targetID)
}

func resolveDoorLockSN(targetID string, params map[string]interface{}) string {
	sn := firstNonEmptyString(asString(params["sn"]), asString(params["doorlock_sn"]))
	if sn != "" {
		return sn
	}
	return resolveDeviceID(targetID, params)
}
