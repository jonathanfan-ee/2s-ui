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

func TestExternalSubCacheAPIAuthorizationAndQueue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.InitLogger(logging.ERROR)
	if err := database.InitDB(filepath.Join(t.TempDir(), "api-cache.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.CloseDBForTest(); err != nil {
			t.Error(err)
		}
	})
	tokens := []TokenInMemory{{Token: "test-token", Username: "admin"}}
	router := gin.New()
	router.Use(sessions.Sessions("test-session", cookie.NewStore([]byte("test-secret"))))
	(&APIHandler{}).initRouter(router.Group("/api"))
	(&APIv2Handler{tokens: &tokens}).initRouter(router.Group("/apiv2"))
	for _, prefix := range []string{"/api", "/apiv2"} {
		for _, action := range []struct{ method, path, body string }{{"GET", "externalSubCache", ""}, {"POST", "configureExternalSubCache", `{"enabled":true,"refreshMinutes":5}`}, {"POST", "refreshExternalSubs", `{"keys":[]}`}} {
			req := httptest.NewRequest(action.method, prefix+"/"+action.path, strings.NewReader(action.body))
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			var msg Msg
			if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil || msg.Success {
				t.Fatalf("unauthenticated action allowed %s/%s: %s", prefix, action.path, w.Body.String())
			}
		}
	}
	links := json.RawMessage(`[{"type":"sub","uri":"https://provider.example/sub?token=synthetic-secret","remark":"provider"}]`)
	if err := database.GetDB().Create(&model.Client{Name: "alice", Enable: true, Links: links}).Error; err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) Msg {
		t.Helper()
		req := httptest.NewRequest(method, "/apiv2/"+path, strings.NewReader(body))
		req.Header.Set("Token", "test-token")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var msg Msg
		if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
			t.Fatal(err)
		}
		return msg
	}
	msg := request("GET", "externalSubCache", "")
	if !msg.Success {
		t.Fatal(msg.Msg)
	}
	data, _ := json.Marshal(msg.Obj)
	if strings.Contains(string(data), "synthetic-secret") {
		t.Fatal("status leaked source URL")
	}
	var status struct{ Sources []struct{ Key string } }
	if err := json.Unmarshal(data, &status); err != nil || len(status.Sources) != 1 {
		t.Fatal("source status missing", string(data))
	}
	msg = request("POST", "configureExternalSubCache", `{"enabled":true,"refreshMinutes":5}`)
	if msg.Success {
		t.Fatal("API enabled cold cache")
	}
	msg = request("POST", "refreshExternalSubs", `{"keys":["unassigned-key"]}`)
	if msg.Success {
		t.Fatal("API queued unassigned source")
	}
	msg = request("POST", "refreshExternalSubs", `{"keys":["`+status.Sources[0].Key+`"]}`)
	if !msg.Success {
		t.Fatal(msg.Msg)
	}
	var row model.ExternalSubSnapshot
	if err := database.GetDB().First(&row).Error; err != nil || !row.Requested || row.LastAttempt != 0 {
		t.Fatal("queue fetched inline or was not saved", err)
	}
}
