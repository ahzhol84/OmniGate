package common

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

type DeviceIDMapping struct {
	ID             uint64    `gorm:"primaryKey;autoIncrement;column:id"`
	PluginName     string    `gorm:"column:plugin_name;type:varchar(64);not null;uniqueIndex:uk_plugin_unique"`
	UniqueID       string    `gorm:"column:unique_id;type:varchar(128);not null;uniqueIndex:uk_plugin_unique"`
	VendorDeviceID string    `gorm:"column:vendor_device_id;type:varchar(64);not null"`
	ComponyName    string    `gorm:"column:compony_name;type:varchar(128);default:''"`
	DeviceType     string    `gorm:"column:device_type;type:varchar(64);default:''"`
	LastDataType   string    `gorm:"column:last_data_type;type:varchar(64);default:''"`
	CreatedAt      time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt      time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (DeviceIDMapping) TableName() string {
	return "device_id_mapping"
}

func EnsureDeviceIDMappingTable() error {
	if DB == nil {
		return fmt.Errorf("database unavailable")
	}
	return DB.AutoMigrate(&DeviceIDMapping{})
}

func UpsertDeviceIDMapping(pluginName, uniqueID, vendorDeviceID, componyName, deviceType, dataType string) error {
	if DB == nil {
		return fmt.Errorf("database unavailable")
	}
	pluginName = strings.TrimSpace(pluginName)
	uniqueID = strings.TrimSpace(uniqueID)
	vendorDeviceID = strings.TrimSpace(vendorDeviceID)
	if pluginName == "" || uniqueID == "" || vendorDeviceID == "" {
		return nil
	}

	record := DeviceIDMapping{
		PluginName:     pluginName,
		UniqueID:       uniqueID,
		VendorDeviceID: vendorDeviceID,
		ComponyName:    strings.TrimSpace(componyName),
		DeviceType:     strings.TrimSpace(deviceType),
		LastDataType:   strings.TrimSpace(dataType),
	}

	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "plugin_name"}, {Name: "unique_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"vendor_device_id", "compony_name", "device_type", "last_data_type", "updated_at",
		}),
	}).Create(&record).Error
}

func ResolveMappedDeviceID(pluginName, uniqueID string) (string, bool, error) {
	if DB == nil {
		return "", false, fmt.Errorf("database unavailable")
	}
	pluginName = strings.TrimSpace(pluginName)
	uniqueID = strings.TrimSpace(uniqueID)
	if pluginName == "" || uniqueID == "" {
		return "", false, nil
	}

	var row DeviceIDMapping
	err := DB.Table((DeviceIDMapping{}).TableName()).
		Where("plugin_name = ? AND unique_id = ?", pluginName, uniqueID).
		Order("updated_at DESC").
		Limit(1).
		Take(&row).Error
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "record not found") {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(row.VendorDeviceID), true, nil
}
