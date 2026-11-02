package service

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"gorm.io/gorm"
	ormlog "gorm.io/gorm/logger"
)

const sourceFeedGroup = "@source-feed"

var ErrExternalSubSourceDenied = errors.New("source-feed-unavailable")

// These identities keep legacy aggregators reading the centralized snapshots.
// A deliberate removal must first move the identity out of this reserved role.
var ErrSystemSubscriptionClientProtected = errors.New("system subscription reader cannot be deleted; remove its system role explicitly first")

// ExternalSourceSnapshot is the original, validated provider payload. It is
// deliberately independent of node prefixes, client templates and self-hosted
// quota headers so another aggregator can preserve its existing presentation.
type ExternalSourceSnapshot struct {
	Body      string
	UpdatedAt int64
	UserInfo  map[string]uint64
}

// SnapshotForReader is read-only even when refresh is disabled. Each format
// must have its own recent valid snapshot; universal data never impersonates
// native Clash. Missing/paused/stale data cannot be re-stamped as a new success.
func (*ExternalSubSourceService) SnapshotForReader(id, reader string, native bool) (*ExternalSourceSnapshot, error) {
	if !validExternalSourceId(id) || reader == "" || len(reader) > 256 {
		return nil, ErrExternalSubSourceDenied
	}
	now := time.Now().Unix()
	db := database.GetDB().Session(&gorm.Session{Logger: ormlog.Discard})
	var source model.ExternalSubSource
	err := db.Model(&model.ExternalSubSource{}).Select("external_sub_sources.*").
		Joins("JOIN external_sub_grants ON external_sub_grants.source_id = external_sub_sources.id").
		Joins("JOIN clients ON clients.id = external_sub_grants.client_id").
		Where("external_sub_sources.id = ? AND external_sub_sources.enabled = ? AND clients.name = ? AND clients.enable = ? AND clients.`group` = ? AND (clients.expiry = 0 OR clients.expiry > ?) AND (clients.volume <= 0 OR clients.up + clients.down <= clients.volume)", id, true, reader, true, sourceFeedGroup, now).
		First(&source).Error
	if err != nil {
		return nil, ErrExternalSubSourceDenied
	}
	var row model.ExternalSubSnapshot
	if err := db.Where("key = ?", externalSubKey(source.URL)).First(&row).Error; err != nil || row.Paused {
		return nil, ErrExternalSubCacheUnavailable
	}
	result := &ExternalSourceSnapshot{Body: row.Payload, UpdatedAt: row.LastSuccess}
	info := row.UniversalInfo
	if native {
		result.Body, result.UpdatedAt, info = row.ClashPayload, row.ClashLastSuccess, row.ClashInfo
	}
	if result.Body == "" || result.UpdatedAt <= 0 || result.UpdatedAt > now || now-result.UpdatedAt > 15*60 {
		return nil, ErrExternalSubCacheUnavailable
	}
	var values map[string]uint64
	if len(info) > 0 && json.Unmarshal(info, &values) != nil {
		return nil, ErrExternalSubCacheUnavailable
	}
	result.UserInfo = map[string]uint64{}
	for _, key := range []string{"upload", "download", "total", "expire"} {
		if value, ok := values[key]; ok && value <= 1<<53 {
			result.UserInfo[key] = value
		}
	}
	return result, nil
}
