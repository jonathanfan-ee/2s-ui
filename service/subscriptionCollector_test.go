package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
)

const collectorFixtureSecret = "fixture-secret-for-testing-only-123456789"
const collectorNativeBody = `proxies:
- {name: native-node, type: trojan, server: node.example, port: 443, password: synthetic-password}
`

func setupCollector(t *testing.T) (*ExternalCollectorService, *int64, model.ExternalSubSource) {
	t.Helper()
	_, now, _ := setupSnapshotDB(t)
	t.Setenv("SUI_SUB_COLLECTORS", `{"fixture":{"secret":"`+collectorFixtureSecret+`","expectedIpv4":"8.8.8.8"}}`)
	alice := catalogClient(t, "alice", "")
	ids := []uint{alice.Id}
	var catalog ExternalSubSourceService
	owner := "fixture"
	id, err := catalog.Save(ExternalSubSourceWrite{Name: "remote", URL: snapshotTestURL, Enabled: true, ClientIds: &ids, CollectorId: &owner}, "test")
	if err != nil {
		t.Fatal(err)
	}
	svc := &ExternalCollectorService{now: func() time.Time { return time.Unix(*now, 0) }}
	return svc, now, catalogSource(t, id)
}

func authenticateFixture(t *testing.T, svc *ExternalCollectorService, path, body, nonce, ip string, stamp int64) error {
	t.Helper()
	request := httptest.NewRequest("POST", path, strings.NewReader(body))
	timestamp := strconv.FormatInt(stamp, 10)
	mac := hmac.New(sha256.New, []byte(collectorFixtureSecret))
	mac.Write([]byte(CollectorSigningText("POST", path, "fixture", timestamp, nonce, []byte(body))))
	request.Header.Set("X-Collector-Id", "fixture")
	request.Header.Set("X-Collector-Timestamp", timestamp)
	request.Header.Set("X-Collector-Nonce", nonce)
	request.Header.Set("X-Collector-Signature", hex.EncodeToString(mac.Sum(nil)))
	_, _, err := svc.Authenticate(request, ip, 4096)
	return err
}
func TestCollectorAuthenticationReplayIPAndFutureTimestamp(t *testing.T) {
	svc, now, _ := setupCollector(t)
	path := "/app/api/collector/tasks"
	if err := authenticateFixture(t, svc, path, "{}", "fixture-nonce-123456789012", "1.1.1.1", *now*1000); !errors.Is(err, ErrCollectorUnauthorized) {
		t.Fatal("wrong IP accepted")
	}
	if err := authenticateFixture(t, svc, path, "{}", "fixture-nonce-123456789012", "8.8.8.8", *now*1000); err != nil {
		t.Fatal(err)
	}
	if err := authenticateFixture(t, svc, path, "{}", "fixture-nonce-123456789012", "8.8.8.8", *now*1000); !errors.Is(err, ErrCollectorUnauthorized) {
		t.Fatal("replayed nonce accepted")
	}
	future := (*now + 240) * 1000
	if err := authenticateFixture(t, svc, path, "{}", "future-nonce-123456789012", "8.8.8.8", future); err != nil {
		t.Fatal(err)
	}
	*now += 301
	if err := authenticateFixture(t, svc, path, "{}", "future-nonce-123456789012", "8.8.8.8", future); !errors.Is(err, ErrCollectorUnauthorized) {
		t.Fatal("future timestamp outlived replay protection")
	}
	if err := authenticateFixture(t, svc, path, "{}", "stale-nonce-123456789012", "8.8.8.8", (*now-301)*1000); !errors.Is(err, ErrCollectorUnauthorized) {
		t.Fatal("stale signature accepted")
	}
}
func claimCollectorFixture(t *testing.T, svc *ExternalCollectorService, source model.ExternalSubSource) ExternalCollectorTask {
	t.Helper()
	var cache ExternalSubCacheService
	if err := cache.Queue([]string{externalSubKey(source.URL)}, false); err != nil {
		t.Fatal(err)
	}
	tasks, err := svc.Claim("fixture")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("claim: %+v %v", tasks, err)
	}
	if tasks[0].URL != source.URL || len(tasks[0].Formats) != 2 {
		t.Fatal("task contract lost")
	}
	return tasks[0]
}
func uploadCollectorFixture(task ExternalCollectorTask) ExternalCollectorUpload {
	return ExternalCollectorUpload{SourceId: task.SourceId, URLHash: task.URLHash, LeaseToken: task.LeaseToken, Snapshots: map[string]ExternalCollectorSnapshot{"universal": {Text: snapshotTestBody}, "clash": {Text: collectorNativeBody}}}
}
func TestCollectorPublishesFormatsIndependentlyAndNeverFetchesLocally(t *testing.T) {
	svc, now, source := setupCollector(t)
	var cache ExternalSubCacheService
	cache.now = func() time.Time { return time.Unix(*now, 0) }
	localCalls := 0
	cache.fetch = func(_ context.Context, _ string, _ func() error) (externalSubFetchResult, error) {
		localCalls++
		return externalSubFetchResult{}, errors.New("must-not-fetch")
	}
	task := claimCollectorFixture(t, svc, source)
	if err := cache.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.Complete("fixture", uploadCollectorFixture(task))
	if err != nil || !receipt.Success {
		t.Fatal("complete", err)
	}
	row := snapshotRow(t, task.URLHash)
	if row.Payload != snapshotTestBody || row.ClashPayload != collectorNativeBody {
		t.Fatal("format snapshots not published")
	}
	*now += 301
	task = claimCollectorFixture(t, svc, source)
	input := uploadCollectorFixture(task)
	input.Snapshots["clash"] = ExternalCollectorSnapshot{Text: "<html>login</html>"}
	input.Snapshots["universal"] = ExternalCollectorSnapshot{Text: strings.ReplaceAll(snapshotTestBody, "test-node", "updated-node")}
	if _, err := svc.Complete("fixture", input); err != nil {
		t.Fatal(err)
	}
	row = snapshotRow(t, task.URLHash)
	if !strings.Contains(row.Payload, "updated-node") || row.ClashPayload != collectorNativeBody || row.ClashError != "source-native-clash-invalid" {
		t.Fatal("sibling failure destroyed last good format")
	}
	// Missing registration must not switch either the worker or a legacy URL
	// consumer to a different exit address.
	t.Setenv("SUI_SUB_COLLECTORS", "")
	if err := cache.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body, cached, err := cache.Resolve(source.URL); err != nil || !cached || body != row.Payload {
		t.Fatal("collector source fell back to live fetch", err)
	}
	if localCalls != 0 {
		t.Fatal("remote source was fetched by panel")
	}
}
func TestCollectorRevisionLeaseOwnershipAndPause(t *testing.T) {
	for _, change := range []string{"revision", "owner", "expiry", "secret"} {
		t.Run(change, func(t *testing.T) {
			svc, now, source := setupCollector(t)
			task := claimCollectorFixture(t, svc, source)
			switch change {
			case "revision":
				source = saveCatalogSource(t, source, nil, "", false)
			case "owner":
				database.GetDB().Model(&model.ExternalSubSource{}).Where("id = ?", source.Id).Update("collector_id", "other")
			case "expiry":
				*now += 31
			case "secret":
				t.Setenv("SUI_SUB_COLLECTORS", `{"fixture":{"secret":"a-different-fixture-secret-1234567890","expectedIpv4":"8.8.8.8"}}`)
			}
			if _, err := svc.Complete("fixture", uploadCollectorFixture(task)); !errors.Is(err, ErrCollectorStale) {
				t.Fatal("obsolete collector job accepted", change, err)
			}
		})
	}
	svc, now, source := setupCollector(t)
	task := claimCollectorFixture(t, svc, source)
	input := uploadCollectorFixture(task)
	input.Snapshots["universal"] = ExternalCollectorSnapshot{Failure: "provider-blocked"}
	input.Snapshots["clash"] = ExternalCollectorSnapshot{Failure: "http-429", RetryAfterSeconds: 900}
	receipt, err := svc.Complete("fixture", input)
	if err != nil || !receipt.Paused || receipt.NextAttemptAt < (*now+900)*1000 {
		t.Fatal("provider pause/retry ignored", receipt, err)
	}
	if _, err := svc.Complete("fixture", input); !errors.Is(err, ErrCollectorStale) {
		t.Fatal("completed lease reused")
	}
	var cache ExternalSubCacheService
	if err := cache.Queue([]string{task.URLHash}, false); err == nil {
		t.Fatal("paused source resumed implicitly")
	}
}
func TestCollectorPublicMetadataContainsNoSecret(t *testing.T) {
	_, _, _ = setupCollector(t)
	var catalog ExternalSubSourceService
	view, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), collectorFixtureSecret) || strings.Contains(string(data), "synthetic-secret") {
		t.Fatal("collector metadata leaked credentials")
	}
	if len(view.Collectors) != 1 || view.Collectors[0].ExpectedIpv4 != "8.8.8.8" {
		t.Fatal("public registration unavailable")
	}
}

func TestCollectorCannotClaimTwoConcurrentJobs(t *testing.T) {
	svc, _, first := setupCollector(t)
	var catalog ExternalSubSourceService
	owner := "fixture"
	id, err := catalog.Save(ExternalSubSourceWrite{Name: "second", URL: "https://second.example/sub", Enabled: true, CollectorId: &owner}, "test")
	if err != nil {
		t.Fatal(err)
	}
	second := catalogSource(t, id)
	var cache ExternalSubCacheService
	if err := cache.Queue([]string{externalSubKey(first.URL), externalSubKey(second.URL)}, false); err != nil {
		t.Fatal(err)
	}
	tasks, err := svc.Claim("fixture")
	if err != nil || len(tasks) != 1 {
		t.Fatal("initial claim failed", err)
	}
	tasks, err = svc.Claim("fixture")
	if err != nil || len(tasks) != 0 {
		t.Fatal("one collector got overlapping jobs", err)
	}
}
