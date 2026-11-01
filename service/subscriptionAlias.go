package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	ormlog "gorm.io/gorm/logger"
)

var ErrLegacySubscriptionQuery = errors.New("legacy-query-unsupported")

type LegacySubscriptionAliasWrite struct {
	Id              string `json:"id"`
	Name            string `json:"name"`
	URL             string `json:"url"`
	ClientId        uint   `json:"clientId"`
	Enabled         bool   `json:"enabled"`
	Expiry          int64  `json:"expiry"`
	DefaultFormat   string `json:"defaultFormat"`
	ClashTemplateId string `json:"clashTemplateId"`
	JsonTemplateId  string `json:"jsonTemplateId"`
	Revision        uint64 `json:"revision"`
}
type LegacySubscriptionAliasService struct{}

// CanonicalLegacyHost uses the actual Host header, not forwarded host headers.
func CanonicalLegacyHost(host string) (string, error) {
	if strings.ContainsFunc(host, unicode.IsControl) {
		return "", errors.New("legacy-host-invalid")
	}
	if strings.Contains(host, ":") {
		h, p, err := net.SplitHostPort(host)
		if err != nil {
			return "", errors.New("legacy-host-invalid")
		}
		if _, err := strconv.ParseUint(p, 10, 16); err != nil {
			return "", errors.New("legacy-host-invalid")
		}
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/\\ @?#") {
		return "", errors.New("legacy-host-invalid")
	}
	for _, r := range host {
		if r > 127 {
			return "", errors.New("legacy-host-must-use-ascii-dns-name")
		}
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("legacy-host-invalid")
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", errors.New("legacy-host-invalid")
			}
		}
	}
	return host, nil
}
func legacyLookupKey(host, escapedPath, token string) string {
	// Normalize percent escape case without treating an escaped slash as a path separator.
	bytes := []byte(escapedPath)
	for i := 0; i+2 < len(bytes); i++ {
		if bytes[i] == '%' {
			bytes[i+1] = byte(strings.ToUpper(string(bytes[i+1]))[0])
			bytes[i+2] = byte(strings.ToUpper(string(bytes[i+2]))[0])
			i += 2
		}
	}
	sum := sha256.Sum256([]byte(host + "\n" + string(bytes) + "\n" + token))
	return hex.EncodeToString(sum[:])
}
func legacyURLIdentity(raw string) (string, string, error) {
	if len(raw) > 8192 {
		return "", "", errors.New("legacy-url-too-long")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return "", "", errors.New("legacy-url-must-be-https")
	}
	host, err := CanonicalLegacyHost(parsed.Host)
	if err != nil {
		return "", "", err
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if strings.ContainsAny(parsed.Path, "\\\r\n\x00") || strings.ContainsFunc(parsed.Path, unicode.IsControl) {
		return "", "", errors.New("legacy-path-invalid")
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == "." || part == ".." {
			return "", "", errors.New("legacy-path-invalid")
		}
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || !validLegacyQuery(query) {
		return "", "", errors.New("legacy-query-unsupported")
	}
	if path == "/" && query.Get("token") == "" {
		return "", "", errors.New("legacy-root-requires-token")
	}
	return host, legacyLookupKey(host, path, query.Get("token")), nil
}
func validLegacyQuery(query url.Values) bool {
	for key, values := range query {
		switch key {
		case "token", "target", "format", "clash", "singbox", "base64", "v2ray", "trojan", "plain":
		default:
			return false
		}
		if len(values) != 1 {
			return false
		}
	}
	return true
}
func legacyFormatAllowed(format string) bool {
	switch format {
	case "auto", "clash", "json", "base64", "plain":
		return true
	}
	return false
}
func (*LegacySubscriptionAliasService) List() ([]model.LegacySubscriptionAlias, error) {
	result := []model.LegacySubscriptionAlias{}
	err := database.GetDB().Order("host,name,id").Find(&result).Error
	return result, err
}
func (*LegacySubscriptionAliasService) Save(input LegacySubscriptionAliasWrite, actor string) (string, error) {
	if !validSubscriptionLabel(input.Name) || input.ClientId == 0 || input.Expiry < 0 || input.Expiry > 253402300799 || !legacyFormatAllowed(input.DefaultFormat) {
		return "", errors.New("legacy-alias-fields-invalid")
	}
	if input.Id != "" && !validExternalSourceId(input.Id) {
		return "", errors.New("legacy-alias-id-invalid")
	}
	host, key := "", ""
	if input.URL != "" {
		var err error
		host, key, err = legacyURLIdentity(input.URL)
		if err != nil {
			return "", err
		}
	}
	id := input.Id
	err := database.GetDB().Session(&gorm.Session{Logger: ormlog.Discard}).Transaction(func(tx *gorm.DB) error {
		var entry model.LegacySubscriptionAlias
		if id == "" {
			if key == "" {
				return errors.New("legacy-url-required")
			}
			id = rand.Text()
			entry.Id = id
		} else {
			if err := tx.Where("id = ?", id).First(&entry).Error; err != nil {
				return err
			}
			if entry.Revision != input.Revision {
				return ErrSubscriptionRevision
			}
		}
		if key != "" {
			entry.Host = host
			entry.LookupKey = key
		}
		var client model.Client
		if err := tx.Where("id = ?", input.ClientId).First(&client).Error; err != nil {
			return errors.New("legacy-client-not-found")
		}
		for format, templateId := range map[string]string{"clash": input.ClashTemplateId, "json": input.JsonTemplateId} {
			if templateId != "" {
				var count int64
				if err := tx.Model(&model.SubscriptionTemplate{}).Where("id = ? AND format = ?", templateId, format).Count(&count).Error; err != nil {
					return err
				}
				if count != 1 {
					return errors.New("legacy-template-not-found")
				}
			}
		}
		var current []model.Setting
		if err := tx.Where("key in ?", []string{"subDomain", "webDomain"}).Find(&current).Error; err != nil {
			return err
		}
		for _, setting := range current {
			if domain, err := CanonicalLegacyHost(setting.Value); err == nil && domain == entry.Host {
				return errors.New("legacy-host-must-be-dedicated")
			}
		}
		entry.Name = strings.TrimSpace(input.Name)
		entry.ClientId = input.ClientId
		entry.Enabled = input.Enabled
		entry.Expiry = input.Expiry
		entry.DefaultFormat = input.DefaultFormat
		entry.ClashTemplateId = input.ClashTemplateId
		entry.JsonTemplateId = input.JsonTemplateId
		entry.Revision++
		var count int64
		if err := tx.Model(&model.LegacySubscriptionAlias{}).Where("lookup_key = ? AND id <> ?", entry.LookupKey, id).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return errors.New("legacy-url-already-mapped")
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.LegacySubscriptionHost{Host: entry.Host}).Error; err != nil {
			return err
		}
		if err := tx.Save(&entry).Error; err != nil {
			return err
		}
		audit, _ := json.Marshal(map[string]interface{}{"id": id, "clientId": entry.ClientId, "host": entry.Host, "enabled": entry.Enabled, "expiry": entry.Expiry, "revision": entry.Revision})
		return tx.Create(&model.Changes{DateTime: time.Now().Unix(), Actor: actor, Key: "legacy-subscription-aliases", Action: "save", Obj: audit}).Error
	})
	if err != nil {
		return "", err
	}
	SetLastUpdate(time.Now().Unix())
	return id, nil
}
func (*LegacySubscriptionAliasService) Delete(id string, revision uint64, actor string) error {
	if !validExternalSourceId(id) {
		return errors.New("legacy-alias-id-invalid")
	}
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		var entry model.LegacySubscriptionAlias
		if err := tx.Where("id = ?", id).First(&entry).Error; err != nil {
			return err
		}
		if entry.Revision != revision {
			return ErrSubscriptionRevision
		}
		if err := tx.Where("id = ?", id).Delete(&model.LegacySubscriptionAlias{}).Error; err != nil {
			return err
		}
		audit, _ := json.Marshal(map[string]interface{}{"id": id})
		return tx.Create(&model.Changes{DateTime: time.Now().Unix(), Actor: actor, Key: "legacy-subscription-aliases", Action: "delete", Obj: audit}).Error
	})
	if err == nil {
		SetLastUpdate(time.Now().Unix())
	}
	return err
}
func (*LegacySubscriptionAliasService) Match(host, escapedPath, rawQuery string) (*model.LegacySubscriptionAlias, bool, error) {
	normalized, err := CanonicalLegacyHost(host)
	if err != nil {
		return nil, false, err
	}
	var count int64
	if err := database.GetDB().Model(&model.LegacySubscriptionHost{}).Where("host = ?", normalized).Count(&count).Error; err != nil {
		return nil, false, err
	}
	if count == 0 {
		return nil, false, nil
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil || !validLegacyQuery(query) {
		return nil, true, ErrLegacySubscriptionQuery
	}
	if escapedPath == "" {
		escapedPath = "/"
	}
	var entry model.LegacySubscriptionAlias
	err = database.GetDB().Where("lookup_key = ?", legacyLookupKey(normalized, escapedPath, query.Get("token"))).First(&entry).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, true, nil
	}
	return &entry, true, err
}
