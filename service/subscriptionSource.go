package service

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"gorm.io/gorm"
	ormlog "gorm.io/gorm/logger"
)

var ErrExternalSubSourceConflict = errors.New("source-changed-reload-before-saving")

// SourceWrite intentionally has no snapshot payload or user configuration.
// An omitted clientIds preserves grants; an explicit [] revokes all grants.
// Empty URL on edit retains the write-only stored URL.
type ExternalSubSourceWrite struct {
	Id          string  `json:"id"`
	Revision    uint64  `json:"revision"`
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	Enabled     bool    `json:"enabled"`
	ClientIds   *[]uint `json:"clientIds"`
	AdoptLegacy bool    `json:"adoptLegacy"`
}

type ExternalSubSourceView struct {
	Id        string                 `json:"id"`
	Name      string                 `json:"name"`
	Host      string                 `json:"host"`
	Enabled   bool                   `json:"enabled"`
	Revision  uint64                 `json:"revision"`
	ClientIds []uint                 `json:"clientIds"`
	Cache     ExternalSubCacheStatus `json:"cache"`
}

type ExternalSubCatalogClient struct {
	Id     uint   `json:"id"`
	Name   string `json:"name"`
	Enable bool   `json:"enable"`
	Expiry int64  `json:"expiry"`
}

type ExternalSubCatalog struct {
	Sources []ExternalSubSourceView    `json:"sources"`
	Clients []ExternalSubCatalogClient `json:"clients"`
}

type ExternalSubSourceService struct{}

func (*ExternalSubSourceService) List() (*ExternalSubCatalog, error) {
	db := database.GetDB()
	var sources []model.ExternalSubSource
	if err := db.Order("name, id").Find(&sources).Error; err != nil {
		return nil, err
	}
	var grants []model.ExternalSubGrant
	if err := db.Order("client_id").Find(&grants).Error; err != nil {
		return nil, err
	}
	status, err := externalSubSnapshotMetadata(db, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	result := &ExternalSubCatalog{Sources: []ExternalSubSourceView{}, Clients: []ExternalSubCatalogClient{}}
	if err := db.Model(&model.Client{}).Select("id, name, enable, expiry").Order("name, id").Scan(&result.Clients).Error; err != nil {
		return nil, err
	}
	activeClients := map[uint]bool{}
	for _, client := range result.Clients {
		activeClients[client.Id] = client.Enable && (client.Expiry == 0 || client.Expiry > time.Now().Unix())
	}
	grantIds := map[string][]uint{}
	for _, grant := range grants {
		grantIds[grant.SourceId] = append(grantIds[grant.SourceId], grant.ClientId)
	}
	for _, source := range sources {
		parsed, _ := url.Parse(source.URL)
		host := ""
		if parsed != nil {
			host = parsed.Hostname()
		}
		ids := grantIds[source.Id]
		if ids == nil {
			ids = []uint{}
		}
		key := externalSubKey(source.URL)
		cache := status[key]
		cache.Key = key
		cache.Name = source.Name
		cache.Host = host
		for _, id := range ids {
			if activeClients[id] {
				cache.References++
			}
		}
		result.Sources = append(result.Sources, ExternalSubSourceView{Id: source.Id, Name: source.Name, Host: host, Enabled: source.Enabled, Revision: source.Revision, ClientIds: ids, Cache: cache})
	}
	return result, nil
}

// Save updates the source and all explicitly supplied grants in one transaction.
// Its audit entry records ids and whether the URL changed, never the URL itself.
func (*ExternalSubSourceService) Save(input ExternalSubSourceWrite, actor string) (string, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || utf8.RuneCountInString(name) > 128 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", errors.New("source-name-required-max-128-characters")
	}
	raw := strings.TrimSpace(input.URL)
	if len(raw) > 8192 {
		return "", errors.New("source-url-too-long")
	}
	if raw != "" {
		if err := validateCachedSubURL(raw); err != nil {
			return "", err
		}
	}
	now := time.Now().Unix()
	id := input.Id
	if id != "" && !validExternalSourceId(id) {
		return "", errors.New("source-id-invalid")
	}
	// Debug SQL must not echo a credential-bearing URL on a failed write.
	err := database.GetDB().Session(&gorm.Session{Logger: ormlog.Discard}).Transaction(func(tx *gorm.DB) error {
		var source model.ExternalSubSource
		if id == "" {
			if raw == "" {
				return errors.New("source-url-required")
			}
			id = rand.Text()
			source = model.ExternalSubSource{Id: id, CreatedAt: now}
		} else {
			if err := tx.Where("id = ?", id).First(&source).Error; err != nil {
				return err
			}
			if input.Revision != source.Revision {
				return ErrExternalSubSourceConflict
			}
		}
		oldURL := source.URL
		source.Name = name
		source.Enabled = input.Enabled
		source.Revision++
		source.UpdatedAt = now
		if raw != "" {
			source.URL = raw
		}
		if err := tx.Save(&source).Error; err != nil {
			return err
		}
		if input.AdoptLegacy && input.ClientIds == nil {
			return errors.New("select-clients-before-adopting-legacy-links")
		}
		var ids []uint
		if input.ClientIds != nil {
			ids = slices.Clone(*input.ClientIds)
			slices.Sort(ids)
			ids = slices.Compact(ids)
			if len(ids) > 1000 || (len(ids) > 0 && ids[0] == 0) {
				return errors.New("invalid-source-client-selection")
			}
			if len(ids) > 0 {
				var count int64
				if err := tx.Model(&model.Client{}).Where("id in ?", ids).Count(&count).Error; err != nil {
					return err
				}
				if count != int64(len(ids)) {
					return errors.New("source-client-not-found")
				}
			}
			if err := tx.Where("source_id = ?", id).Delete(&model.ExternalSubGrant{}).Error; err != nil {
				return err
			}
			grants := make([]model.ExternalSubGrant, 0, len(ids))
			for _, clientId := range ids {
				grants = append(grants, model.ExternalSubGrant{SourceId: id, ClientId: clientId})
			}
			if len(grants) > 0 {
				if err := tx.Create(&grants).Error; err != nil {
					return err
				}
			}
		}
		if input.AdoptLegacy {
			var ready int64
			if err := tx.Model(&model.ExternalSubSnapshot{}).Where("key = ? AND last_success > 0 AND length(payload) > 0", externalSubKey(source.URL)).Count(&ready).Error; err != nil {
				return err
			}
			if ready == 0 {
				return errors.New("prewarm-source-before-adopting-legacy-links")
			}
			if err := adoptLegacyExternalSubs(tx, ids, source.URL); err != nil {
				return err
			}
		}
		audit, _ := json.Marshal(map[string]interface{}{"id": id, "name": name, "enabled": source.Enabled, "revision": source.Revision, "urlChanged": oldURL != source.URL, "clientIds": input.ClientIds, "adoptLegacy": input.AdoptLegacy})
		return tx.Create(&model.Changes{DateTime: now, Actor: actor, Key: "subscription-sources", Action: "save", Obj: audit}).Error
	})
	if err != nil {
		return "", err
	}
	SetLastUpdate(now)
	return id, nil
}

func (*ExternalSubSourceService) Delete(id string, revision uint64, actor string) error {
	if !validExternalSourceId(id) {
		return errors.New("source-id-invalid")
	}
	now := time.Now().Unix()
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		var source model.ExternalSubSource
		if err := tx.Where("id = ?", id).First(&source).Error; err != nil {
			return err
		}
		if source.Revision != revision {
			return ErrExternalSubSourceConflict
		}
		if err := tx.Where("source_id = ?", id).Delete(&model.ExternalSubGrant{}).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", id).Delete(&model.ExternalSubSource{}).Error; err != nil {
			return err
		}
		audit, _ := json.Marshal(map[string]interface{}{"id": id})
		return tx.Create(&model.Changes{DateTime: now, Actor: actor, Key: "subscription-sources", Action: "delete", Obj: audit}).Error
	})
	if err == nil {
		SetLastUpdate(now)
	}
	return err
}

// AuthorizedSources scopes catalog expansion to this active client. URLs stay
// internal and retain their model-level JSON exclusion.
func (*ExternalSubSourceService) AuthorizedSources(clientId uint) ([]model.ExternalSubSource, error) {
	var sources []model.ExternalSubSource
	err := database.GetDB().Model(&model.ExternalSubSource{}).
		Select("external_sub_sources.*").
		Joins("JOIN external_sub_grants ON external_sub_grants.source_id = external_sub_sources.id").
		Joins("JOIN clients ON clients.id = external_sub_grants.client_id").
		Where("clients.id = ? AND clients.enable = ? AND (clients.expiry = 0 OR clients.expiry > ?) AND external_sub_sources.enabled = ?", clientId, true, time.Now().Unix(), true).
		Order("external_sub_sources.name, external_sub_sources.id").Find(&sources).Error
	return sources, err
}

// Resolve always reads a snapshot, even while legacy URL caching is off. A
// catalog reference cannot trigger a live fetch or authorize a different user.
func (*ExternalSubSourceService) Resolve(id string, clientId uint) (string, error) {
	if !validExternalSourceId(id) || clientId == 0 {
		return "", ErrExternalSubCacheUnavailable
	}
	var source model.ExternalSubSource
	err := database.GetDB().Model(&model.ExternalSubSource{}).
		Select("external_sub_sources.*").
		Joins("JOIN external_sub_grants ON external_sub_grants.source_id = external_sub_sources.id").
		Joins("JOIN clients ON clients.id = external_sub_grants.client_id").
		Where("external_sub_sources.id = ? AND external_sub_sources.enabled = ? AND clients.id = ? AND clients.enable = ? AND (clients.expiry = 0 OR clients.expiry > ?)", id, true, clientId, true, time.Now().Unix()).First(&source).Error
	if err != nil {
		return "", ErrExternalSubCacheUnavailable
	}
	var cache ExternalSubCacheService
	return cache.snapshot(source.URL)
}

// Adoption touches only exact matching external-subscription entries in the
// selected clients. Local/node links and every other client field are retained.
func adoptLegacyExternalSubs(tx *gorm.DB, ids []uint, raw string) error {
	if len(ids) == 0 {
		return nil
	}
	var clients []model.Client
	if err := tx.Select("id, links").Where("id in ?", ids).Find(&clients).Error; err != nil {
		return err
	}
	for _, client := range clients {
		var links []map[string]json.RawMessage
		if len(client.Links) == 0 {
			continue
		}
		if err := json.Unmarshal(client.Links, &links); err != nil {
			return errors.New("client-links-invalid-cannot-adopt")
		}
		kept := make([]map[string]json.RawMessage, 0, len(links))
		changed := false
		for _, link := range links {
			var kind, uri string
			_ = json.Unmarshal(link["type"], &kind)
			_ = json.Unmarshal(link["uri"], &uri)
			if kind == "sub" && uri == raw {
				changed = true
				continue
			}
			kept = append(kept, link)
		}
		if !changed {
			continue
		}
		data, err := json.Marshal(kept)
		if err != nil {
			return err
		}
		if err := tx.Model(&model.Client{}).Where("id = ?", client.Id).Update("links", json.RawMessage(data)).Error; err != nil {
			return err
		}
	}
	return nil
}

func validExternalSourceId(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
