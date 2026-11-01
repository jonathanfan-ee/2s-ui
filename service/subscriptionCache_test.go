package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/op/go-logging"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/logger"
)

const snapshotTestURL = "https://provider.example/sub?token=synthetic-secret"
const snapshotTestBody = "trojan://synthetic-password@node.example:443#test-node"

func setupSnapshotDB(t *testing.T) (*ExternalSubCacheService, *int64, string) {
	t.Helper()
	logger.InitLogger(logging.ERROR)
	path := filepath.Join(t.TempDir(), "cache.db")
	if err := database.InitDB(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.CloseDBForTest(); err != nil {
			t.Error(err)
		}
	})
	now := int64(1800000000)
	return &ExternalSubCacheService{now: func() time.Time { return time.Unix(now, 0) }}, &now, path
}

func snapshotClient(t *testing.T, name, source string, enabled bool, expiry int64) {
	t.Helper()
	links, _ := json.Marshal([]map[string]string{{"type": "sub", "uri": source, "remark": "provider"}, {"type": "sub", "uri": source}})
	if err := database.GetDB().Create(&model.Client{Name: name, Enable: enabled, Expiry: expiry, Links: links}).Error; err != nil {
		t.Fatal(err)
	}
}

func snapshotRow(t *testing.T, key string) model.ExternalSubSnapshot {
	t.Helper()
	var row model.ExternalSubSnapshot
	if err := database.GetDB().Where("key = ?", key).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestExternalSubCachePrewarmDeduplicateAndPersist(t *testing.T) {
	s, now, path := setupSnapshotDB(t)
	snapshotClient(t, "alice", snapshotTestURL, true, 0)
	snapshotClient(t, "bob", snapshotTestURL, true, *now+60)
	snapshotClient(t, "disabled", "https://disabled.example/sub", false, 0)
	snapshotClient(t, "expired", "https://expired.example/sub", true, *now)
	cfg, err := s.Config()
	if err != nil || cfg.Enabled || cfg.AutoRefresh || cfg.RefreshMinutes != 5 {
		t.Fatalf("default config: %+v %v", cfg, err)
	}
	calls := 0
	s.fetch = func(_ context.Context, raw string, reserve func() error) (externalSubFetchResult, error) {
		calls++
		if raw != snapshotTestURL {
			t.Fatalf("unexpected source: %q", raw)
		}
		if err := reserve(); err != nil {
			return externalSubFetchResult{}, err
		}
		return externalSubFetchResult{body: snapshotTestBody, status: 200}, nil
	}
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int64
	database.GetDB().Model(&model.ExternalSubSnapshot{}).Count(&count)
	if calls != 0 || count != 0 {
		t.Fatal("disabled cache performed background work")
	}
	if err := s.Configure(ExternalSubCacheConfig{Enabled: true, RefreshMinutes: 5}, "test"); err == nil {
		t.Fatal("cold cache enabled")
	}
	if err := s.Queue(nil, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("duplicate URLs fetched %d times", calls)
	}
	status, err := s.Status()
	if err != nil || len(status) != 1 || !status[0].Ready || status[0].References != 2 {
		t.Fatalf("status: %+v %v", status, err)
	}
	data, _ := json.Marshal(status)
	for _, secret := range []string{"synthetic-secret", "synthetic-password", snapshotTestURL, "payload", "lease"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("status leaked %q", secret)
		}
	}
	if err := s.Configure(ExternalSubCacheConfig{Enabled: true, AutoRefresh: true, RefreshMinutes: 5}, "test"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		body, cached, err := s.Resolve(snapshotTestURL)
		if err != nil || !cached || body != snapshotTestBody {
			t.Fatalf("resolve: %q %v %v", body, cached, err)
		}
	}
	if _, _, err := s.Resolve("https://other.example/sub"); !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("cold source did not fail closed")
	}
	if calls != 1 {
		t.Fatal("download caused upstream request")
	}
	if err := database.CloseDBForTest(); err != nil {
		t.Fatal(err)
	}
	if err := database.InitDB(path); err != nil {
		t.Fatal(err)
	}
	if body, _, err := s.Resolve(snapshotTestURL); err != nil || body != snapshotTestBody {
		t.Fatal("snapshot not persisted", err)
	}
	var attempts int64
	database.GetDB().Model(&model.ExternalSubAttempt{}).Count(&attempts)
	if attempts != 1 {
		t.Fatal("rate budget did not persist")
	}
}

func TestExternalSubCacheLastGoodPauseAndCooldown(t *testing.T) {
	s, now, _ := setupSnapshotDB(t)
	snapshotClient(t, "alice", snapshotTestURL, true, 0)
	key := externalSubKey(snapshotTestURL)
	if err := database.GetDB().Create(&model.ExternalSubSnapshot{Key: key, Payload: snapshotTestBody, LastSuccess: *now - 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Configure(ExternalSubCacheConfig{Enabled: true, RefreshMinutes: 5}, "test"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	status := 429
	s.fetch = func(_ context.Context, _ string, reserve func() error) (externalSubFetchResult, error) {
		calls++
		if err := reserve(); err != nil {
			return externalSubFetchResult{}, err
		}
		return externalSubFetchResult{status: status, retryAt: *now + 900, body: "<html>blocked</html>"}, errors.New("source-http-" + map[int]string{429: "429", 403: "403"}[status])
	}
	if err := s.Queue(nil, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := snapshotRow(t, key)
	if row.Payload != snapshotTestBody || row.NextAttempt != *now+900 || row.LastError != "source-http-429" || row.Failures != 1 {
		t.Fatalf("bad failure state: %+v", row)
	}
	// A queued request cannot reset Retry-After, even when explicitly resumed.
	if err := s.Queue(nil, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("manual refresh bypassed cooldown")
	}
	*now = row.NextAttempt
	status = 403
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	row = snapshotRow(t, key)
	if !row.Paused || row.Payload != snapshotTestBody {
		t.Fatal("403 did not pause and preserve snapshot")
	}
	if err := s.Queue(nil, false); err == nil {
		t.Fatal("paused source resumed implicitly")
	}
	if err := s.Queue(nil, true); err != nil {
		t.Fatal(err)
	}
	if row2 := snapshotRow(t, key); row2.Paused || row2.NextAttempt != row.NextAttempt {
		t.Fatal("resume reset cooldown")
	}
	*now = row.NextAttempt
	s.fetch = func(_ context.Context, _ string, _ func() error) (externalSubFetchResult, error) {
		return externalSubFetchResult{body: "<html>login</html>", status: 200}, nil
	}
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	row = snapshotRow(t, key)
	if row.Payload != snapshotTestBody || row.LastError != "source-content-invalid" || row.Failures != 3 || row.NextAttempt != *now+1200 {
		t.Fatalf("invalid response replaced snapshot/backoff: %+v", row)
	}
	s.fetch = func(_ context.Context, _ string, _ func() error) (externalSubFetchResult, error) {
		return externalSubFetchResult{body: snapshotTestBody, status: 200}, nil
	}
	*now = row.NextAttempt
	if err := s.Queue(nil, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	row = snapshotRow(t, key)
	if row.Failures != 0 || row.LastError != "" || row.NextAttempt != *now+300 {
		t.Fatal("success did not clear backoff")
	}
}

func TestExternalSubCacheAssignmentAndLeaseFence(t *testing.T) {
	for _, change := range []string{"unassigned", "lease", "queued-during-fetch"} {
		t.Run(change, func(t *testing.T) {
			s, now, _ := setupSnapshotDB(t)
			snapshotClient(t, "alice", snapshotTestURL, true, 0)
			key := externalSubKey(snapshotTestURL)
			s.fetch = func(_ context.Context, _ string, _ func() error) (externalSubFetchResult, error) {
				switch change {
				case "unassigned":
					database.GetDB().Model(&model.Client{}).Where("name = ?", "alice").Update("enable", false)
				case "lease":
					database.GetDB().Model(&model.ExternalSubSnapshot{}).Where("key = ?", key).Update("lease", "new-owner")
				case "queued-during-fetch":
					if err := s.Queue([]string{key}, false); err != nil {
						t.Fatal(err)
					}
				}
				return externalSubFetchResult{body: snapshotTestBody, status: 200}, nil
			}
			if err := s.Queue(nil, false); err != nil {
				t.Fatal(err)
			}
			if err := s.RefreshOne(context.Background()); err != nil {
				t.Fatal(err)
			}
			row := snapshotRow(t, key)
			if change == "queued-during-fetch" {
				if !row.Requested || row.LastSuccess != *now {
					t.Fatal("in-flight queue request lost")
				}
			} else if row.Payload != "" || row.LastSuccess != 0 {
				t.Fatal("stale job published a snapshot")
			}
		})
	}
}

func TestExternalSubCacheBudgetAndAuthorization(t *testing.T) {
	s, now, _ := setupSnapshotDB(t)
	snapshotClient(t, "alice", snapshotTestURL, true, 0)
	if err := s.Queue([]string{externalSubKey("https://unassigned.example/sub")}, false); err == nil {
		t.Fatal("unassigned source queued")
	}
	for i := 0; i < 4; i++ {
		if err := reserveExternalSubAttempt(database.GetDB(), *now); err != nil {
			t.Fatal(err)
		}
	}
	if err := reserveExternalSubAttempt(database.GetDB(), *now); !errors.Is(err, errSubRequestBudget) {
		t.Fatal("fifth request allowed", err)
	}
	*now += 60
	if err := reserveExternalSubAttempt(database.GetDB(), *now); err != nil {
		t.Fatal("budget did not recover", err)
	}
	// Corrupt configuration must fail closed, never silently select live mode.
	database.GetDB().Create(&model.Setting{Key: "subCacheEnabled", Value: "invalid"})
	if _, cached, err := s.Resolve(snapshotTestURL); !cached || !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("configuration error allowed live fetch")
	}
}

func TestExternalSubCacheBudgetWaitKeepsPrewarmQueued(t *testing.T) {
	s, now, _ := setupSnapshotDB(t)
	snapshotClient(t, "alice", snapshotTestURL, true, 0)
	snapshotClient(t, "bob", "https://second.example/sub", true, 0)
	calls := 0
	waitingKey := ""
	s.fetch = func(_ context.Context, raw string, reserve func() error) (externalSubFetchResult, error) {
		calls++
		n := 1
		if calls == 1 {
			n = 4
		}
		for range n {
			if err := reserve(); err != nil {
				waitingKey = externalSubKey(raw)
				return externalSubFetchResult{}, err
			}
		}
		return externalSubFetchResult{status: 200, body: snapshotTestBody}, nil
	}
	if err := s.Queue(nil, false); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.RefreshOne(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	row := snapshotRow(t, waitingKey)
	if !row.Requested || row.Failures != 0 || row.NextAttempt != *now+60 || row.Payload != "" {
		t.Fatalf("budget wait lost prewarm: %+v", row)
	}
	*now += 60
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	row = snapshotRow(t, waitingKey)
	if row.Requested || row.LastSuccess != *now || row.Payload != snapshotTestBody || calls != 3 {
		t.Fatal("prewarm did not recover after budget wait")
	}
}

func TestExternalSubCacheRecoversExpiredManualLease(t *testing.T) {
	s, now, _ := setupSnapshotDB(t)
	snapshotClient(t, "alice", snapshotTestURL, true, 0)
	key := externalSubKey(snapshotTestURL)
	if err := database.GetDB().Create(&model.ExternalSubSnapshot{Key: key, Lease: "crashed-job", LockUntil: *now - 1, LastAttempt: *now - 31}).Error; err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.fetch = func(_ context.Context, _ string, reserve func() error) (externalSubFetchResult, error) {
		calls++
		if err := reserve(); err != nil {
			return externalSubFetchResult{}, err
		}
		return externalSubFetchResult{body: snapshotTestBody, status: 200}, nil
	}
	if err := s.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := snapshotRow(t, key)
	if calls != 1 || row.LastSuccess != *now || row.Lease != "" {
		t.Fatal("expired manual job did not recover")
	}
}
