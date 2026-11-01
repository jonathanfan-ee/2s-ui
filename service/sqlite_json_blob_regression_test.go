package service

import (
	"encoding/json"
	"testing"

	"github.com/shenaba/2s-ui/database/model"
)

func TestTextJSONStoredAsBlobIsNotTreatedAsJSONB(t *testing.T) {
	db := newTestDB(t)
	c := model.Client{Name: "blob-case", Enable: true, Inbounds: json.RawMessage(`[1,16]`), Config: json.RawMessage(`{"hysteria2":{"name":"blob-case","password":"test-only"}}`), Links: json.RawMessage(`[]`)}
	if err := db.Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	var storage string
	db.Raw("select typeof(inbounds) from clients where id=?", c.Id).Scan(&storage)
	if storage != "blob" {
		t.Fatalf("want BLOB fixture, got %s", storage)
	}
	var svc InboundService
	users, err := svc.fetchUsers(db, "hysteria2", "? IN (SELECT value FROM json_each(CAST(clients.inbounds AS TEXT)))", map[string]interface{}{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("want one user, got %d", len(users))
	}
	// addUsers owns the real production membership condition; the fixture must survive it too.
	payload, err := svc.addUsers(db, []byte(`{"type":"hysteria2","tag":"test"}`), 1, "hysteria2")
	if err != nil {
		t.Fatalf("valid text JSON BLOB rejected: %v", err)
	}
	var decoded struct {
		Users []map[string]interface{} `json:"users"`
	}
	if err = json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Users) != 1 {
		t.Fatalf("membership lost: %s", payload)
	}
}

func TestFetchUsersTreatsLegacyConfigBlobAsText(t *testing.T) {
	db := newTestDB(t)
	for _, client := range []model.Client{
		{Name: "legacy", Enable: true, Inbounds: json.RawMessage(`[1]`), Config: json.RawMessage(`{"a":12}`), Links: json.RawMessage(`[]`)},
		{Name: "current", Enable: true, Inbounds: json.RawMessage(`[1]`), Config: json.RawMessage(`{"hysteria2":{"name":"current","password":"test-only"}}`), Links: json.RawMessage(`[]`)},
	} {
		if err := db.Create(&client).Error; err != nil {
			t.Fatal(err)
		}
	}
	var svc InboundService
	users, err := svc.fetchUsers(db, "hysteria2", "1=1", map[string]interface{}{})
	if err != nil {
		t.Fatalf("valid legacy text JSON must not poison other clients: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected only the client with protocol credentials, got %d", len(users))
	}
}

func TestMembershipOperationsTreatClientGrantBlobAsText(t *testing.T) {
	db := newTestDB(t)
	client := model.Client{Name: "grants", Enable: true, Inbounds: json.RawMessage(`[1,16]`), Config: json.RawMessage(`{}`), Links: json.RawMessage(`[]`)}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	var inbounds InboundService
	names, err := inbounds.enabledClientNames(db, 16)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := names[client.Name]; !ok || len(names) != 1 {
		t.Fatalf("expected exact enabled membership, got %v", names)
	}
	var clients ClientService
	if err := clients.UpdateClientsOnInboundDelete(db, 1, "removed"); err != nil {
		t.Fatalf("grant cleanup rejected valid JSON: %v", err)
	}
	if err := db.First(&client, client.Id).Error; err != nil {
		t.Fatal(err)
	}
	var grants []uint
	if err := json.Unmarshal(client.Inbounds, &grants); err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0] != 16 {
		t.Fatalf("unexpected remaining grants: %v", grants)
	}
}
