package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	ormlog "gorm.io/gorm/logger"
)

const collectorAuthWindow = int64(5 * 60 * 1000)
const maxCollectorFormatBytes = 512 * 1024

var ErrCollectorUnauthorized = errors.New("collector-unauthorized")
var ErrCollectorRate = errors.New("collector-control-rate-limited")
var ErrCollectorStale = errors.New("collector-stale-job")
var ErrCollectorInput = errors.New("collector-invalid-request")
var ErrCollectorBodySize = errors.New("collector-request-too-large")

type externalCollectorCredential struct {
	Secret       string `json:"secret"`
	ExpectedIpv4 string `json:"expectedIpv4"`
}
type ExternalCollectorPublic struct {
	Id           string `json:"id"`
	ExpectedIpv4 string `json:"expectedIpv4"`
}

func externalCollectorRegistry() (map[string]externalCollectorCredential, error) {
	raw := os.Getenv("SUI_SUB_COLLECTORS")
	if raw == "" {
		return map[string]externalCollectorCredential{}, nil
	}
	var registry map[string]externalCollectorCredential
	if json.Unmarshal([]byte(raw), &registry) != nil || registry == nil {
		return nil, errors.New("collector-registry-invalid")
	}
	for id, entry := range registry {
		ip, err := netip.ParseAddr(entry.ExpectedIpv4)
		if !validExternalSourceId(id) || len(entry.Secret) < 32 || strings.HasPrefix(entry.Secret, "REPLACE_") || err != nil || !publicSubscriptionIPv4(ip) || !ip.Is4() {
			return nil, errors.New("collector-registry-invalid")
		}
	}
	return registry, nil
}
func publicExternalCollectors() ([]ExternalCollectorPublic, string) {
	result := []ExternalCollectorPublic{}
	registry, err := externalCollectorRegistry()
	if err != nil {
		return result, "collector-registry-invalid"
	}
	for id, entry := range registry {
		result = append(result, ExternalCollectorPublic{Id: id, ExpectedIpv4: entry.ExpectedIpv4})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Id < result[j].Id })
	return result, ""
}
func collectorStamp(entry externalCollectorCredential) string {
	sum := sha256.Sum256([]byte(entry.Secret + "\n" + entry.ExpectedIpv4))
	return hex.EncodeToString(sum[:])
}

// The canonical form matches the existing MiSub collector wire protocol. The
// escaped full path also supports panels mounted below /app/ or another prefix.
func CollectorSigningText(method, path, id, timestamp, nonce string, body []byte) string {
	hash := sha256.Sum256(body)
	return strings.Join([]string{method, path, id, timestamp, nonce, hex.EncodeToString(hash[:])}, "\n")
}

type ExternalCollectorService struct{ now func() time.Time }

func (s *ExternalCollectorService) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *ExternalCollectorService) Authenticate(request *http.Request, clientIP string, limit int64) (string, []byte, error) {
	registry, err := externalCollectorRegistry()
	if err != nil {
		return "", nil, err
	}
	id := request.Header.Get("X-Collector-Id")
	entry, registered := registry[id]
	timestamp := request.Header.Get("X-Collector-Timestamp")
	nonce := request.Header.Get("X-Collector-Nonce")
	signature := request.Header.Get("X-Collector-Signature")
	ts, parseErr := strconv.ParseInt(timestamp, 10, 64)
	now := s.clock().UnixMilli()
	ip, ipErr := netip.ParseAddr(clientIP)
	if !registered || request.Method != http.MethodPost || request.URL.RawQuery != "" || len(timestamp) != 13 || parseErr != nil || ts <= now-collectorAuthWindow || ts >= now+collectorAuthWindow || !collectorNoncePattern.MatchString(nonce) || !collectorSignaturePattern.MatchString(signature) || ipErr != nil || ip.Unmap().String() != entry.ExpectedIpv4 {
		return "", nil, ErrCollectorUnauthorized
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		return "", nil, ErrCollectorInput
	}
	if int64(len(body)) > limit {
		return "", nil, ErrCollectorBodySize
	}
	if !utf8.Valid(body) {
		return "", nil, ErrCollectorInput
	}
	mac := hmac.New(sha256.New, []byte(entry.Secret))
	mac.Write([]byte(CollectorSigningText(request.Method, request.URL.EscapedPath(), id, timestamp, nonce, body)))
	supplied, _ := hex.DecodeString(signature)
	if !hmac.Equal(mac.Sum(nil), supplied) {
		return "", nil, ErrCollectorUnauthorized
	}
	err = database.GetDB().Session(&gorm.Session{Logger: ormlog.Discard}).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now).Delete(&model.ExternalCollectorNonce{}).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&model.ExternalCollectorNonce{}).Where("collector_id = ? AND created_at > ?", id, now-60000).Count(&count).Error; err != nil {
			return err
		}
		if count >= 30 {
			return ErrCollectorRate
		}
		row := model.ExternalCollectorNonce{CollectorId: id, Nonce: nonce, CreatedAt: now, ExpiresAt: max(now, ts) + collectorAuthWindow}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrCollectorUnauthorized
		}
		return nil
	})
	return id, body, err
}

var collectorNoncePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{20,80}$`)
var collectorSignaturePattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type ExternalCollectorTask struct {
	SourceId       string   `json:"sourceId"`
	URL            string   `json:"url"`
	URLHash        string   `json:"urlHash"`
	LeaseToken     string   `json:"leaseToken"`
	LeaseUntil     int64    `json:"leaseUntil"`
	Formats        []string `json:"formats"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
}

func (s *ExternalCollectorService) Claim(id string) ([]ExternalCollectorTask, error) {
	registry, err := externalCollectorRegistry()
	if err != nil || registry[id].Secret == "" {
		return nil, ErrCollectorUnauthorized
	}
	var cache ExternalSubCacheService
	config, err := cache.Config()
	if err != nil {
		return nil, err
	}
	now := s.clock().Unix()
	tasks := []ExternalCollectorTask{}
	err = database.GetDB().Session(&gorm.Session{Logger: ormlog.Discard}).Transaction(func(tx *gorm.DB) error {
		var inFlight int64
		if err := tx.Model(&model.ExternalSubSnapshot{}).Where("lease_collector_id = ? AND lease <> ? AND lock_until > ?", id, "", now).Count(&inFlight).Error; err != nil {
			return err
		}
		if inFlight > 0 {
			return nil
		}
		refs, err := activeExternalSubs(tx, now)
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
			if ref.collectorId != id || ref.routeConflict || !ref.routeEnabled {
				continue
			}
			automatic := config.AutoRefresh && ref.references > 0
			if automatic {
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ExternalSubSnapshot{Key: key}).Error; err != nil {
					return err
				}
			}
			lease := rand.Text()
			query := tx.Model(&model.ExternalSubSnapshot{}).Where("key = ? AND paused = ? AND next_attempt <= ? AND lock_until <= ?", key, false, now, now)
			if !automatic {
				query = query.Where("(requested = ? OR (lease <> ? AND lock_until <= ?))", true, "", now)
			}
			update := query.Updates(map[string]interface{}{"lease": lease, "lock_until": now + 30, "last_attempt": now, "requested": false, "next_attempt": now + int64(config.RefreshMinutes)*60, "lease_source_id": ref.sourceId, "lease_revision": ref.revision, "lease_collector_id": id, "lease_collector_stamp": collectorStamp(registry[id])})
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected == 0 {
				continue
			}
			tasks = append(tasks, ExternalCollectorTask{SourceId: ref.sourceId, URL: ref.url, URLHash: key, LeaseToken: lease, LeaseUntil: (now + 30) * 1000, Formats: []string{"clash", "universal"}, TimeoutSeconds: 10})
			break
		}
		return nil
	})
	return tasks, err
}

type ExternalCollectorSnapshot struct {
	Text              string            `json:"text"`
	Failure           string            `json:"failure"`
	RetryAfterSeconds int64             `json:"retryAfterSeconds"`
	UserInfo          map[string]uint64 `json:"userInfo"`
}
type ExternalCollectorUpload struct {
	SourceId   string                               `json:"sourceId"`
	URLHash    string                               `json:"urlHash"`
	LeaseToken string                               `json:"leaseToken"`
	Snapshots  map[string]ExternalCollectorSnapshot `json:"snapshots"`
}
type ExternalCollectorReceipt struct {
	Success       bool  `json:"success"`
	Paused        bool  `json:"paused"`
	NextAttemptAt int64 `json:"nextAttemptAt"`
}

var collectorFailurePattern = regexp.MustCompile(`^(http-[1-5][0-9]{2}|timeout|fetch-failed|unsupported-or-empty-response|provider-blocked|rate-limited|response-too-large|unsafe-target|invalid-response)$`)

func collectorFormatResult(snapshot ExternalCollectorSnapshot, native bool, now int64) (externalSubFormatResult, error) {
	result := externalSubFormatResult{body: snapshot.Text, status: 200}
	if snapshot.Failure != "" {
		if snapshot.Text != "" || !collectorFailurePattern.MatchString(snapshot.Failure) {
			return result, ErrCollectorInput
		}
		result.err = errors.New("source-request-failed")
		result.status = 0
		if strings.HasPrefix(snapshot.Failure, "http-") {
			result.status, _ = strconv.Atoi(snapshot.Failure[5:])
			result.err = errors.New("source-http-" + snapshot.Failure[5:])
		}
		switch snapshot.Failure {
		case "provider-blocked":
			result.err = errors.New("provider-blocked")
		case "rate-limited":
			result.err = errSubRequestBudget
		}
	} else if len(snapshot.Text) > maxCollectorFormatBytes {
		result.err = errors.New("source-body-invalid-or-too-large")
	} else {
		result.err = validateExternalSubFormat(snapshot.Text, native)
	}
	if snapshot.RetryAfterSeconds > 0 {
		result.retryAt = now + min(snapshot.RetryAfterSeconds, 7*86400)
	}
	result.userInfo = map[string]uint64{}
	for _, key := range []string{"upload", "download", "total", "expire"} {
		if value, ok := snapshot.UserInfo[key]; ok && value <= 1<<53 {
			result.userInfo[key] = value
		}
	}
	return result, nil
}
func (s *ExternalCollectorService) Complete(id string, input ExternalCollectorUpload) (*ExternalCollectorReceipt, error) {
	registry, err := externalCollectorRegistry()
	if err != nil || registry[id].Secret == "" {
		return nil, ErrCollectorUnauthorized
	}
	if !validExternalSourceId(input.SourceId) || len(input.URLHash) != 64 || len(input.LeaseToken) < 20 {
		return nil, ErrCollectorInput
	}
	universal, hasUniversal := input.Snapshots["universal"]
	native, hasNative := input.Snapshots["clash"]
	if !hasUniversal || !hasNative || len(input.Snapshots) != 2 {
		return nil, ErrCollectorInput
	}
	now := s.clock().Unix()
	uni, err := collectorFormatResult(universal, false, now)
	if err != nil {
		return nil, err
	}
	clash, err := collectorFormatResult(native, true, now)
	if err != nil {
		return nil, err
	}
	var cache ExternalSubCacheService
	config, err := cache.Config()
	if err != nil {
		return nil, err
	}
	receipt := &ExternalCollectorReceipt{Success: true}
	err = database.GetDB().Session(&gorm.Session{Logger: ormlog.Discard}).Transaction(func(tx *gorm.DB) error {
		var source model.ExternalSubSource
		if err := tx.Where("id = ? AND enabled = ? AND collector_id = ?", input.SourceId, true, id).First(&source).Error; err != nil {
			return ErrCollectorStale
		}
		if externalSubKey(source.URL) != input.URLHash {
			return ErrCollectorStale
		}
		var row model.ExternalSubSnapshot
		if err := tx.Where("key = ? AND lease = ? AND lock_until > ? AND lease_source_id = ? AND lease_collector_id = ?", input.URLHash, input.LeaseToken, now, input.SourceId, id).First(&row).Error; err != nil {
			return ErrCollectorStale
		}
		if row.LeaseRevision != source.Revision || row.LeaseCollectorStamp != collectorStamp(registry[id]) {
			return ErrCollectorStale
		}
		refs, err := activeExternalSubs(tx, now)
		if err != nil {
			return err
		}
		ref, ok := refs[input.URLHash]
		if !ok || ref.routeConflict || ref.collectorId != id {
			return ErrCollectorStale
		}
		values := snapshotRefreshValues(row, externalSubFetchResult{body: uni.body, status: uni.status, retryAt: uni.retryAt, userInfo: uni.userInfo, clash: &clash}, uni.err, row.LastAttempt, now, config.RefreshMinutes)
		if err := tx.Model(&model.ExternalSubSnapshot{}).Where("key = ? AND lease = ?", input.URLHash, input.LeaseToken).Updates(values).Error; err != nil {
			return err
		}
		receipt.Paused = values["paused"].(bool)
		receipt.NextAttemptAt = values["next_attempt"].(int64) * 1000
		return nil
	})
	return receipt, err
}
