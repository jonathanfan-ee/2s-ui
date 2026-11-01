package sub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/logger"
)

func TestSubscriptionCacheExportsAndAccess(t *testing.T) {
	setupSubDB(t)
	logger.InitLogger(logging.ERROR)
	gin.SetMode(gin.TestMode)
	// This local server is intentionally unreachable by the scheduled fetcher.
	// If a download regresses to a live fetch, it increments calls and fails.
	calls := 0
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "must not fetch", 500) }))
	defer source.Close()
	links, _ := json.Marshal([]Link{{Type: "sub", Uri: source.URL}})
	hash := sha256.Sum256([]byte(source.URL))
	key := hex.EncodeToString(hash[:])
	body := "trojan://synthetic-password@node.example:443#cached-node"
	now := time.Now().Unix()
	for _, row := range []interface{}{
		&model.Setting{Key: "subCacheEnabled", Value: "true"},
		&model.Setting{Key: "subEncode", Value: "false"},
		&model.ExternalSubSnapshot{Key: key, Payload: body, LastSuccess: now},
		&model.Client{Name: "alice", Enable: true, Links: links, Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`)},
		&model.Client{Name: "expired", Enable: true, Expiry: now - 1, Links: links, Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`)},
		&model.Client{Name: "disabled", Enable: false, Links: links, Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`)},
		&model.Client{Name: "no-grant", Enable: true, Links: json.RawMessage(`[]`), Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`)},
	} {
		if err := database.GetDB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	h := &SubHandler{}
	router := gin.New()
	router.GET("/:subid", h.subs)
	for _, format := range []string{"", "?format=clash", "?format=json", "?format=info"} {
		for range 2 {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/alice"+format, nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "cached-node") {
				t.Fatalf("export %s: %d %s", format, w.Code, w.Body.String())
			}
		}
		for _, client := range []string{"expired", "disabled", "no-grant"} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/"+client+format, nil))
			if strings.Contains(w.Body.String(), "synthetic-password") || strings.Contains(w.Body.String(), "cached-node") {
				t.Fatalf("snapshot leaked to %s in %s", client, format)
			}
			if client == "no-grant" && format == "?format=info" && !strings.Contains(w.Body.String(), `"links":[]`) {
				t.Fatal("empty links must be an array", w.Body.String())
			}
			if client != "no-grant" && format != "?format=info" && w.Code == 200 {
				t.Fatalf("inactive client %s received a config", client)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("subscription download contacted provider %d times", calls)
	}
	// A missing source blocks every config format instead of issuing a live
	// fetch or publishing a partial config (including the raw-link endpoint).
	if err := database.GetDB().Where("key = ?", key).Delete(&model.ExternalSubSnapshot{}).Error; err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"", "?format=clash", "?format=json", "?format=info"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/alice"+format, nil))
		if w.Code != 503 || w.Header().Get("Retry-After") != "300" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("cold cache %s: %d %s", format, w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("cold cache fell back to live provider")
	}
}
