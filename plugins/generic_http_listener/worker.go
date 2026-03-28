package generic_http_listener

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"iot-middleware/pkg/auth"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

type ConfigItem struct {
	ListenAddr         string `json:"listen_addr"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	AuthKey            string `json:"auth_key"`
	ComponyName        string `json:"compony_name"`
	DeviceID           string `json:"device_id"`
	UniqueID           string `json:"unique_id"`
	DeviceType         string `json:"device_type"`
	RealtimeURL        string `json:"realtime_url"`
	RealtimeTTLSeconds int    `json:"realtime_ttl_seconds"`
	HistoryTTLSeconds  int    `json:"history_ttl_seconds"`
}

type GenericHTTPListener struct {
	configs []ConfigItem
	servers []*http.Server
}

func (w *GenericHTTPListener) Init(configs []json.RawMessage) error {
	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d]: %v", i, err)
		}
		if cfg.ListenAddr == "" {
			cfg.ListenAddr = ":8080"
		}
		if cfg.RealtimeTTLSeconds <= 0 {
			cfg.RealtimeTTLSeconds = 120
		}
		if cfg.HistoryTTLSeconds <= 0 {
			cfg.HistoryTTLSeconds = 60
		}
		w.configs = append(w.configs, cfg)
	}
	return nil
}

func (w *GenericHTTPListener) Start(ctx context.Context, out chan<- *base.DeviceData) {
	var wg sync.WaitGroup

	for i, cfg := range w.configs {
		wg.Add(1)
		go func(idx int, c ConfigItem) {
			defer wg.Done()
			w.run(ctx, out, c, idx)
		}(i, cfg)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-ctx.Done():
		for _, s := range w.servers {
			if s != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.Shutdown(shutdownCtx)
				cancel()
			}
		}
		<-done
	case <-done:
	}
}

func (w *GenericHTTPListener) run(ctx context.Context, out chan<- *base.DeviceData, cfg ConfigItem, index int) {
	validator, err := auth.NewValidator(auth.AuthConfig{
		Username: cfg.Username,
		Password: cfg.Password,
		AuthKey:  cfg.AuthKey,
	}, common.RDB, fmt.Sprintf("responder-%d", index+1))

	if err != nil {
		log.Fatalf("[RESPONDER-%d] 授权初始化失败: %v", index+1, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/login", validator.LoginHandler())
	mux.HandleFunc("/device/realtime", validator.AuthMiddleware(func(rw http.ResponseWriter, r *http.Request) {
		if common.DB == nil {
			http.Error(rw, "database not initialized", http.StatusInternalServerError)
			return
		}

		requestedID := strings.TrimSpace(r.URL.Query().Get("device_id"))
		if requestedID == "" {
			http.Error(rw, "device_id is required", http.StatusBadRequest)
			return
		}
		normalizedID, sourceDeviceID, err := normalizeDeviceID(cfg, requestedID)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		requestedType := strings.TrimSpace(r.URL.Query().Get("device_type"))

		canDirectRequest := cfg.RealtimeURL != "" &&
			requestedType != "" &&
			cfg.DeviceType != "" &&
			strings.EqualFold(requestedType, cfg.DeviceType)

		if canDirectRequest {
			deviceUniqueID := common.ResolveDeviceUniqueID(
				cfg.UniqueID,
				cfg.ComponyName,
				"generic_http_listener",
				requestedType,
				sourceDeviceID,
			)

			payloadBytes, err := requestRealtimePayload(r.Context(), cfg.RealtimeURL, sourceDeviceID)
			if err == nil {
				if common.RDB != nil {
					cacheKey := realtimeCacheKey(normalizedID)
					_ = common.RDB.Set(r.Context(), cacheKey, payloadBytes, time.Duration(cfg.RealtimeTTLSeconds)*time.Second).Err()
				}

				select {
				case out <- &base.DeviceData{
					DeviceID:    deviceUniqueID,
					UniqueID:    deviceUniqueID,
					DeviceType:  requestedType,
					DataType:    "REALTIME_QUERY",
					Timestamp:   time.Now(),
					Payload:     payloadBytes,
					ComponyName: cfg.ComponyName,
				}:
				case <-time.After(2 * time.Second):
					log.Printf("[RESPONDER-%d] realtime query write channel timeout, device=%s", index+1, requestedID)
				}

				writeJSON(rw, http.StatusOK, map[string]interface{}{
					"device_id":   normalizedID,
					"unique_id":   deviceUniqueID,
					"source_id":   sourceDeviceID,
					"device_type": requestedType,
					"source":      "direct_request",
					"payload":     toJSONObject(payloadBytes),
					"timestamp":   time.Now(),
				})
				return
			}
			log.Printf("[RESPONDER-%d] direct request failed, fallback to cache/db, device=%s, err=%v", index+1, requestedID, err)
		}

		if common.RDB != nil {
			cacheKey := realtimeCacheKey(normalizedID)
			if raw, err := common.RDB.Get(r.Context(), cacheKey).Bytes(); err == nil {
				writeJSON(rw, http.StatusOK, map[string]interface{}{
					"device_id": normalizedID,
					"source":    "redis",
					"payload":   toJSONObject(raw),
				})
				return
			} else if err != redis.Nil {
				log.Printf("[RESPONDER-%d] redis get realtime failed: %v", index+1, err)
			}
		}

		type latestRow struct {
			Payload    string `gorm:"column:payload"`
			DeviceID   string `gorm:"column:device_id"`
			UniqueID   string `gorm:"column:unique_id"`
			DeviceType string `gorm:"column:device_type"`
			Timestamp  string `gorm:"column:timestamp"`
		}

		var latest latestRow
		result := common.DB.Table("device_data").
			Select("payload", "device_id", "unique_id", "device_type", "timestamp").
			Where("unique_id = ? OR device_id = ?", normalizedID, normalizedID).
			Order("timestamp DESC").
			Order("id DESC").
			Limit(1).
			Scan(&latest)

		if result.Error != nil {
			http.Error(rw, "failed to query latest payload", http.StatusInternalServerError)
			return
		}
		if result.RowsAffected == 0 {
			http.Error(rw, "no device data found", http.StatusNotFound)
			return
		}

		if common.RDB != nil {
			cacheKey := realtimeCacheKey(normalizedID)
			_ = common.RDB.Set(r.Context(), cacheKey, latest.Payload, time.Duration(cfg.RealtimeTTLSeconds)*time.Second).Err()
		}

		if strings.TrimSpace(latest.UniqueID) == "" {
			latest.UniqueID = strings.TrimSpace(latest.DeviceID)
		}

		writeJSON(rw, http.StatusOK, map[string]interface{}{
			"device_id":   normalizedID,
			"unique_id":   latest.UniqueID,
			"device_type": latest.DeviceType,
			"source":      "db_latest",
			"payload":     toJSONObject([]byte(latest.Payload)),
			"timestamp":   latest.Timestamp,
		})
	}))

	mux.HandleFunc("/device/history/simpolist", validator.AuthMiddleware(func(rw http.ResponseWriter, r *http.Request) {
		if common.DB == nil {
			http.Error(rw, "database not initialized", http.StatusInternalServerError)
			return
		}

		requestedID := strings.TrimSpace(r.URL.Query().Get("device_id"))
		if requestedID == "" {
			http.Error(rw, "device_id is required", http.StatusBadRequest)
			return
		}
		normalizedID, _, err := normalizeDeviceID(cfg, requestedID)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}

		page := parsePositiveInt(r.URL.Query().Get("page"), 1)
		pageSize := parsePositiveInt(r.URL.Query().Get("page_size"), 20)
		if pageSize > 200 {
			pageSize = 200
		}

		cacheKey := historyCacheKey(normalizedID, page, pageSize)
		if common.RDB != nil {
			if raw, err := common.RDB.Get(r.Context(), cacheKey).Bytes(); err == nil {
				rw.Header().Set("Content-Type", "application/json; charset=utf-8")
				rw.WriteHeader(http.StatusOK)
				_, _ = rw.Write(raw)
				return
			} else if err != redis.Nil {
				log.Printf("[RESPONDER-%d] redis get history failed: %v", index+1, err)
			}
		}

		type historyRow struct {
			ID         uint64 `gorm:"column:id" json:"id"`
			DeviceID   string `gorm:"column:device_id" json:"device_id"`
			UniqueID   string `gorm:"column:unique_id" json:"unique_id"`
			DeviceType string `gorm:"column:device_type" json:"device_type"`
			DataType   string `gorm:"column:data_type" json:"data_type"`
			Compony    string `gorm:"column:compony_name" json:"compony_name"`
			Timestamp  string `gorm:"column:timestamp" json:"timestamp"`
			Payload    string `gorm:"column:payload" json:"-"`
		}

		baseQuery := common.DB.Table("device_data").Where("unique_id = ? OR device_id = ?", normalizedID, normalizedID)

		var total int64
		if err := baseQuery.Count(&total).Error; err != nil {
			http.Error(rw, "failed to count history", http.StatusInternalServerError)
			return
		}

		var rows []historyRow
		if err := common.DB.Table("device_data").
			Select("id", "device_id", "unique_id", "device_type", "data_type", "compony_name", "timestamp", "payload").
			Where("unique_id = ? OR device_id = ?", normalizedID, normalizedID).
			Order("timestamp DESC").
			Order("id DESC").
			Offset((page - 1) * pageSize).
			Limit(pageSize).
			Find(&rows).Error; err != nil {
			http.Error(rw, "failed to query history", http.StatusInternalServerError)
			return
		}

		items := make([]map[string]interface{}, 0, len(rows))
		for _, row := range rows {
			if strings.TrimSpace(row.UniqueID) == "" {
				row.UniqueID = strings.TrimSpace(row.DeviceID)
			}
			items = append(items, map[string]interface{}{
				"id":           row.ID,
				"device_id":    row.DeviceID,
				"unique_id":    row.UniqueID,
				"device_type":  row.DeviceType,
				"data_type":    row.DataType,
				"compony_name": row.Compony,
				"timestamp":    row.Timestamp,
				"payload":      toJSONObject([]byte(row.Payload)),
			})
		}

		resp := map[string]interface{}{
			"device_id": normalizedID,
			"page":      page,
			"page_size": pageSize,
			"total":     total,
			"items":     items,
			"source":    "db",
		}

		respBytes, _ := json.Marshal(resp)
		if common.RDB != nil {
			_ = common.RDB.Set(r.Context(), cacheKey, respBytes, time.Duration(cfg.HistoryTTLSeconds)*time.Second).Err()
		}

		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write(respBytes)
	}))

	mux.HandleFunc("/device/history/simplelist", validator.AuthMiddleware(func(rw http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/device/history/simpolist"
		mux.ServeHTTP(rw, r)
	}))

	mux.HandleFunc("/hello", validator.AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if common.DB == nil {
			http.Error(w, "database not initialized", http.StatusInternalServerError)
			return
		}

		var latest struct {
			//     `payload` text COLLATE utf8mb4_general_ci,
			//     `device_id` varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
			Payload  string `gorm:"column:payload"`
			DeviceID string `gorm:"column:device_id"`
		}

		result := common.DB.Table("device_data").
			Select("payload", "device_id").
			Order("id DESC").
			Limit(1).
			Scan(&latest)

		if result.Error != nil {
			http.Error(w, "failed to query latest payload", http.StatusInternalServerError)
			return
		}
		if result.RowsAffected == 0 {
			http.Error(w, "no device data found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"device_id": latest.DeviceID,
			"payload":   json.RawMessage(latest.Payload), // payload是JSON字符串时可直接嵌入
		})
	}))

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	w.servers = append(w.servers, srv)

	go func() {
		log.Printf("[RESPONDER-%d] 正在听 %s", index+1, cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[RESPONDER-%d] error: %v", index+1, err)
		}
	}()

	<-ctx.Done()
	srv.Shutdown(context.Background())
}

func parsePositiveInt(raw string, defaultValue int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return defaultValue
	}
	return value
}

func realtimeCacheKey(deviceID string) string {
	return "goiot:realtime:" + strings.TrimSpace(deviceID)
}

func historyCacheKey(deviceID string, page, pageSize int) string {
	return fmt.Sprintf("goiot:history:simpolist:%s:%d:%d", strings.TrimSpace(deviceID), page, pageSize)
}

func normalizeDeviceID(cfg ConfigItem, requestedID string) (normalizedID string, sourceDeviceID string, err error) {
	requestedID = strings.TrimSpace(requestedID)
	if requestedID == "" {
		return "", "", fmt.Errorf("device_id is required")
	}

	configuredNumericID := common.ResolveDeviceUniqueID(
		cfg.UniqueID,
		cfg.ComponyName,
		"generic_http_listener",
		cfg.DeviceType,
		cfg.DeviceID,
	)

	if common.IsNumericDeviceID(requestedID) {
		if configuredNumericID != "" && requestedID == configuredNumericID {
			return configuredNumericID, strings.TrimSpace(cfg.DeviceID), nil
		}
		return requestedID, requestedID, nil
	}

	if strings.TrimSpace(cfg.DeviceID) != "" && requestedID == strings.TrimSpace(cfg.DeviceID) {
		return configuredNumericID, strings.TrimSpace(cfg.DeviceID), nil
	}

	return "", "", fmt.Errorf("device_id must be numeric unique id")
}

func requestRealtimePayload(ctx context.Context, realtimeURL string, deviceID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realtimeURL, nil)
	if err != nil {
		return nil, err
	}
	query := req.URL.Query()
	query.Set("device_id", strings.TrimSpace(deviceID))
	req.URL.RawQuery = query.Encode()

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("realtime request failed, status=%s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return []byte(`{}`), nil
	}
	return body, nil
}

func toJSONObject(raw []byte) interface{} {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return map[string]interface{}{}
	}

	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		return parsed
	}

	return map[string]interface{}{"raw": string(raw)}
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func init() {
	plugin.Register("generic_http_listener", func() base.IWorker {
		return &GenericHTTPListener{}
	})

	plugin.Register("generic_tcp_listener", func() base.IWorker {
		return &GenericHTTPListener{}
	})
}
