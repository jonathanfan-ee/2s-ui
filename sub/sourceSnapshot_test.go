package sub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
)

func TestAuthorizedRawSourceFeedPreservesFormatsAndNeverFetches(t *testing.T) {
	setupSubDB(t)
	db := database.GetDB()
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer provider.Close()
	reader := model.Client{Name: "private-feed-bearer", Enable: true, Group: "@source-feed", Volume: 12345, Links: json.RawMessage(`[]`), Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`)}
	if err := db.Create(&reader).Error; err != nil {
		t.Fatal(err)
	}
	source := model.ExternalSubSource{Id: "airport-one", Name: "Airport", URL: provider.URL + "/secret-provider-link", Enabled: true, NodePrefix: "personal-prefix - "}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	grant := model.ExternalSubGrant{SourceId: source.Id, ClientId: reader.Id}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(source.URL))
	now := time.Now().Unix() - 30
	row := model.ExternalSubSnapshot{Key: hex.EncodeToString(key[:]), Payload: "hy2://fixture@proxy.example:443#Original%20Name", ClashPayload: "proxies:\n  - {name: Original Name, type: ss, server: proxy.example, port: 443, cipher: aes-128-gcm, password: fixture}\nhosts: {proxy.example: 8.8.8.8}\n", LastSuccess: now, ClashLastSuccess: now - 5, UniversalInfo: []byte(`{"upload":1,"download":2,"total":300,"expire":400}`), ClashInfo: []byte(`{"download":9,"total":900}`), NextAttempt: now + 300, LastAttempt: now, Lease: "unchanged-lease", LockUntil: now + 100}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	other := model.ExternalSubSource{Id: "other", Name: "Other", URL: provider.URL + "/other", Enabled: true}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	otherKey := sha256.Sum256([]byte(other.URL))
	otherRow := row
	otherRow.Key = hex.EncodeToString(otherKey[:])
	otherRow.Payload = "must-not-leak-another-source"
	if err := db.Create(&otherRow).Error; err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s := &SubHandler{}
	router.GET("/sub/:subid", s.subs)
	router.HEAD("/sub/:subid", s.subHeaders)
	request := func(method, bearer, query, ua string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/sub/"+bearer+query, nil)
		req.Header.Set("User-Agent", ua)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct {
		query, ua, want, info string
		stamp                 int64
	}{
		{"?source=airport-one", "SubscriptionFetcher/1.0", row.Payload, "upload=1; download=2; total=300; expire=400", now},
		{"?source=airport-one", "ClashMeta/fixture", row.ClashPayload, "download=9; total=900", now - 5},
		{"?source=airport-one&format=clash", "Other", row.ClashPayload, "download=9; total=900", now - 5},
		{"?source=airport-one&format=universal", "mihomo", row.Payload, "upload=1; download=2; total=300; expire=400", now},
	} {
		w := request("GET", reader.Name, tc.query, tc.ua)
		if w.Code != 200 || w.Body.String() != tc.want {
			t.Fatalf("raw payload changed: status %d", w.Code)
		}
		if w.Header().Get("Subscription-Userinfo") != tc.info || w.Header().Get("X-Snapshot-Updated-At") != fmt.Sprint(tc.stamp) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("source metadata replaced with feed-account/render metadata")
		}
		h := request("HEAD", reader.Name, tc.query, tc.ua)
		if h.Code != 200 || h.Body.Len() != 0 || h.Header().Get("X-Snapshot-Updated-At") != fmt.Sprint(tc.stamp) {
			t.Fatal("HEAD bypassed feed semantics")
		}
	}
	for _, q := range []string{"?source=other", "?source=", "?source=airport-one&source=other", "?source=airport-one&format=page", "?source=airport-one&format=clash&format=universal"} {
		if w := request("GET", reader.Name, q, ""); w.Code != 404 {
			t.Fatalf("invalid/cross-source request accepted: %s %d", q, w.Code)
		}
	}
	if w := request("GET", "unknown", "?source=airport-one", ""); w.Code != 404 {
		t.Fatal("unknown reader allowed")
	}
	// Ordinary users cannot obtain unrendered provider payloads even with a grant.
	if err := db.Model(&reader).Update("group", "ordinary").Error; err != nil {
		t.Fatal(err)
	}
	if w := request("GET", reader.Name, "?source=airport-one", ""); w.Code != 404 {
		t.Fatal("ordinary client used internal feed")
	}
	db.Model(&reader).Update("group", "@source-feed")
	for _, fields := range []map[string]any{{"enable": false}, {"expiry": time.Now().Unix() - 1}, {"up": 12346}} {
		if err := db.Model(&reader).Updates(fields).Error; err != nil {
			t.Fatal(err)
		}
		if w := request("HEAD", reader.Name, "?source=airport-one", ""); w.Code != 404 {
			t.Fatal("disabled/expired/depleted reader allowed")
		}
		db.Model(&reader).Updates(map[string]any{"enable": true, "expiry": 0, "up": 0})
	}
	db.Delete(&grant)
	if w := request("GET", reader.Name, "?source=airport-one", ""); w.Code != 404 {
		t.Fatal("revoked grant still readable")
	}
	db.Create(&grant)
	db.Model(&source).Update("enabled", false)
	if w := request("GET", reader.Name, "?source=airport-one", ""); w.Code != 404 {
		t.Fatal("disabled source readable")
	}
	db.Model(&source).Update("enabled", true)
	originalURL := source.URL
	db.Model(&source).Update("url", provider.URL+"/changed-provider")
	if w := request("GET", reader.Name, "?source=airport-one", ""); w.Code != 503 {
		t.Fatal("old URL snapshot survived a provider URL change")
	}
	db.Model(&source).Update("url", originalURL)
	// No alternate-format fallback, no fresh timestamp on stale/paused snapshots.
	for _, fields := range []map[string]any{{"clash_payload": ""}, {"clash_last_success": now - 1000}, {"paused": true}} {
		db.Model(&model.ExternalSubSnapshot{}).Where("key = ?", row.Key).Updates(fields)
		w := request("GET", reader.Name, "?source=airport-one&format=clash", "")
		if w.Code != 503 || w.Header().Get("X-Snapshot-Updated-At") != "" || w.Body.Len() != 0 {
			t.Fatal("unavailable source masqueraded as a fresh format")
		}
		db.Model(&model.ExternalSubSnapshot{}).Where("key = ?", row.Key).Updates(map[string]any{"clash_payload": row.ClashPayload, "clash_last_success": row.ClashLastSuccess, "paused": false})
	}
	var after model.ExternalSubSnapshot
	if err := db.First(&after, "key = ?", row.Key).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, row) {
		t.Fatal("reader changed cache/cooldown/lease state")
	}
	if requests.Load() != 0 {
		t.Fatal("reader fetched the provider")
	}
	if strings.Contains(request("GET", reader.Name, "?source=airport-one", "").Body.String(), "personal-prefix") {
		t.Fatal("prefix polluted raw source")
	}
}
