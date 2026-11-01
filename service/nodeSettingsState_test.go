package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/op/go-logging"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/logger"

	"gorm.io/gorm"
)

func nodeSettingsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	logger.InitLogger(logging.CRITICAL)
	if err := database.InitDB(filepath.Join(t.TempDir(), "node-settings.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.CloseDBForTest(); err != nil {
			t.Error(err)
		}
	})
	return database.GetDB()
}

func saveNodeSettingsForTest(t *testing.T, db *gorm.DB, input map[string]any) {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var nodes NodeService
	if err := db.Transaction(func(tx *gorm.DB) error { return nodes.Save(tx, "edit", body) }); err != nil {
		t.Fatal(err)
	}
}

// A settings drawer opened before a user change must not erase the pending
// reconciliation when its address is restored after an outage. Runtime fields
// are server-owned even when an old API caller includes stale values.
func TestNodeSettingsEditPreservesPendingSync(t *testing.T) {
	db := nodeSettingsTestDB(t)
	node := model.Node{
		Name: "remote", Enable: true, BaseUrl: "https://old.example", WebPath: "/app/", Token: "kept-token",
		Dirty: true, LastSeen: 101, LastSync: 202,
		Baselines: json.RawMessage(`{"alice":{"up":100,"down":200}}`),
	}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	saveNodeSettingsForTest(t, db, map[string]any{
		"id": node.Id, "name": node.Name, "enable": false, "baseUrl": "https://restored.example", "webPath": "/control/",
		"token": "", "insecure": false, "certPin": "", "desc": "updated settings",
		"dirty": false, "lastSync": 0, "lastSeen": 0,
	})
	var got model.Node
	if err := db.First(&got, node.Id).Error; err != nil {
		t.Fatal(err)
	}
	if !got.Dirty || got.LastSync != node.LastSync || got.LastSeen != node.LastSeen || !jsonEqual(got.Baselines, node.Baselines) {
		t.Fatalf("settings erased pending sync/accounting: dirty=%v lastSync=%d lastSeen=%d baselines=%s", got.Dirty, got.LastSync, got.LastSeen, got.Baselines)
	}
	if got.Enable || got.BaseUrl != "https://restored.example" || got.WebPath != "/control/" || got.Desc != "updated settings" || got.Token != node.Token {
		t.Fatalf("editable settings were not saved correctly")
	}
}

// The first collection after editing a node must count only new traffic,
// rather than crediting every byte since the node started a second time.
func TestNodeSettingsEditDoesNotRecountTraffic(t *testing.T) {
	db := nodeSettingsTestDB(t)
	var remoteUp, remoteDown int64 = 100, 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/apiv2/clients" || r.Header.Get("Token") != "test-token" {
			t.Errorf("unexpected node request: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": map[string]any{"clients": []map[string]any{
			{"name": "alice", "group": "@cluster", "up": remoteUp, "down": remoteDown},
		}}})
	}))
	defer srv.Close()
	node := model.Node{Name: "remote", Enable: true, BaseUrl: srv.URL, WebPath: "/app/", Token: "test-token", Baselines: json.RawMessage(`{"alice":{"up":100,"down":200}}`)}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	client := model.Client{Name: "alice", Up: 1000, Down: 2000}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	saveNodeSettingsForTest(t, db, map[string]any{"id": node.Id, "name": node.Name, "enable": true, "baseUrl": srv.URL, "webPath": "/app/", "desc": "edited"})
	var sync NodeSyncService
	collect := func(wantUp, wantDown int64) {
		t.Helper()
		var current model.Node
		if err := db.First(&current, node.Id).Error; err != nil {
			t.Fatal(err)
		}
		if err := sync.collectNodeTraffic(&current); err != nil {
			t.Fatal(err)
		}
		var got model.Client
		if err := db.First(&got, client.Id).Error; err != nil {
			t.Fatal(err)
		}
		if got.Up != wantUp || got.Down != wantDown {
			t.Fatalf("master counters after settings edit = %d/%d; want %d/%d", got.Up, got.Down, wantUp, wantDown)
		}
	}
	collect(1000, 2000)
	remoteUp, remoteDown = 150, 400
	collect(1050, 2200)
	collect(1050, 2200)
}
