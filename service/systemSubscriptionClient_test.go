package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/shenaba/2s-ui/database/model"
)

// Exercise the real deletion path, including policy cleanup. A mixed batch must
// not silently remove the ordinary user or the reader's source authorization.
func TestSystemSubscriptionClientDeletionProtected(t *testing.T) {
	for _, action := range []string{"del", "delbulk"} {
		t.Run(action, func(t *testing.T) {
			db := newTestDB(t)
			reader := model.Client{Name: "reader-token", Group: sourceFeedGroup, Inbounds: json.RawMessage(`[]`), Links: json.RawMessage(`[]`)}
			ordinary := model.Client{Name: "employee", Group: "staff", Inbounds: json.RawMessage(`[]`), Links: json.RawMessage(`[]`)}
			for _, client := range []*model.Client{&reader, &ordinary} {
				if err := db.Create(client).Error; err != nil {
					t.Fatal(err)
				}
			}
			grant := model.ExternalSubGrant{SourceId: "source-one", ClientId: reader.Id}
			if err := db.Create(&grant).Error; err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if action == "del" {
				payload, _ = json.Marshal(reader.Id)
			} else {
				payload, _ = json.Marshal([]uint{ordinary.Id, reader.Id})
			}
			_, err := (&ClientService{}).Save(db, action, payload, "panel.example")
			if !errors.Is(err, ErrSystemSubscriptionClientProtected) {
				t.Fatalf("delete error = %v", err)
			}
			var count int64
			if err := db.Model(&model.Client{}).Count(&count).Error; err != nil || count != 2 {
				t.Fatalf("clients = %d, err = %v", count, err)
			}
			if err := db.Model(&model.ExternalSubGrant{}).Where("client_id = ?", reader.Id).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("source grants = %d, err = %v", count, err)
			}
		})
	}
}

func TestOrdinarySubscriptionClientStillDeletable(t *testing.T) {
	for _, action := range []string{"del", "delbulk"} {
		t.Run(action, func(t *testing.T) {
			db := newTestDB(t)
			client := model.Client{Name: "ordinary", Group: "staff", Inbounds: json.RawMessage(`[]`), Links: json.RawMessage(`[]`)}
			if err := db.Create(&client).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.ExternalSubGrant{SourceId: "source-one", ClientId: client.Id}).Error; err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if action == "del" {
				payload, _ = json.Marshal(client.Id)
			} else {
				payload, _ = json.Marshal([]uint{client.Id})
			}
			if _, err := (&ClientService{}).Save(db, action, payload, "panel.example"); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := db.Model(&model.Client{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("clients = %d, err = %v", count, err)
			}
			if err := db.Model(&model.ExternalSubGrant{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("source grants = %d, err = %v", count, err)
			}
		})
	}
}
