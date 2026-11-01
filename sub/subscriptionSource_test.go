package sub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/logger"
)

func TestCatalogSourceExportsAreAuthorizedAndSnapshotOnly(t *testing.T) {
	setupSubDB(t)
	logger.InitLogger(logging.ERROR)
	gin.SetMode(gin.TestMode)
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "never fetch during export", 500)
	}))
	defer provider.Close()
	// A loopback fixture catches accidental network fallback. Catalog creation
	// separately tests the public-HTTPS URL guard; this is a seeded snapshot.
	source := model.ExternalSubSource{Id: "shared-source", Name: "provider", URL: provider.URL, Enabled: true, Revision: 1}
	selfLinks := json.RawMessage(`[{"type":"external","uri":"trojan://self-password@self.example:443#self-node"}]`)
	var clients []model.Client
	for _, name := range []string{"alice", "bob", "expired", "disabled"} {
		clients = append(clients, model.Client{Name: name, Enable: name != "disabled", Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`), Links: selfLinks})
	}
	clients[2].Expiry = time.Now().Unix() - 1
	if err := database.GetDB().Create(&clients).Error; err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(source.URL))
	key := hex.EncodeToString(hash[:])
	body := "trojan://catalog-password@catalog.example:443#catalog-node"
	for _, row := range []interface{}{&source, &model.ExternalSubSnapshot{Key: key, Payload: body, LastSuccess: time.Now().Unix()}, &model.Setting{Key: "subEncode", Value: "false"}} {
		if err := database.GetDB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []int{0, 2, 3} {
		if err := database.GetDB().Create(&model.ExternalSubGrant{SourceId: source.Id, ClientId: clients[i].Id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	handler := &SubHandler{}
	router := gin.New()
	router.GET("/:subid", handler.subs)
	get := func(name, format string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/"+name+format, nil))
		return w
	}
	formats := []string{"", "?format=clash", "?format=json", "?format=info"}
	// Global legacy cache reads are off. These grants still use snapshots.
	for _, format := range formats {
		w := get("alice", format)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "catalog-node") || !strings.Contains(w.Body.String(), "self-node") {
			t.Fatalf("catalog export %s: %d %s", format, w.Code, w.Body.String())
		}
		for _, name := range []string{"bob", "expired", "disabled"} {
			w = get(name, format)
			if strings.Contains(w.Body.String(), "catalog-password") {
				t.Fatalf("catalog credentials leaked to %s", name)
			}
		}
	}
	// Duplicating the exact URL through legacy and catalog paths cannot turn a
	// snapshot-only read back into live fetching or duplicate the source nodes.
	duplicate, _ := json.Marshal([]Link{{Type: "external", Uri: "trojan://self-password@self.example:443#self-node"}, {Type: "sub", Uri: source.URL}})
	database.GetDB().Model(&model.Client{}).Where("id = ?", clients[0].Id).Update("links", json.RawMessage(duplicate))
	if w := get("alice", ""); w.Code != 200 || strings.Count(w.Body.String(), "catalog-node") != 1 {
		t.Fatal("catalog/legacy export was not deduplicated", w.Body.String())
	}
	database.GetDB().Model(&model.Client{}).Where("id = ?", clients[0].Id).Update("links", selfLinks)
	// Guessing/embedding a known source id does not manufacture a grant.
	forged, _ := json.Marshal([]Link{{Type: "source", Uri: source.Id}})
	database.GetDB().Model(&model.Client{}).Where("id = ?", clients[1].Id).Update("links", json.RawMessage(forged))
	if w := get("bob", ""); w.Code != 503 || strings.Contains(w.Body.String(), "catalog-password") {
		t.Fatal("forged source reference bypassed grants", w.Body.String())
	}
	// URL rotation must not reuse the previous URL's valid snapshot.
	database.GetDB().Model(&model.ExternalSubSource{}).Where("id = ?", source.Id).Update("url", source.URL+"/rotated")
	for _, format := range formats {
		if w := get("alice", format); w.Code != 503 || strings.Contains(w.Body.String(), "catalog-password") {
			t.Fatal("cold rotated source exported old/partial configuration", w.Code, w.Body.String())
		}
	}
	database.GetDB().Model(&model.ExternalSubSource{}).Where("id = ?", source.Id).Update("url", source.URL)
	// Revocation removes only the catalog nodes; local nodes still export.
	database.GetDB().Where("source_id = ? AND client_id = ?", source.Id, clients[0].Id).Delete(&model.ExternalSubGrant{})
	for _, format := range formats {
		w := get("alice", format)
		if w.Code != 200 || strings.Contains(w.Body.String(), "catalog-node") || !strings.Contains(w.Body.String(), "self-node") {
			t.Fatal("revocation changed other nodes", format, w.Code, w.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("client exports made %d upstream requests", calls.Load())
	}
}
