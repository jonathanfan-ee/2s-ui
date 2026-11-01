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
	ormlog "gorm.io/gorm/logger"
)

var ErrExternalSubCacheUnavailable = errors.New("external-subscription-snapshot-unavailable")

var externalSubRefreshMu sync.Mutex

type ExternalSubCacheConfig struct {
	Enabled        bool `json:"enabled"`
	AutoRefresh    bool `json:"autoRefresh"`
	RefreshMinutes int  `json:"refreshMinutes"`
}

type ExternalSubCacheStatus struct {
	Key              string `json:"key"`
	Name             string `json:"name"`
	Host             string `json:"host"`
	References       int    `json:"references"`
	Ready            bool   `json:"ready"`
	UniversalReady   bool   `json:"universalReady"`
	ClashReady       bool   `json:"clashReady"`
	ClashLastSuccess int64  `json:"clashLastSuccess"`
	UniversalError   string `json:"universalError"`
	ClashError       string `json:"clashError"`
	LastSuccess      int64  `json:"lastSuccess"`
	LastAttempt      int64  `json:"lastAttempt"`
	NextAttempt      int64  `json:"nextAttempt"`
	LastError        string `json:"lastError"`
	Paused           bool   `json:"paused"`
	Requested        bool   `json:"requested"`
	Refreshing       bool   `json:"refreshing"`
}

type externalSubReference struct {
	url           string
	name          string
	references    int
	collectorId   string
	sourceId      string
	revision      uint64
	routeEnabled  bool
	routeConflict bool
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
func (s *ExternalSubCacheService) Resolve(raw string, native ...bool) (string, bool, error) {
	config, err := s.Config()
	if err != nil {
		return "", true, ErrExternalSubCacheUnavailable
	}
	if !config.Enabled {
		owned := false
		if database.GetDB() != nil {
			var routeErr error
			owned, routeErr = collectorOwnedURL(raw)
			if routeErr != nil {
				return "", true, ErrExternalSubCacheUnavailable
			}
		}
		if !owned {
			return util.GetExternalLink(raw), false, nil
		}
	}
	body, err := s.snapshot(raw, native...)
	return body, true, err
}

func (s *ExternalSubCacheService) snapshot(raw string, native ...bool) (string, error) {
	var row model.ExternalSubSnapshot
	err := database.GetDB().Where("key = ?", externalSubKey(raw)).First(&row).Error
	if err != nil {
		return "", ErrExternalSubCacheUnavailable
	}
	if len(native) > 0 && native[0] && row.ClashPayload != "" && row.ClashLastSuccess > 0 {
		return row.ClashPayload, nil
	}
	if row.Payload == "" || row.LastSuccess == 0 {
		return "", ErrExternalSubCacheUnavailable
	}
	return row.Payload, nil
}

// Enabled catalog sources can be prewarmed without a user. Only sources with
// active consumers are scheduled automatically. Legacy URLs retain their scope.
func activeExternalSubs(db *gorm.DB, now int64) (map[string]externalSubReference, error) {
	var clients []model.Client
	if err := db.Select("id, links").Where("enable = ? AND (expiry = 0 OR expiry > ?)", true, now).Find(&clients).Error; err != nil {
		return nil, err
	}
	refs := map[string]externalSubReference{}
	seenByClient := map[uint]map[string]bool{}
	for _, client := range clients {
		seen := map[string]bool{}
		seenByClient[client.Id] = seen
		var links []struct {
			Type   string `json:"type"`
			Uri    string `json:"uri"`
			Remark string `json:"remark"`
		}
		if json.Unmarshal(client.Links, &links) != nil {
			continue
		}
		for _, link := range links {
			if link.Type != "sub" || link.Uri == "" {
				continue
			}
			key := externalSubKey(link.Uri)
			ref := refs[key]
			ref.url = link.Uri
			if ref.name == "" {
				ref.name = link.Remark
			}
			refs[key] = ref
			seen[key] = true
		}
	}
	var sources []model.ExternalSubSource
	if err := db.Order("id").Find(&sources).Error; err != nil {
		return nil, err
	}
	byId := map[string]string{}
	for _, source := range sources {
		if !source.Enabled {
			continue
		}
		key := externalSubKey(source.URL)
		ref := refs[key]
		ref.url, ref.name = source.URL, source.Name
		refs[key] = ref
		byId[source.Id] = key
	}
	declared := map[string]bool{}
	for _, source := range sources {
		key := externalSubKey(source.URL)
		ref, exists := refs[key]
		if !exists {
			continue
		}
		if declared[key] && ref.collectorId != source.CollectorId {
			ref.routeConflict = true
		}
		if !declared[key] {
			ref.collectorId = source.CollectorId
		}
		declared[key] = true
		if ref.sourceId == "" || !ref.routeEnabled && source.Enabled {
			ref.sourceId = source.Id
			ref.revision = source.Revision
			ref.routeEnabled = source.Enabled
		}
		refs[key] = ref
	}
	var grants []model.ExternalSubGrant
	if err := db.Find(&grants).Error; err != nil {
		return nil, err
	}
	for _, grant := range grants {
		key, enabled := byId[grant.SourceId]
		if seen, active := seenByClient[grant.ClientId]; active && enabled {
			seen[key] = true
		}
	}
	for _, seen := range seenByClient {
		for key := range seen {
			ref := refs[key]
			ref.references++
			refs[key] = ref
		}
	}
	return refs, nil
}

// Metadata queries never load payloads into an admin status object.
func externalSubSnapshotMetadata(db *gorm.DB, now int64) (map[string]ExternalSubCacheStatus, error) {
	var rows []ExternalSubCacheStatus
	err := db.Model(&model.ExternalSubSnapshot{}).Select("key, last_success, clash_last_success, universal_error, clash_error, last_attempt, next_attempt, last_error, paused, requested, ((last_success > 0 AND length(payload) > 0) OR (clash_last_success > 0 AND length(clash_payload) > 0)) AS ready, (last_success > 0 AND length(payload) > 0) AS universal_ready, (clash_last_success > 0 AND length(clash_payload) > 0) AS clash_ready, (lock_until > ?) AS refreshing", now).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := map[string]ExternalSubCacheStatus{}
	for _, row := range rows {
		result[row.Key] = row
	}
	return result, nil
}

func (s *ExternalSubCacheService) Status() ([]ExternalSubCacheStatus, error) {
	now := s.clock()
	refs, err := activeExternalSubs(database.GetDB(), now)
	if err != nil {
		return nil, err
	}
	stored, err := externalSubSnapshotMetadata(database.GetDB(), now)
	if err != nil {
		return nil, err
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
			Ready: row.Ready, UniversalReady: row.UniversalReady, ClashReady: row.ClashReady, ClashLastSuccess: row.ClashLastSuccess, UniversalError: row.UniversalError, ClashError: row.ClashError, LastSuccess: row.LastSuccess, LastAttempt: row.LastAttempt,
			NextAttempt: row.NextAttempt, LastError: row.LastError, Paused: row.Paused,
			Requested: row.Requested, Refreshing: row.Refreshing,
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
			if row.References > 0 && !row.Ready {
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
		if ref.routeConflict {
			return errors.New("source-route-conflict")
		}
		if ref.collectorId != "" {
			registry, err := externalCollectorRegistry()
			if err != nil || registry[ref.collectorId].Secret == "" {
				return errors.New("collector-not-configured")
			}
			if !ref.routeEnabled {
				return errors.New("collector-source-disabled")
			}
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
		if ref.collectorId != "" || ref.routeConflict {
			continue
		}
		row := model.ExternalSubSnapshot{Key: key}
		automatic := config.AutoRefresh && ref.references > 0
		if automatic {
			if err := database.GetDB().Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
		}
		lease := rand.Text()
		query := database.GetDB().Model(&model.ExternalSubSnapshot{}).
			Where("key = ? AND paused = ? AND next_attempt <= ? AND lock_until <= ?", key, false, now, now)
		if !automatic {
			// A crashed manual job has already consumed its queue flag. Its
			// expired lease makes it eligible again after a panel restart.
			query = query.Where("requested = ? OR (lease <> ? AND lock_until <= ?)", true, "", now)
		}
		claim := query.Updates(map[string]interface{}{"lease": lease, "lock_until": now + 30, "last_attempt": now, "requested": false, "lease_source_id": "", "lease_collector_id": ""})
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
		finished := s.clock()
		values := snapshotRefreshValues(row, result, fetchErr, now, finished, config.RefreshMinutes)
		return database.GetDB().Session(&gorm.Session{Logger: ormlog.Discard}).Transaction(func(tx *gorm.DB) error {
			// The URL may have been removed or its last client disabled during
			// the fetch. Never publish a now-unassigned source's response.
			current, err := activeExternalSubs(tx, finished)
			if err != nil {
				return err
			}
			if currentRef, stillAssigned := current[key]; !stillAssigned || currentRef.collectorId != "" || currentRef.routeConflict {
				// Keep every last-good format while discarding obsolete results.
				values = map[string]interface{}{"lease": "", "lock_until": 0, "requested": false, "last_error": "source-unassigned"}
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
		"source-native-clash-invalid": true, "provider-blocked": true,
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

// Both local and remote workers publish through the same independent-format
// validation path. A sibling format's failure cannot erase a valid snapshot.
func snapshotRefreshValues(row model.ExternalSubSnapshot, result externalSubFetchResult, universalErr error, started, finished int64, minutes int) map[string]interface{} {
	if universalErr == nil {
		universalErr = validateExternalSubFormat(result.body, false)
	}
	formats := []*externalSubFormatResult{{body: result.body, status: result.status, retryAt: result.retryAt, err: universalErr, userInfo: result.userInfo}}
	if result.clash != nil {
		native := *result.clash
		if native.err == nil {
			native.err = validateExternalSubFormat(native.body, true)
		}
		formats = append(formats, &native)
	}
	values := map[string]interface{}{"lease": "", "lock_until": 0, "last_error": "", "failures": 0}
	successes := 0
	paused := false
	onlyBudget := true
	retryAt := int64(0)
	for i, format := range formats {
		payload, success, errorKey, info := "payload", "last_success", "universal_error", "universal_info"
		if i == 1 {
			payload, success, errorKey, info = "clash_payload", "clash_last_success", "clash_error", "clash_info"
		}
		values[errorKey] = ""
		if format.err == nil {
			successes++
			body := format.body
			if i == 0 {
				body = util.DecodeExternalSubBody(body)
			}
			values[payload] = body
			values[success] = finished
			encoded, _ := json.Marshal(format.userInfo)
			values[info] = encoded
		} else {
			code := safeExternalSubError(format.err)
			values[errorKey] = code
			if values["last_error"] == "" {
				values["last_error"] = code
			}
			if !errors.Is(format.err, errSubRequestBudget) {
				onlyBudget = false
			}
		}
		if format.status == 401 || format.status == 403 || format.err != nil && format.err.Error() == "provider-blocked" {
			paused = true
		}
		retryAt = max(retryAt, format.retryAt)
	}
	next := started + int64(minutes)*60
	if successes == 0 {
		if onlyBudget {
			values["failures"] = row.Failures
			values["requested"] = true
			next = finished + 60
		} else {
			failures := min(max(row.Failures, 0)+1, 10)
			values["failures"] = failures
			next = finished + min(int64(minutes)*60*(1<<(failures-1)), 6*60*60)
		}
	}
	values["paused"] = paused
	values["next_attempt"] = max(next, retryAt)
	return values
}
