package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/util"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrExternalSubCacheUnavailable = errors.New("external-subscription-snapshot-unavailable")

var externalSubRefreshMu sync.Mutex

type ExternalSubCacheConfig struct {
	Enabled        bool `json:"enabled"`
	AutoRefresh    bool `json:"autoRefresh"`
	RefreshMinutes int  `json:"refreshMinutes"`
}

type ExternalSubCacheStatus struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	References  int    `json:"references"`
	Ready       bool   `json:"ready"`
	LastSuccess int64  `json:"lastSuccess"`
	LastAttempt int64  `json:"lastAttempt"`
	NextAttempt int64  `json:"nextAttempt"`
	LastError   string `json:"lastError"`
	Paused      bool   `json:"paused"`
	Requested   bool   `json:"requested"`
	Refreshing  bool   `json:"refreshing"`
}

type externalSubReference struct {
	url        string
	name       string
	references int
}

type ExternalSubCacheService struct {
	now   func() time.Time
	fetch func(context.Context, string, func() error) (externalSubFetchResult, error)
}

func (s *ExternalSubCacheService) clock() int64 {
	if s.now != nil {
		return s.now().Unix()
	}
	return time.Now().Unix()
}

func externalSubKey(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

func (s *ExternalSubCacheService) Config() (ExternalSubCacheConfig, error) {
	config := ExternalSubCacheConfig{RefreshMinutes: 5}
	// LinkService is also used as a standalone converter, without a panel DB.
	if database.GetDB() == nil {
		return config, nil
	}
	var settings SettingService
	var err error
	config.Enabled, err = settings.getBool("subCacheEnabled")
	if err != nil {
		return config, err
	}
	config.AutoRefresh, err = settings.getBool("subCacheAutoRefresh")
	if err != nil {
		return config, err
	}
	config.RefreshMinutes, err = settings.getInt("subCacheRefreshMinutes")
	if err != nil || config.RefreshMinutes < 5 || config.RefreshMinutes > 1440 {
		return config, errors.New("cache-refresh-interval-must-be-5-to-1440-minutes")
	}
	return config, nil
}

// Resolve never contacts an upstream when caching is enabled. A failed cache
// read is deliberately not a reason to fall back to a live provider request.
func (s *ExternalSubCacheService) Resolve(raw string) (string, bool, error) {
	config, err := s.Config()
	if err != nil {
		return "", true, ErrExternalSubCacheUnavailable
	}
	if !config.Enabled {
		return util.GetExternalLink(raw), false, nil
	}
	var snapshot model.ExternalSubSnapshot
	err = database.GetDB().Where("key = ?", externalSubKey(raw)).First(&snapshot).Error
	if err != nil || snapshot.Payload == "" || snapshot.LastSuccess == 0 {
		return "", true, ErrExternalSubCacheUnavailable
	}
	return snapshot.Payload, true, nil
}

func activeExternalSubs(db *gorm.DB, now int64) (map[string]externalSubReference, error) {
	var clients []model.Client
	err := db.Select("links").Where("enable = ? AND (expiry = 0 OR expiry > ?)", true, now).Find(&clients).Error
	if err != nil {
		return nil, err
	}
	result := map[string]externalSubReference{}
	for _, client := range clients {
		var links []struct {
			Type   string `json:"type"`
			Uri    string `json:"uri"`
			Remark string `json:"remark"`
		}
		if json.Unmarshal(client.Links, &links) != nil {
			continue
		}
		seen := map[string]bool{}
		for _, link := range links {
			if link.Type != "sub" || link.Uri == "" {
				continue
			}
			key := externalSubKey(link.Uri)
			if seen[key] {
				continue
			}
			seen[key] = true
			ref := result[key]
			ref.url = link.Uri
			if ref.name == "" {
				ref.name = link.Remark
			}
			ref.references++
			result[key] = ref
		}
	}
	return result, nil
}

func (s *ExternalSubCacheService) Status() ([]ExternalSubCacheStatus, error) {
	now := s.clock()
	refs, err := activeExternalSubs(database.GetDB(), now)
	if err != nil {
		return nil, err
	}
	var snapshots []model.ExternalSubSnapshot
	if err = database.GetDB().Omit("payload", "lease").Find(&snapshots).Error; err != nil {
		return nil, err
	}
	stored := map[string]model.ExternalSubSnapshot{}
	for _, row := range snapshots {
		stored[row.Key] = row
	}
	var readyKeys []string
	if err := database.GetDB().Model(&model.ExternalSubSnapshot{}).
		Where("last_success > 0 AND length(payload) > 0").Pluck("key", &readyKeys).Error; err != nil {
		return nil, err
	}
	ready := map[string]bool{}
	for _, key := range readyKeys {
		ready[key] = true
	}
	result := make([]ExternalSubCacheStatus, 0, len(refs))
	for key, ref := range refs {
		row := stored[key]
		host := ""
		if parsed, err := url.Parse(ref.url); err == nil {
			host = parsed.Hostname()
		}
		result = append(result, ExternalSubCacheStatus{
			Key: key, Name: ref.name, Host: host, References: ref.references,
			Ready: ready[key], LastSuccess: row.LastSuccess, LastAttempt: row.LastAttempt,
			NextAttempt: row.NextAttempt, LastError: row.LastError, Paused: row.Paused,
			Requested: row.Requested, Refreshing: row.LockUntil > now,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

func (s *ExternalSubCacheService) Configure(config ExternalSubCacheConfig, actor string) error {
	if config.RefreshMinutes < 5 || config.RefreshMinutes > 1440 {
		return errors.New("cache-refresh-interval-must-be-5-to-1440-minutes")
	}
	if config.Enabled {
		status, err := s.Status()
		if err != nil {
			return err
		}
		for _, row := range status {
			if !row.Ready {
				return errors.New("warm-all-active-sources-before-enabling-cache")
			}
		}
	}
	data, _ := json.Marshal(config)
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		for key, value := range map[string]string{
			"subCacheEnabled":        strconv.FormatBool(config.Enabled),
			"subCacheAutoRefresh":    strconv.FormatBool(config.AutoRefresh),
			"subCacheRefreshMinutes": strconv.Itoa(config.RefreshMinutes),
		} {
			if err := upsertSetting(tx, key, value); err != nil {
				return err
			}
		}
		return tx.Create(&model.Changes{DateTime: s.clock(), Actor: actor, Key: "subscription-cache", Action: "configure", Obj: data}).Error
	})
	if err == nil {
		SetLastUpdate(s.clock())
	}
	return err
}

// Queue is safe to call repeatedly: it never clears a cooldown or a live
// lease. Prewarming is possible with automatic refresh and cache reads off.
func (s *ExternalSubCacheService) Queue(keys []string, resume bool) error {
	refs, err := activeExternalSubs(database.GetDB(), s.clock())
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		for key := range refs {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		ref, ok := refs[key]
		if !ok {
			return errors.New("source-not-assigned-to-an-active-client")
		}
		if err := validateCachedSubURL(ref.url); err != nil {
			return err
		}
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		for _, key := range keys {
			row := model.ExternalSubSnapshot{Key: key}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
			if err := tx.Where("key = ?", key).First(&row).Error; err != nil {
				return err
			}
			if row.Paused && !resume {
				return errors.New("source-paused-use-explicit-resume")
			}
			values := map[string]interface{}{"requested": true}
			if resume {
				values["paused"] = false
			}
			if err := tx.Model(&model.ExternalSubSnapshot{}).Where("key = ?", key).Updates(values).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func reserveExternalSubAttempt(db *gorm.DB, now int64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.ExternalSubAttempt{}).Where("at > ?", now-60).Count(&count).Error; err != nil {
			return err
		}
		if count >= 4 {
			return errSubRequestBudget
		}
		if err := tx.Where("at <= ?", now-60).Delete(&model.ExternalSubAttempt{}).Error; err != nil {
			return err
		}
		return tx.Create(&model.ExternalSubAttempt{At: now}).Error
	})
}

// RefreshOne runs at most one assigned source per poll. The DB lease fences
// overlapping processes as well as the in-process cron wrapper.
func (s *ExternalSubCacheService) RefreshOne(ctx context.Context) error {
	if !externalSubRefreshMu.TryLock() {
		return nil
	}
	defer externalSubRefreshMu.Unlock()
	config, err := s.Config()
	if err != nil {
		return err
	}
	now := s.clock()
	refs, err := activeExternalSubs(database.GetDB(), now)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ref := refs[key]
		row := model.ExternalSubSnapshot{Key: key}
		if config.AutoRefresh {
			if err := database.GetDB().Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
		}
		lease := rand.Text()
		query := database.GetDB().Model(&model.ExternalSubSnapshot{}).
			Where("key = ? AND paused = ? AND next_attempt <= ? AND lock_until <= ?", key, false, now, now)
		if !config.AutoRefresh {
			// A crashed manual job has already consumed its queue flag. Its
			// expired lease makes it eligible again after a panel restart.
			query = query.Where("requested = ? OR (lease <> ? AND lock_until <= ?)", true, "", now)
		}
		claim := query.Updates(map[string]interface{}{"lease": lease, "lock_until": now + 30, "last_attempt": now, "requested": false})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			continue
		}
		if err := database.GetDB().Where("key = ?", key).First(&row).Error; err != nil {
			return err
		}
		fetch := s.fetch
		if fetch == nil {
			fetch = fetchCachedExternalSub
		}
		result, fetchErr := fetch(ctx, ref.url, func() error { return reserveExternalSubAttempt(database.GetDB(), s.clock()) })
		if fetchErr == nil {
			fetchErr = validateCachedSubBody(result.body)
		}
		finished := s.clock()
		next := now + int64(config.RefreshMinutes)*60
		failures := 0
		budgetWait := errors.Is(fetchErr, errSubRequestBudget)
		if budgetWait {
			failures = row.Failures
			next = finished + 60
		} else if fetchErr != nil {
			failures = min(max(row.Failures, 0)+1, 10)
			// Repeated failures back off, rather than hammer a suspended source.
			delay := min(int64(config.RefreshMinutes)*60*(1<<(failures-1)), 6*60*60)
			next = finished + delay
		}
		if result.retryAt > next {
			next = result.retryAt
		}
		return database.GetDB().Transaction(func(tx *gorm.DB) error {
			// The URL may have been removed or its last client disabled during
			// the fetch. Never publish a now-unassigned source's response.
			current, err := activeExternalSubs(tx, finished)
			if err != nil {
				return err
			}
			values := map[string]interface{}{"lease": "", "lock_until": 0, "next_attempt": next, "failures": failures}
			if budgetWait {
				// Local budget contention is not an upstream failure. Keep a
				// manual prewarm queued until a request can actually be made.
				values["requested"] = true
			}
			if _, stillAssigned := current[key]; !stillAssigned {
				values["last_error"] = "source-unassigned"
				values["requested"] = false
			} else if fetchErr != nil {
				values["last_error"] = safeExternalSubError(fetchErr)
				values["paused"] = result.status == 401 || result.status == 403
			} else {
				values["payload"] = util.DecodeExternalSubBody(result.body)
				values["last_success"] = finished
				values["last_error"] = ""
			}
			return tx.Model(&model.ExternalSubSnapshot{}).Where("key = ? AND lease = ? AND lock_until >= ?", key, lease, finished).Updates(values).Error
		})
	}
	return nil
}

func safeExternalSubError(err error) string {
	// Fetchers are internal, but keeping this allowlist also makes future
	// collector adapters unable to persist URL-bearing errors by accident.
	if errors.Is(err, errSubRequestBudget) {
		return errSubRequestBudget.Error()
	}
	allowed := map[string]bool{
		"source-requires-public-https": true, "source-address-not-public-ipv4": true,
		"source-request-invalid": true, "source-request-failed": true,
		"source-content-encoding-unsupported": true, "source-body-read-failed": true,
		"source-body-invalid-or-too-large": true, "source-content-invalid": true,
	}
	if allowed[err.Error()] {
		return err.Error()
	}
	for status := 100; status <= 599; status++ {
		if err.Error() == "source-http-"+strconv.Itoa(status) {
			return err.Error()
		}
	}
	return "source-refresh-failed"
}
