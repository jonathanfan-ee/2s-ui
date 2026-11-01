package api

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/logger"
)

func TestExternalSubSourceAPIAuthWriteOnlyAndRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.InitLogger(logging.ERROR)
	if err := database.InitDB(filepath.Join(t.TempDir(), "catalog-api.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.CloseDBForTest(); err != nil {
			t.Error(err)
		}
	})
	router := gin.New()
	router.Use(sessions.Sessions("test-session", cookie.NewStore([]byte("test-secret"))))
	(&APIHandler{}).initRouter(router.Group("/api"))
	tokens := []TokenInMemory{{Token: "test-token", Username: "admin"}}
	(&APIv2Handler{tokens: &tokens}).initRouter(router.Group("/apiv2"))
	request := func(method, path string, body interface{}, authenticated bool) (Msg, string) {
		t.Helper()
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		if authenticated {
			req.Header.Set("Token", "test-token")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var msg Msg
		if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
			t.Fatal(err, w.Body.String())
		}
		return msg, w.Body.String()
	}
	for _, prefix := range []string{"/api", "/apiv2"} {
		for _, action := range []string{"externalSubSources", "saveExternalSubSource", "deleteExternalSubSource"} {
			method := "POST"
			if action == "externalSubSources" {
				method = "GET"
			}
			msg, _ := request(method, prefix+"/"+action, map[string]interface{}{}, false)
			if msg.Success {
				t.Fatal("unauthenticated catalog action allowed", action)
			}
		}
	}
	alice := model.Client{Name: "alice", Enable: true, Links: json.RawMessage(`[]`)}
	if err := database.GetDB().Create(&alice).Error; err != nil {
		t.Fatal(err)
	}
	input := map[string]interface{}{"name": "provider", "url": "https://provider.example/sub?token=synthetic-secret", "enabled": true, "clientIds": []uint{alice.Id}}
	msg, raw := request("POST", "/apiv2/saveExternalSubSource", input, true)
	if !msg.Success || strings.Contains(raw, "synthetic-secret") {
		t.Fatal("create failed or URL leaked", raw)
	}
	msg, raw = request("GET", "/apiv2/externalSubSources", nil, true)
	if !msg.Success || strings.Contains(raw, "synthetic-secret") {
		t.Fatal("list leaked URL", raw)
	}
	var obj struct {
		Sources []struct {
			Id        string
			Revision  uint64
			ClientIds []uint
		}
	}
	data, _ := json.Marshal(msg.Obj)
	if err := json.Unmarshal(data, &obj); err != nil || len(obj.Sources) != 1 {
		t.Fatal("catalog missing", raw)
	}
	source := obj.Sources[0]
	if len(source.ClientIds) != 1 || source.ClientIds[0] != alice.Id {
		t.Fatal("grant not saved")
	}
	input = map[string]interface{}{"id": source.Id, "revision": source.Revision, "name": "renamed", "url": "", "enabled": true}
	msg, _ = request("POST", "/apiv2/saveExternalSubSource", input, true)
	if !msg.Success {
		t.Fatal(msg.Msg)
	}
	var stored model.ExternalSubSource
	if err := database.GetDB().First(&stored, "id = ?", source.Id).Error; err != nil || stored.URL != "https://provider.example/sub?token=synthetic-secret" {
		t.Fatal("blank edit cleared URL", err)
	}
	msg, _ = request("POST", "/apiv2/saveExternalSubSource", input, true)
	if msg.Success {
		t.Fatal("stale editor overwrote source")
	}
	msg, _ = request("POST", "/apiv2/deleteExternalSubSource", map[string]interface{}{"id": source.Id, "revision": stored.Revision}, true)
	if !msg.Success {
		t.Fatal(msg.Msg)
	}
	var count int64
	database.GetDB().Model(&model.ExternalSubGrant{}).Count(&count)
	if count != 0 {
		t.Fatal("delete retained grants")
	}
}
