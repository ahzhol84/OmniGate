package common

import (
	"fmt"
	"log"
	"strings"
	"time"

	"iot-middleware/pkg/base"

	"github.com/go-redis/redis/v8"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var DB *gorm.DB

var RDB *redis.Client // 首字母大写，允许被插件调用
func InitRedis(addr string) {
	RDB = redis.NewClient(&redis.Options{
		// Addr: "127.0.0.1:6379",
		Addr: addr,
	})
}
func InitDB(dns string) {
	// dsn := "root:ffedcw@tcp(127.0.0.1:3306)/db_name?charset=utf8mb4&parseTime=True&loc=Local"
	dsn := dns
	if !strings.Contains(strings.ToLower(dsn), "parsetime=") {
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		dsn = dsn + separator + "parseTime=True&loc=Local"
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		fmt.Printf("数据库连接失败: %v\n", err)
		return
	}
	DB = db
	fmt.Println("✅ 数据库连接成功")
	migrateLegacyDeviceIDs()
}

func migrateLegacyDeviceIDs() {
	if DB == nil {
		return
	}

	type legacyRow struct {
		ID         uint64 `gorm:"column:id"`
		DeviceID   string `gorm:"column:device_id"`
		UniqueID   string `gorm:"column:unique_id"`
		DeviceType string `gorm:"column:device_type"`
		Compony    string `gorm:"column:compony_name"`
	}

	var rows []legacyRow
	err := DB.Table("device_data").
		Select("id", "device_id", "unique_id", "device_type", "compony_name").
		Where("device_id IS NOT NULL").
		Find(&rows).Error
	if err != nil {
		log.Printf("[MIGRATE] load legacy device ids failed: %v", err)
		return
	}

	updated := 0
	for _, row := range rows {
		currentDeviceID := strings.TrimSpace(row.DeviceID)
		currentUniqueID := strings.TrimSpace(row.UniqueID)

		if IsNumericDeviceID(currentDeviceID) && IsNumericDeviceID(currentUniqueID) {
			continue
		}

		explicit := currentUniqueID
		if explicit == "" {
			explicit = currentDeviceID
		}

		sourceID := currentDeviceID
		if strings.Contains(sourceID, "|") {
			sourceID = ""
		}

		numericID := ResolveDeviceUniqueID(explicit, row.Compony, "", row.DeviceType, sourceID)
		if !IsNumericDeviceID(numericID) {
			continue
		}

		if err := DB.Table("device_data").
			Where("id = ?", row.ID).
			Updates(map[string]interface{}{
				"device_id": numericID,
				"unique_id": numericID,
			}).Error; err != nil {
			log.Printf("[MIGRATE] update row id=%d failed: %v", row.ID, err)
			continue
		}
		updated++
	}

	if updated > 0 {
		log.Printf("[MIGRATE] legacy device ids normalized: %d rows", updated)
	}
}

func StartDataWriter(dataChan <-chan *base.DeviceData) {
	log.Println("[WRITER] 数据写入协程启动")
	// batch 用来暂存“已经从通道取出来”的数据，达到条件后再统一写库。
	var batch []base.DeviceData
	counter := 0
	flushInterval := 30 * time.Second // 30秒强制刷盘

	//定时闹钟
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop() //退出时关掉闹钟

	// flushBatch 只是“写库动作”的定义，真正执行发生在下面 select 的调用点。
	// 阅读顺序请按 select 看：先取通道数据 -> append 到 batch -> 满批/超时触发写库。
	flushBatch := func(reason string) {
		if len(batch) == 0 {
			return
		}
		log.Printf("[WRITER] %s，写入 %d 条记录...", reason, len(batch))
		if err := DB.CreateInBatches(batch, len(batch)).Error; err != nil {
			log.Printf("[WRITER] 写入失败: %v", err)
		} else {
			log.Printf("[WRITER] 写入完成")
		}
		batch = nil
	}

	for {
		select {
		// 1) 先从通道取值（生产者写入的 *DeviceData）。
		case data, ok := <-dataChan:
			if !ok {
				flushBatch("通道关闭")
				log.Println("[WRITER] 数据写入协程结束")
				return
			}
			if strings.TrimSpace(data.UniqueID) == "" {
				data.UniqueID = strings.TrimSpace(data.DeviceID)
			}
			counter++
			log.Printf("[WRITER] 收到数据 #%d, DeviceType: %s, DeviceID: %s",
				counter, data.DeviceType, data.DeviceID)
			// 2) 把指针解引用成值，放入 batch 暂存。
			batch = append(batch, *data)

			// 3) 满批后再落库。
			if len(batch) >= 50 {
				flushBatch("批次满")
			}

		// 4) 超时也会强制落库，避免小流量场景数据长期滞留在内存。
		case <-ticker.C:
			flushBatch("超时强制刷新")
		}
	}
}
