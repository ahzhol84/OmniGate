package dyf20a_poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 单个配置项结构
type ConfigItem struct {
	Auth struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		LoginType   string `json:"login_type"`
		BaseURL     string `json:"base_url"`
		ComponyName string `json:"compony_name"`
	} `json:"auth"`
	Devices []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Interval string `json:"interval"`  // e.g., "5m"
		UniqueID string `json:"unique_id"` // 设备唯一索引，可选
	} `json:"devices"`
}

type DYF20AWorker struct {
	configs []ConfigItem
	authMap map[string]*TokenManager // 按BaseURL存储认证管理器
}

func (w *DYF20AWorker) Init(configs []json.RawMessage) error {
	log.Printf("[dyf20a] 开始初始化，配置项数量: %d", len(configs))

	w.authMap = make(map[string]*TokenManager)
	totalDevices := 0

	for i, config := range configs {
		var item ConfigItem
		if err := json.Unmarshal(config, &item); err != nil {
			return fmt.Errorf("第%d个配置解析失败: %v", i+1, err)
		}
		w.configs = append(w.configs, item)

		// 为每个BaseURL创建认证管理器（去重）
		baseURL := item.Auth.BaseURL
		if _, exists := w.authMap[baseURL]; !exists {
			w.authMap[baseURL] = NewTokenManager(
				item.Auth.Username,
				item.Auth.Password,
				baseURL,
			)
			log.Printf("[dyf20a] 创建认证管理器: %s", baseURL)
		}

		totalDevices += len(item.Devices)
		log.Printf("[dyf20a] 配置%d: 公司=%s, 设备数=%d",
			i+1, item.Auth.ComponyName, len(item.Devices))
	}

	log.Printf("[dyf20a] 总计配置项: %d, 总设备数: %d, 认证管理器数: %d",
		len(configs), totalDevices, len(w.authMap))
	return nil
}

func (w *DYF20AWorker) Start(ctx context.Context, out chan<- *base.DeviceData) {
	var wg sync.WaitGroup
	taskCount := 0

	log.Printf("[WORKER] 启动插件，开始调度设备任务...")

	// 遍历所有配置项和设备
	for configIndex, config := range w.configs {
		auth := w.authMap[config.Auth.BaseURL]

		for _, dev := range config.Devices {
			taskCount++
			wg.Add(1)

			go func(cfgIdx int, device DeviceInfo, authMgr *TokenManager, compony string) {
				defer wg.Done()
				w.runDeviceTask(ctx, out, cfgIdx, device, authMgr, compony)
			}(configIndex+1, dev, auth, config.Auth.ComponyName)
		}
	}

	log.Printf("[WORKER] 共启动 %d 个设备任务", taskCount)

	// 等待所有任务结束
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		log.Println("[WORKER] 收到停止信号，等待任务清理...")
		<-done
	case <-done:
		log.Println("[WORKER] 所有设备任务已正常结束")
	}
}

func (w *DYF20AWorker) runDeviceTask(ctx context.Context, out chan<- *base.DeviceData,
	configIndex int, device DeviceInfo, auth *TokenManager, componyName string) {

	interval, err := time.ParseDuration(device.Interval)
	if err != nil {
		log.Printf("[DEVICE-%s] 无效间隔 %s: %v", device.ID, device.Interval, err)
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	runTask := func() {
		data, err := w.fetch(device.ID, auth, componyName)
		if err != nil {
			log.Printf("[DEVICE-%s] 采集失败: %v", device.ID, err)
			return
		}
		deviceNumericID := common.ResolveDeviceUniqueID(
			device.UniqueID,
			componyName,
			"dyf20a_poller",
			device.Type,
			device.ID,
		)
		data.DeviceID = deviceNumericID
		data.UniqueID = deviceNumericID
		select {
		case out <- data:
			log.Printf("[DEVICE-%s] 数据已上报", device.ID)
		case <-time.After(5 * time.Second):
			log.Printf("[DEVICE-%s] 通道阻塞，丢弃数据", device.ID)
		}
	}

	// 立即执行一次
	runTask()

	// 定时执行
	for {
		select {
		case <-ctx.Done():
			log.Printf("[DEVICE-%s] 任务已停止", device.ID)
			return
		case <-ticker.C:
			runTask()
		}
	}
}

func (w *DYF20AWorker) fetch(DeviceID string, auth *TokenManager, componyName string) (*base.DeviceData, error) {
	log.Printf("[FETCH] 开始获取设备 %s 数据", DeviceID)

	token := auth.GetToken()
	if token == "" {
		log.Println("[FETCH] 警告: Token为空")
	} else {
		log.Printf("[FETCH] Token获取成功: %s", maskToken(token))
	}

	apiURL := fmt.Sprintf("%s/monitor_currency?login_type=1&page=1&pageSize=10", auth.baseURL)
	log.Printf("[FETCH] 请求URL: %s", apiURL)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		log.Printf("[FETCH] 创建请求失败: %v", err)
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	log.Println("[FETCH] 发送HTTP请求...")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[FETCH] HTTP请求失败: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	log.Printf("[FETCH] HTTP响应状态: %s", resp.Status)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status: %s", resp.Status)
	}

	// 读取并解析JSON
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[FETCH] 读取响应失败: %v", err)
		return nil, err
	}
	log.Printf("[FETCH] 原始响应: %s", string(bodyBytes))

	var raw map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		log.Printf("[FETCH] JSON解析失败: %v", err)
		return nil, err
	}

	// 提取外层data对象
	outerData, ok := raw["data"].(map[string]interface{})
	if !ok {
		log.Printf("[FETCH] 错误: data字段不是对象，实际类型: %T", raw["data"])
		return nil, fmt.Errorf("data field not object")
	}

	// 提取内层data数组
	dataArr, ok := outerData["data"].([]interface{})
	if !ok {
		log.Printf("[FETCH] 错误: data.data不是数组，实际类型: %T", outerData["data"])
		return nil, fmt.Errorf("data.data not array")
	}
	if len(dataArr) == 0 {
		log.Println("[FETCH] 错误: data数组为空")
		return nil, fmt.Errorf("no data")
	}
	log.Printf("[FETCH] data数组长度: %d", len(dataArr))

	// 优先按DeviceID匹配设备数据，未匹配则回退第一条
	first, ok := selectDeviceRecord(dataArr, DeviceID)
	if !ok {
		log.Printf("[FETCH] 错误: 未找到可解析设备记录，DeviceID=%s", DeviceID)
		return nil, fmt.Errorf("no parseable device record")
	}

	// 解析data_list指标
	result := map[string]interface{}{
		"timestamp": time.Now().Format("2006-01-02 15:04:05"),
	}

	if list, ok := first["data_list"].([]interface{}); ok {
		log.Printf("[FETCH] data_list长度: %d", len(list))
		for i, v := range list {
			item, ok := v.(map[string]interface{})
			if !ok {
				log.Printf("[FETCH] 警告: data_list[%d]不是对象，跳过", i)
				continue
			}
			name, _ := item["cn_name"].(string)
			val := item["data"]
			log.Printf("[FETCH] 解析指标 %d: %s = %v", i, name, val)

			switch name {
			case "温度":
				result["temperature"] = val
			case "湿度":
				result["humidity"] = val
			}
		}
	} else {
		log.Printf("[FETCH] 警告: data_list不存在或不是数组，实际类型: %T", first["data_list"])
	}

	// 提取设备元数据
	result["power"] = first["power"]
	result["signal"] = first["xinhao"]
	result["last_jingdu"] = first["last_jingdu"]
	result["last_weidu"] = first["last_weidu"]
	log.Printf("[FETCH] 提取power: %v, signal: %v, 经度：%v, 纬度:%v", result["power"], result["signal"], result["last_jingdu"], result["last_weidu"])

	payload, err := json.Marshal(result)
	if err != nil {
		log.Printf("[FETCH] payload序列化失败: %v", err)
		return nil, err
	}
	log.Printf("[FETCH] 最终payload: %s", string(payload))

	return &base.DeviceData{
		ComponyName: componyName,
		DeviceType:  "DYF20A",
		DeviceID:    DeviceID,
		Payload:     payload,
		Timestamp:   time.Now(),
	}, nil
}

func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "****" + token[len(token)-4:]
}

func selectDeviceRecord(dataArr []interface{}, deviceID string) (map[string]interface{}, bool) {
	for _, entry := range dataArr {
		record, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}

		if matchDeviceID(record, deviceID) {
			return record, true
		}
	}

	return nil, false
}

func matchDeviceID(record map[string]interface{}, deviceID string) bool {
	targetID := normalizeDeviceID(deviceID)

	if id, ok := record["id"]; ok && normalizeDeviceID(fmt.Sprint(id)) == targetID {
		return true
	}
	if id, ok := record["device_id"]; ok && normalizeDeviceID(fmt.Sprint(id)) == targetID {
		return true
	}
	if id, ok := record["deviceId"]; ok && normalizeDeviceID(fmt.Sprint(id)) == targetID {
		return true
	}
	if id, ok := record["sn"]; ok && normalizeDeviceID(fmt.Sprint(id)) == targetID {
		return true
	}
	if id, ok := record["imei"]; ok && normalizeDeviceID(fmt.Sprint(id)) == targetID {
		return true
	}
	if id, ok := record["shebeibianhao"]; ok && normalizeDeviceID(fmt.Sprint(id)) == targetID {
		return true
	}
	return false
}

func normalizeDeviceID(id string) string {
	return strings.TrimSpace(id)
}

// 设备信息结构体
type DeviceInfo struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Interval string `json:"interval"`
	UniqueID string `json:"unique_id"`
}
