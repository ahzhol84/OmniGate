// -- Active: 1772094146886@@127.0.0.1@3306@db_name
package common

import (
	// context 用于 Redis 命令的上下文控制。
	"context"
	"fmt"

	// os 用于读取环境变量（可覆盖默认 DSN/Redis 地址）。
	"os"
	// testing 是 Go 标准测试框架。
	"testing"
	// time 用于时间字段与 Redis 过期时间。
	"time"

	// Redis 客户端。
	"github.com/go-redis/redis/v8"
	// MySQL 驱动（给 GORM 使用）。
	"gorm.io/driver/mysql"
	// GORM ORM 库。
	"gorm.io/gorm"
)

// SimpleDeviceData 是“教学专用”的最小结构体。
// 这里不依赖项目内其他结构，方便你独立理解数据库读写。
type SimpleDeviceData struct {
	// ID 主键，自增。
	ID uint `gorm:"primaryKey"`
	// DeviceID 设备编号，并建立索引方便查询。
	DeviceID string `gorm:"size:100;index"`
	// DeviceType 设备类型。
	DeviceType string `gorm:"size:50"`
	// Payload 这里简单用字符串存 JSON 内容。
	Payload string `gorm:"type:text"`
	// CreatedAt 由 GORM 自动维护创建时间。
	CreatedAt time.Time
}

// TableName 指定测试表名，避免影响业务正式表。
func (SimpleDeviceData) TableName() string {
	return "device_data_test_simple"
}

// TestSimpleMySQLAndRedis 演示最简单链路：
// MySQL: 建表 -> 插入 -> 查询
// Redis: Set -> Get -> 比较结果
func TestSimpleMySQLAndRedis(t *testing.T) {
	// 创建一个基础上下文，供 Redis 请求使用。
	ctx := context.Background()

	// 优先读取环境变量，便于在不同机器上复用测试。
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		// 若未配置环境变量，则使用你提供的默认 DSN。
		dsn = "root:ffedcw@tcp(127.0.0.1:3306)/db_name?charset=utf8mb4&parseTime=True&loc=Local"
	}
	// Redis 地址同理，支持外部覆盖。
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		// 默认本机 Redis。
		redisAddr = "127.0.0.1:6379"
	}

	// 1) 连接 MySQL。
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		// 连接失败直接终止测试。
		t.Fatalf("connect mysql failed: %v", err)
	}
	fmt.Println("✅ Connected to MySQL successfully")
	// 2) 自动建表/迁移，确保测试表存在。
	if err := db.AutoMigrate(&SimpleDeviceData{}); err != nil {
		t.Fatalf("migrate table failed: %v", err)
	}

	// 3) 准备一条最小测试数据。
	row := SimpleDeviceData{
		DeviceID:   "dev-1001",
		DeviceType: "demo",
		Payload:    `{"temperature":25.6,"humidity":60}`,
	}
	// 4) 写入 MySQL。
	result := db.Create(&row)
	// if result.Error != nil {
	// 	t.Fatalf("mysql insert failed: %v", result.Error)
	// }
	fmt.Printf("Create result: rows=%d\n", result.RowsAffected)
	fmt.Printf("Inserted row primary key: ID=%d\n", row.ID)

	// 5) 再从 MySQL 按 device_id 查询最近一条。
	var got SimpleDeviceData
	if err := db.Where("device_id = ?", "dev-1001").Order("id desc").First(&got).Error; err != nil {
		t.Fatalf("mysql query failed: %v", err)
	}
	fmt.Printf("Queried from MySQL: ID=%d, DeviceID=%s, Payload=%s\n", got.ID, got.DeviceID, got.Payload)
	// 6) 最小断言：payload 不应为空。
	if got.Payload == "" {
		t.Fatalf("mysql query got empty payload")
	}

	// 7) 创建 Redis 客户端。
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	// 测试结束时释放连接。
	defer rdb.Close()

	// 8) 把刚从 MySQL 读到的 payload 放到 Redis，TTL=1分钟。
	if err := rdb.Set(ctx, "device:dev-1001", got.Payload, time.Minute).Err(); err != nil {
		t.Fatalf("redis set failed: %v", err)
	}
	// 9) 再从 Redis 读出来。
	val, err := rdb.Get(ctx, "device:dev-1001").Result()
	if err != nil {
		t.Fatalf("redis get failed: %v", err)
	}

	fmt.Printf("%s\n", val)

	// 10) 断言 Redis 值与 MySQL 查询值一致。
	if val != got.Payload {
		t.Fatalf("redis value mismatch, got=%s want=%s", val, got.Payload)
	}
}
