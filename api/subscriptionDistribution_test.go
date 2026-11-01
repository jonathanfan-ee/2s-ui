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

func TestSubscriptionDistributionAPIAuthAndWriteOnlyURLs(t *testing.T) {
	logger.InitLogger(logging.ERROR)
	if err := database.InitDB(filepath.Join(t.TempDir(), "distribution.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.CloseDBForTest() })
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(sessions.Sessions("test-session", cookie.NewStore([]byte("test-secret"))))
	(&APIHandler{}).initRouter(router.Group("/api"))
	tokens := []TokenInMemory{{Token: "test-token", Username: "admin"}}
	(&APIv2Handler{tokens: &tokens}).initRouter(router.Group("/apiv2"))
	request := func(method, path string, body interface{}, auth bool) (Msg, string) {
		t.Helper()
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		if auth {
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
		for _, action := range []string{"subscriptionDistribution", "subscriptionTemplate", "saveSubscriptionTemplate", "deleteSubscriptionTemplate", "saveLegacySubscriptionAlias", "deleteLegacySubscriptionAlias"} {
			method := "POST"
			if strings.HasPrefix(action, "subscription") {
				method = "GET"
			}
			msg, _ := request(method, prefix+"/"+action, nil, false)
			if msg.Success {
				t.Fatal("unauthenticated distribution allowed", action)
			}
		}
	}
	client := model.Client{Name: "alice", Enable: true}
	if err := database.GetDB().Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	msg, raw := request("POST", "/apiv2/saveSubscriptionTemplate", map[string]interface{}{"name": "rules", "format": "clash", "content": "rules: ['MATCH,DIRECT']", "enabled": true, "clientIds": []uint{client.Id}}, true)
	if !msg.Success {
		t.Fatal(raw)
	}
	msg, raw = request("POST", "/apiv2/saveLegacySubscriptionAlias", map[string]interface{}{"name": "old URL", "url": "https://legacy.example/private-bearer-token/shared", "clientId": client.Id, "enabled": true, "defaultFormat": "auto"}, true)
	if !msg.Success || strings.Contains(raw, "private-bearer-token") {
		t.Fatal(raw)
	}
	msg, raw = request("GET", "/apiv2/subscriptionDistribution", nil, true)
	if !msg.Success || strings.Contains(raw, "private-bearer-token") || strings.Contains(raw, "lookupKey") || strings.Contains(raw, "MATCH,DIRECT") {
		t.Fatal("catalog leaked sensitive detail", raw)
	}
}
