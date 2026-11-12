package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MonitoringSample 保存一轮监控快照，时间戳以 Unix 毫秒计。
type MonitoringSample struct {
	Timestamp int64  `gorm:"primaryKey;autoIncrement:false"`
	Payload   string `gorm:"type:longtext;not null"`
}

// TableName 返回监控历史表名。
func (MonitoringSample) TableName() string { return "vn_monitoring_samples" }

func (s *sqlStore) SaveMonitoring(ctx context.Context, sample *MonitoringSample, cutoff time.Time) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if sample != nil {
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(sample).Error; err != nil {
				return err
			}
		}
		if !cutoff.IsZero() {
			return tx.Where("timestamp < ?", cutoff.UnixMilli()).Delete(&MonitoringSample{}).Error
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: save monitoring: %w", err)
	}
	return nil
}

func (s *sqlStore) MonitoringHistory(ctx context.Context, since, until time.Time, limit int) ([]MonitoringSample, error) {
	if limit < 2 || limit > 120 {
		return nil, fmt.Errorf("store: monitoring limit must be 2..120")
	}
	var timestamps []int64
	if err := s.db.WithContext(ctx).Model(&MonitoringSample{}).
		Where("timestamp >= ? AND timestamp <= ?", since.UnixMilli(), until.UnixMilli()).
		Order("timestamp").Pluck("timestamp", &timestamps).Error; err != nil {
		return nil, fmt.Errorf("store: monitoring timestamps: %w", err)
	}
	if len(timestamps) > limit {
		selected := make([]int64, limit)
		for i := range selected {
			selected[i] = timestamps[i*(len(timestamps)-1)/(limit-1)]
		}
		timestamps = selected
	}
	rows := make([]MonitoringSample, 0)
	if len(timestamps) == 0 {
		return rows, nil
	}
	if err := s.db.WithContext(ctx).Where("timestamp IN ?", timestamps).Order("timestamp").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: monitoring history: %w", err)
	}
	return rows, nil
}
