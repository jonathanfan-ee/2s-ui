package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"gorm.io/gorm"
)

func catalogClient(t *testing.T, name string, links string) model.Client {
	t.Helper()
	if links == "" {
		links = "[]"
	}
	client := model.Client{Name: name, Enable: true, Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`), Links: json.RawMessage(links)}
	if err := database.GetDB().Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	return client
}
func catalogSource(t *testing.T, id string) model.ExternalSubSource {
	t.Helper()
	var source model.ExternalSubSource
	if err := database.GetDB().Where("id = ?", id).First(&source).Error; err != nil {
		t.Fatal(err)
	}
	return source
}
func saveCatalogSource(t *testing.T, source model.ExternalSubSource, ids *[]uint, raw string, adopt bool) model.ExternalSubSource {
	t.Helper()
	var catalog ExternalSubSourceService
	id, err := catalog.Save(ExternalSubSourceWrite{Id: source.Id, Revision: source.Revision, Name: source.Name, URL: raw, Enabled: source.Enabled, ClientIds: ids, AdoptLegacy: adopt}, "test-admin")
	if err != nil {
		t.Fatal(err)
	}
	return catalogSource(t, id)
}

func TestExternalSubCatalogStableIdentityAndAuthorization(t *testing.T) {
	cache, _, _ := setupSnapshotDB(t)
	alice, bob := catalogClient(t, "alice", ""), catalogClient(t, "bob", "")
	var catalog ExternalSubSourceService
	source := saveCatalogSource(t, model.ExternalSubSource{Name: "provider", Enabled: true}, nil, snapshotTestURL, false)
	calls := 0
	cache.fetch = func(_ context.Context, _ string, reserve func() error) (externalSubFetchResult, error) {
		calls++
		if err := reserve(); err != nil {
			return externalSubFetchResult{}, err
		}
		return externalSubFetchResult{body: snapshotTestBody, status: 200}, nil
	}
	key := externalSubKey(snapshotTestURL)
	// An unused catalog source is visible and manually prewarmable, but does not
	// spend background requests even with automatic refresh enabled.
	if err := cache.Configure(ExternalSubCacheConfig{AutoRefresh: true, RefreshMinutes: 5}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := cache.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("unassigned source refreshed automatically")
	}
	if err := cache.Queue([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	if err := cache.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unassigned source did not prewarm")
	}
	if _, err := catalog.Resolve(source.Id, alice.Id); !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("warm snapshot granted access implicitly")
	}
	ids := []uint{alice.Id, alice.Id}
	source = saveCatalogSource(t, source, &ids, "", false)
	if body, err := catalog.Resolve(source.Id, alice.Id); err != nil || body != snapshotTestBody {
		t.Fatal("authorized read failed", err)
	}
	if _, err := catalog.Resolve(source.Id, bob.Id); !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("source id granted another client access")
	}
	view, err := catalog.List()
	if err != nil || len(view.Sources) != 1 || view.Sources[0].Cache.References != 1 || len(view.Sources[0].ClientIds) != 1 {
		t.Fatalf("catalog: %+v %v", view, err)
	}
	data, _ := json.Marshal(view)
	for _, secret := range []string{snapshotTestURL, "synthetic-secret", "synthetic-password", "payload"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("catalog leaked %q", secret)
		}
	}
	var audits []model.Changes
	if err := database.GetDB().Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(audits)
	if strings.Contains(string(data), "synthetic-secret") {
		t.Fatal("audit leaked URL")
	}
	originalId := source.Id
	source = saveCatalogSource(t, source, nil, "https://rotated.example/sub?token=synthetic-new-secret", false)
	if source.Id != originalId {
		t.Fatal("URL rotation changed source identity")
	}
	if _, err := catalog.Resolve(source.Id, alice.Id); !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("rotated URL used old snapshot")
	}
	if row := snapshotRow(t, key); row.Payload != snapshotTestBody {
		t.Fatal("rotation destroyed shared old-URL snapshot")
	}
	// A stale editor cannot overwrite a newer URL, enabled state or grants.
	_, err = catalog.Save(ExternalSubSourceWrite{Id: source.Id, Revision: source.Revision - 1, Name: "stale", Enabled: true}, "test")
	if !errors.Is(err, ErrExternalSubSourceConflict) {
		t.Fatal("stale edit accepted", err)
	}
	source.Enabled = false
	source = saveCatalogSource(t, source, nil, "", false)
	if granted, err := catalog.AuthorizedSources(alice.Id); err != nil || len(granted) != 0 {
		t.Fatal("disabled source exported")
	}
	if _, err := catalog.Resolve(source.Id, alice.Id); !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("disabled source resolved")
	}
	if err := catalog.Delete(source.Id, source.Revision-1, "test"); !errors.Is(err, ErrExternalSubSourceConflict) {
		t.Fatal("stale delete accepted")
	}
	if err := catalog.Delete(source.Id, source.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	var grants int64
	database.GetDB().Model(&model.ExternalSubGrant{}).Count(&grants)
	if grants != 0 {
		t.Fatal("deleted source retained grants")
	}
	if catalogSourceClientName(t, alice.Id) != "alice" {
		t.Fatal("source operations changed user subscription identity")
	}
}
func catalogSourceClientName(t *testing.T, id uint) string {
	t.Helper()
	var client model.Client
	if err := database.GetDB().First(&client, id).Error; err != nil {
		t.Fatal(err)
	}
	return client.Name
}

func TestExternalSubCatalogAdoptionIsExplicitAtomicAndWarm(t *testing.T) {
	_, now, _ := setupSnapshotDB(t)
	links, _ := json.Marshal([]map[string]interface{}{
		{"type": "sub", "uri": snapshotTestURL, "custom": map[string]bool{"retainElsewhere": true}},
		{"type": "local", "uri": "trojan://local@example.com:443#local", "remark": "local"},
		{"type": "sub", "uri": snapshotTestURL + "&different=1", "custom": map[string]bool{"keep": true}},
	})
	alice, bob := catalogClient(t, "alice", string(links)), catalogClient(t, "bob", string(links))
	source := saveCatalogSource(t, model.ExternalSubSource{Name: "provider", Enabled: true}, nil, snapshotTestURL, false)
	var catalog ExternalSubSourceService
	ids := []uint{alice.Id}
	_, err := catalog.Save(ExternalSubSourceWrite{Id: source.Id, Revision: source.Revision, Name: source.Name, Enabled: true, ClientIds: &ids, AdoptLegacy: true}, "test")
	if err == nil {
		t.Fatal("cold source adopted")
	}
	if catalogSource(t, source.Id).Revision != source.Revision {
		t.Fatal("failed adoption modified source")
	}
	if err := database.GetDB().Create(&model.ExternalSubSnapshot{Key: externalSubKey(snapshotTestURL), Payload: snapshotTestBody, LastSuccess: *now}).Error; err != nil {
		t.Fatal(err)
	}
	// Merely granting access does not mutate legacy links.
	source = saveCatalogSource(t, source, &ids, "", false)
	var stored model.Client
	database.GetDB().First(&stored, alice.Id)
	if string(stored.Links) != string(links) {
		t.Fatal("grant silently migrated legacy links")
	}
	source = saveCatalogSource(t, source, &ids, "", true)
	database.GetDB().First(&stored, alice.Id)
	if stored.Name != alice.Name || string(stored.Config) != string(alice.Config) {
		t.Fatal("adoption changed user identity or protocol credentials")
	}
	var kept []map[string]interface{}
	if err := json.Unmarshal(stored.Links, &kept); err != nil || len(kept) != 2 || kept[1]["custom"] == nil {
		t.Fatal("adoption did not preserve other links", string(stored.Links), err)
	}
	stored = model.Client{}
	database.GetDB().First(&stored, bob.Id)
	if string(stored.Links) != string(links) {
		t.Fatal("adoption touched unselected client")
	}
	bad := []uint{alice.Id, 999999}
	_, err = catalog.Save(ExternalSubSourceWrite{Id: source.Id, Revision: source.Revision, Name: "changed", URL: "https://new.example/sub", Enabled: true, ClientIds: &bad}, "test")
	if err == nil {
		t.Fatal("nonexistent client accepted")
	}
	if fresh := catalogSource(t, source.Id); fresh.Name != source.Name || fresh.URL != source.URL || fresh.Revision != source.Revision {
		t.Fatal("failed save was not atomic")
	}
	// One active client using the same URL through two independent paths still
	// contributes one reference and one scheduled request.
	refs, err := activeExternalSubs(database.GetDB(), *now)
	if err != nil || refs[externalSubKey(snapshotTestURL)].references != 2 {
		t.Fatal("catalog/legacy reference dedup failed", err, refs)
	}
}

func TestExternalSubCatalogRevokeExpiryAndClientDelete(t *testing.T) {
	_, now, _ := setupSnapshotDB(t)
	alice := catalogClient(t, "alice", "")
	ids := []uint{alice.Id}
	source := saveCatalogSource(t, model.ExternalSubSource{Name: "provider", Enabled: true}, &ids, snapshotTestURL, false)
	if err := database.GetDB().Create(&model.ExternalSubSnapshot{Key: externalSubKey(snapshotTestURL), Payload: snapshotTestBody, LastSuccess: *now}).Error; err != nil {
		t.Fatal(err)
	}
	var catalog ExternalSubSourceService
	for _, change := range []map[string]interface{}{{"enable": false}, {"enable": true, "expiry": time.Now().Unix() - 1}} {
		if err := database.GetDB().Model(&model.Client{}).Where("id = ?", alice.Id).Updates(change).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Resolve(source.Id, alice.Id); !errors.Is(err, ErrExternalSubCacheUnavailable) {
			t.Fatal("inactive client received a snapshot")
		}
	}
	database.GetDB().Model(&model.Client{}).Where("id = ?", alice.Id).Updates(map[string]interface{}{"enable": true, "expiry": 0})
	empty := []uint{}
	source = saveCatalogSource(t, source, &empty, "", false)
	if _, err := catalog.Resolve(source.Id, alice.Id); !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("revoked client received snapshot")
	}
	source = saveCatalogSource(t, source, &ids, "", false)
	var clients ClientService
	data, _ := json.Marshal(alice.Id)
	if err := database.GetDB().Transaction(func(tx *gorm.DB) error { _, err := clients.Save(tx, "del", data, "panel.example"); return err }); err != nil {
		t.Fatal(err)
	}
	var count int64
	database.GetDB().Model(&model.ExternalSubGrant{}).Where("client_id = ?", alice.Id).Count(&count)
	if count != 0 {
		t.Fatal("client delete did not remove source grants")
	}
}

func TestExternalSubCatalogConcurrentEditsUseRevision(t *testing.T) {
	setupSnapshotDB(t)
	source := saveCatalogSource(t, model.ExternalSubSource{Name: "provider", Enabled: true}, nil, snapshotTestURL, false)
	outcomes := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func(name string) {
			var catalog ExternalSubSourceService
			_, err := catalog.Save(ExternalSubSourceWrite{Id: source.Id, Revision: source.Revision, Name: name, Enabled: true}, "test")
			outcomes <- err
		}(name)
	}
	success, conflict := 0, 0
	for range 2 {
		err := <-outcomes
		if err == nil {
			success++
		} else if errors.Is(err, ErrExternalSubSourceConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent edits: success=%d conflict=%d", success, conflict)
	}
}

func TestExternalSubCatalogRotationFencesInFlightPublication(t *testing.T) {
	cache, _, _ := setupSnapshotDB(t)
	alice := catalogClient(t, "alice", "")
	ids := []uint{alice.Id}
	source := saveCatalogSource(t, model.ExternalSubSource{Name: "provider", Enabled: true}, &ids, snapshotTestURL, false)
	oldKey := externalSubKey(source.URL)
	cache.fetch = func(_ context.Context, _ string, _ func() error) (externalSubFetchResult, error) {
		saveCatalogSource(t, source, nil, "https://rotated.example/sub", false)
		return externalSubFetchResult{body: snapshotTestBody, status: 200}, nil
	}
	if err := cache.Queue([]string{oldKey}, false); err != nil {
		t.Fatal(err)
	}
	if err := cache.RefreshOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := snapshotRow(t, oldKey)
	if row.Payload != "" || row.LastSuccess != 0 || row.LastError != "source-unassigned" {
		t.Fatal("obsolete URL job published a snapshot")
	}
	var catalog ExternalSubSourceService
	if _, err := catalog.Resolve(source.Id, alice.Id); !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("rotated catalog source exposed obsolete response")
	}
}
