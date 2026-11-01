package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/logger"
	"github.com/shenaba/2s-ui/service"
)

func TestCollectorMachineAPIHMACPrefixIPAndPublication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.InitLogger(logging.ERROR)
	if err := database.InitDB(filepath.Join(t.TempDir(), "collector-api.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.CloseDBForTest(); err != nil {
			t.Error(err)
		}
	})
	secret := "fixture-secret-for-testing-only-123456789"
	t.Setenv("SUI_SUB_COLLECTORS", `{"fixture":{"secret":"`+secret+`","expectedIpv4":"8.8.8.8"}}`)
	alice := model.Client{Name: "alice", Enable: true, Links: json.RawMessage(`[]`), Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`)}
	if err := database.GetDB().Create(&alice).Error; err != nil {
		t.Fatal(err)
	}
	ids := []uint{alice.Id}
	owner := "fixture"
	var catalog service.ExternalSubSourceService
	sourceId, err := catalog.Save(service.ExternalSubSourceWrite{Name: "remote", URL: "https://provider.example/sub?token=fixture-secret-url", Enabled: true, ClientIds: &ids, CollectorId: &owner}, "test")
	if err != nil {
		t.Fatal(err)
	}
	view, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	var cache service.ExternalSubCacheService
	if err := cache.Queue([]string{view.Sources[0].Cache.Key}, false); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewCollectorHandler(router.Group("/control/api/collector"))
	counter := 0
	request := func(path string, payload interface{}, ip string, signed bool, canonicalPath string, nonce string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
		req.RemoteAddr = ip + ":12345"
		req.Header.Set("Content-Type", "application/json")
		// Neither a browser/admin identity nor a forged forwarding header is enough.
		req.Header.Set("Token", "administrator-token")
		req.Header.Set("Cookie", "session=irrelevant")
		req.Header.Set("X-Forwarded-For", "8.8.8.8")
		req.Header.Set("CF-Connecting-IP", "8.8.8.8")
		if signed {
			counter++
			if nonce == "" {
				nonce = "fixture-nonce-1234567890-" + strconv.Itoa(counter)
			}
			stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
			if canonicalPath == "" {
				canonicalPath = path
			}
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(service.CollectorSigningText("POST", canonicalPath, "fixture", stamp, nonce, body)))
			req.Header.Set("X-Collector-Id", "fixture")
			req.Header.Set("X-Collector-Timestamp", stamp)
			req.Header.Set("X-Collector-Nonce", nonce)
			req.Header.Set("X-Collector-Signature", hex.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	path := "/control/api/collector/tasks"
	if w := request(path, map[string]interface{}{}, "8.8.8.8", false, "", ""); w.Code != 401 {
		t.Fatal("admin cookie/token bypassed collector auth", w.Code)
	}
	if w := request(path, map[string]interface{}{}, "9.9.9.9", true, "", ""); w.Code != 401 {
		t.Fatal("untrusted XFF spoofed source IP")
	}
	if w := request(path, map[string]interface{}{}, "8.8.8.8", true, "/api/collector/tasks", ""); w.Code != 401 {
		t.Fatal("signature ignored panel base path")
	}
	w := request(path, map[string]interface{}{}, "8.8.8.8", true, "", "")
	var claimed struct {
		Tasks []service.ExternalCollectorTask `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &claimed); err != nil || w.Code != 200 || len(claimed.Tasks) != 1 {
		t.Fatal("claim failed", w.Code, w.Body.String(), err)
	}
	if claimed.Tasks[0].SourceId != sourceId || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("task contract/header changed")
	}
	task := claimed.Tasks[0]
	input := service.ExternalCollectorUpload{SourceId: task.SourceId, URLHash: task.URLHash, LeaseToken: task.LeaseToken, Snapshots: map[string]service.ExternalCollectorSnapshot{"universal": {Text: "trojan://password@node.example:443#universal-node"}, "clash": {Text: "proxies: [{name: native-node, type: trojan, server: node.example, port: 443, password: synthetic}]"}}}
	w = request("/control/api/collector/results", input, "8.8.8.8", true, "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "fixture-secret-url") {
		t.Fatal("publication failed/leaked payload", w.Code, w.Body.String())
	}
	native, err := catalog.Resolve(sourceId, alice.Id, true)
	if err != nil || !strings.Contains(native, "native-node") {
		t.Fatal("published native format unavailable", err)
	}
	w = request("/control/api/collector/results", input, "8.8.8.8", true, "", "")
	if w.Code != 409 {
		t.Fatal("completed lease republished", w.Code, w.Body.String())
	}
}
