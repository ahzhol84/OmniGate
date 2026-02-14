package common

import (
	"fmt"
	"log"
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
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		fmt.Printf("数据库连接失败: %v\n", err)
		return
	}
	DB = db
	fmt.Println("✅ 数据库连接成功")
}

func StartDataWriter(dataChan <-chan *base.DeviceData) {
	log.Println("[WRITER] 数据写入协程启动")
	var batch []base.DeviceData
	counter := 0
	flushInterval := 30 * time.Second // 30秒强制刷盘

	//定时闹钟
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop() //退出时关掉闹钟

	flush := func(reason string) {
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
		case data, ok := <-dataChan:
			if !ok {
				flush("通道关闭")
				log.Println("[WRITER] 数据写入协程结束")
				return
			}
			counter++
			log.Printf("[WRITER] 收到数据 #%d, DeviceType: %s, DeviceID: %s",
				counter, data.DeviceType, data.DeviceID)
			batch = append(batch, *data)

			if len(batch) >= 50 {
				flush("批次满")
			}

		case <-ticker.C:
			flush("超时强制刷新")
		}
	}
}
