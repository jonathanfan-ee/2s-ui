package service

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/util"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
	ormlog "gorm.io/gorm/logger"
)

var ErrSubscriptionPolicyUnavailable = errors.New("subscription-policy-unavailable")
var ErrSubscriptionRevision = errors.New("subscription-entry-changed-reload-before-saving")

const maxSubscriptionTemplateBytes = 2 << 20

type SubscriptionRenderOptions struct {
	TemplateId string
	Encode     *bool
}
type SubscriptionTemplateWrite struct {
	Id        string  `json:"id"`
	Name      string  `json:"name"`
	Format    string  `json:"format"`
	Content   *string `json:"content"`
	Enabled   bool    `json:"enabled"`
	Revision  uint64  `json:"revision"`
	ClientIds *[]uint `json:"clientIds"`
}
type SubscriptionTemplateView struct {
	Id        string `json:"id"`
	Name      string `json:"name"`
	Format    string `json:"format"`
	Enabled   bool   `json:"enabled"`
	Revision  uint64 `json:"revision"`
	ClientIds []uint `json:"clientIds"`
}
type SubscriptionTemplateService struct{}

func validSubscriptionLabel(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.RuneCountInString(name) <= 128 && !strings.ContainsFunc(name, unicode.IsControl)
}

// ValidateSubscriptionTemplate accepts full Clash policies and the existing
// sing-box JSON extension shape. Dynamic proxies always come from client grants.
func ValidateSubscriptionTemplate(format, content string) error {
	if content == "" || len(content) > maxSubscriptionTemplateBytes {
		return errors.New("template-empty-or-too-large")
	}
	if format == "json" {
		var value map[string]interface{}
		if json.Unmarshal([]byte(content), &value) != nil || value == nil {
			return errors.New("template-json-invalid")
		}
		if _, ok := value["outbounds"]; ok {
			return errors.New("template-static-proxies-not-allowed")
		}
		if _, ok := value["route"]; ok {
			return errors.New("use-singbox-extension-rules-shape")
		}
		return nil
	}
	if format != "clash" {
		return errors.New("template-format-must-be-clash-or-json")
	}
	var node yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(content))
	if dec.Decode(&node) != nil {
		return errors.New("template-yaml-invalid")
	}
	var extra yaml.Node
	if dec.Decode(&extra) != io.EOF {
		return errors.New("template-must-have-one-document")
	}
	budget := 100000
	var guard func(*yaml.Node, int) bool
	guard = func(n *yaml.Node, depth int) bool {
		budget--
		if depth > 64 || budget < 0 {
			return false
		}
		if n.Kind == yaml.AliasNode {
			return n.Alias != nil && guard(n.Alias, depth+1)
		}
		for _, c := range n.Content {
			if !guard(c, depth+1) {
				return false
			}
		}
		return true
	}
	if !guard(&node, 0) {
		return errors.New("template-yaml-too-complex")
	}
	var value map[string]interface{}
	if node.Decode(&value) != nil || value == nil {
		return errors.New("template-yaml-must-be-mapping")
	}
	if _, err := json.Marshal(value); err != nil {
		return errors.New("template-yaml-invalid-values")
	}
	if proxies, ok := value["proxies"]; ok {
		if list, ok := proxies.([]interface{}); !ok || len(list) != 0 {
			return errors.New("template-static-proxies-not-allowed")
		}
	}
	if providers, ok := value["proxy-providers"]; ok {
		if entries, ok := providers.(map[string]interface{}); !ok || len(entries) != 0 {
			return errors.New("template-remote-proxy-providers-not-allowed")
		}
	}
	groups := map[string]bool{}
	if raw, ok := value["proxy-groups"]; ok {
		list, ok := raw.([]interface{})
		if !ok {
			return errors.New("template-proxy-groups-invalid")
		}
		for _, entry := range list {
			group, ok := entry.(map[string]interface{})
			if !ok {
				return errors.New("template-proxy-group-invalid")
			}
			name, _ := group["name"].(string)
			if !validSubscriptionLabel(name) || strings.EqualFold(name, "all") || name == "__AUTO_INSERT_NODES__" || groups[name] || util.ClashBuiltins[name] {
				return errors.New("template-proxy-group-name-invalid")
			}
			groups[name] = true
			if filter, ok := group["filter"].(string); ok {
				if _, err := regexp.Compile(filter); err != nil {
					return errors.New("template-group-filter-invalid")
				}
			}
			if len(util.AsStringList(group["use"])) > 0 {
				return errors.New("template-remote-proxy-providers-not-allowed")
			}
		}
		for _, entry := range list {
			group := entry.(map[string]interface{})
			for _, member := range util.AsStringList(group["proxies"]) {
				if member != "__AUTO_INSERT_NODES__" && !strings.EqualFold(member, "all") && !groups[member] && !util.ClashBuiltins[member] {
					return errors.New("template-group-reference-invalid")
				}
			}
		}
		// Reject cycles before a 200 export can hand a client a broken policy.
		byName := map[string]map[string]interface{}{}
		for _, entry := range list {
			group := entry.(map[string]interface{})
			byName[group["name"].(string)] = group
		}
		state := map[string]int{}
		var visit func(string) bool
		visit = func(name string) bool {
			if state[name] == 1 {
				return false
			}
			if state[name] == 2 {
				return true
			}
			state[name] = 1
			for _, member := range util.AsStringList(byName[name]["proxies"]) {
				if byName[member] != nil && !visit(member) {
					return false
				}
			}
			state[name] = 2
			return true
		}
		for name := range byName {
			if !visit(name) {
				return errors.New("template-group-cycle")
			}
		}
	}
	rules, ok := value["rules"].([]interface{})
	if !ok || len(rules) == 0 {
		return errors.New("template-rules-required")
	}
	for _, raw := range rules {
		rule, ok := raw.(string)
		if !ok {
			return errors.New("template-rule-invalid")
		}
		target := clashRuleTarget(rule)
		if !groups[target] && !util.ClashBuiltins[target] {
			return errors.New("template-rule-target-invalid")
		}
	}
	return nil
}
func clashRuleTarget(rule string) string {
	parts := strings.Split(rule, ",")
	if len(parts) < 2 {
		return ""
	}
	last := len(parts) - 1
	if strings.EqualFold(strings.TrimSpace(parts[last]), "no-resolve") {
		last--
	}
	return strings.TrimSpace(parts[last])
}

func (*SubscriptionTemplateService) List() ([]SubscriptionTemplateView, error) {
	var templates []model.SubscriptionTemplate
	if err := database.GetDB().Select("id, name, format, enabled, revision").Order("name,id").Find(&templates).Error; err != nil {
		return nil, err
	}
	var assignments []model.ClientSubscriptionTemplate
	if err := database.GetDB().Order("client_id").Find(&assignments).Error; err != nil {
		return nil, err
	}
	ids := map[string][]uint{}
	for _, entry := range assignments {
		ids[entry.TemplateId] = append(ids[entry.TemplateId], entry.ClientId)
	}
	result := make([]SubscriptionTemplateView, 0, len(templates))
	for _, entry := range templates {
		clients := ids[entry.Id]
		if clients == nil {
			clients = []uint{}
		}
		result = append(result, SubscriptionTemplateView{Id: entry.Id, Name: entry.Name, Format: entry.Format, Enabled: entry.Enabled, Revision: entry.Revision, ClientIds: clients})
	}
	return result, nil
}
func (*SubscriptionTemplateService) Get(id string) (*model.SubscriptionTemplate, error) {
	if !validExternalSourceId(id) {
		return nil, errors.New("template-id-invalid")
	}
	var entry model.SubscriptionTemplate
	err := database.GetDB().Where("id = ?", id).First(&entry).Error
	return &entry, err
}
func (*SubscriptionTemplateService) Save(input SubscriptionTemplateWrite, actor string) (string, error) {
	if !validSubscriptionLabel(input.Name) {
		return "", errors.New("template-name-invalid")
	}
	id := input.Id
	if id != "" && !validExternalSourceId(id) {
		return "", errors.New("template-id-invalid")
	}
	err := database.GetDB().Session(&gorm.Session{Logger: ormlog.Discard}).Transaction(func(tx *gorm.DB) error {
		var entry model.SubscriptionTemplate
		if id == "" {
			id = rand.Text()
			entry.Id = id
		} else {
			if err := tx.Where("id = ?", id).First(&entry).Error; err != nil {
				return err
			}
			if entry.Revision != input.Revision {
				return ErrSubscriptionRevision
			}
			if entry.Format != input.Format {
				return errors.New("template-format-cannot-change")
			}
		}
		entry.Name = strings.TrimSpace(input.Name)
		entry.Format = input.Format
		entry.Enabled = input.Enabled
		entry.Revision++
		if input.Content != nil {
			entry.Content = *input.Content
		}
		if err := ValidateSubscriptionTemplate(entry.Format, entry.Content); err != nil {
			return err
		}
		if err := tx.Save(&entry).Error; err != nil {
			return err
		}
		if input.ClientIds != nil {
			ids, err := validatedSubscriptionClientIds(tx, *input.ClientIds)
			if err != nil {
				return err
			}
			if err := tx.Where("template_id = ?", id).Delete(&model.ClientSubscriptionTemplate{}).Error; err != nil {
				return err
			}
			if len(ids) > 0 {
				var displaced []string
				if err := tx.Model(&model.ClientSubscriptionTemplate{}).Where("client_id in ? AND format = ? AND template_id <> ?", ids, entry.Format, id).Distinct("template_id").Pluck("template_id", &displaced).Error; err != nil {
					return err
				}
				if len(displaced) > 0 {
					if err := tx.Model(&model.SubscriptionTemplate{}).Where("id in ?", displaced).UpdateColumn("revision", gorm.Expr("revision + 1")).Error; err != nil {
						return err
					}
				}
				if err := tx.Where("client_id in ? AND format = ?", ids, entry.Format).Delete(&model.ClientSubscriptionTemplate{}).Error; err != nil {
					return err
				}
				rows := make([]model.ClientSubscriptionTemplate, 0, len(ids))
				for _, clientId := range ids {
					rows = append(rows, model.ClientSubscriptionTemplate{ClientId: clientId, Format: entry.Format, TemplateId: id})
				}
				if err := tx.Create(&rows).Error; err != nil {
					return err
				}
			}
		}
		audit, _ := json.Marshal(map[string]interface{}{"id": id, "format": entry.Format, "revision": entry.Revision, "enabled": entry.Enabled, "clientIds": input.ClientIds})
		return tx.Create(&model.Changes{DateTime: time.Now().Unix(), Actor: actor, Key: "subscription-templates", Action: "save", Obj: audit}).Error
	})
	if err != nil {
		return "", err
	}
	SetLastUpdate(time.Now().Unix())
	return id, nil
}
func validatedSubscriptionClientIds(tx *gorm.DB, ids []uint) ([]uint, error) {
	result := slices.Clone(ids)
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) > 1000 || len(result) > 0 && result[0] == 0 {
		return nil, errors.New("invalid-client-selection")
	}
	if len(result) > 0 {
		var count int64
		if err := tx.Model(&model.Client{}).Where("id in ?", result).Count(&count).Error; err != nil {
			return nil, err
		}
		if count != int64(len(result)) {
			return nil, errors.New("client-not-found")
		}
	}
	return result, nil
}
func (*SubscriptionTemplateService) Delete(id string, revision uint64, actor string) error {
	if !validExternalSourceId(id) {
		return errors.New("template-id-invalid")
	}
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		var entry model.SubscriptionTemplate
		if err := tx.Where("id = ?", id).First(&entry).Error; err != nil {
			return err
		}
		if entry.Revision != revision {
			return ErrSubscriptionRevision
		}
		var refs int64
		if err := tx.Model(&model.ClientSubscriptionTemplate{}).Where("template_id = ?", id).Count(&refs).Error; err != nil {
			return err
		}
		if refs > 0 {
			return errors.New("template-still-assigned")
		}
		if err := tx.Model(&model.LegacySubscriptionAlias{}).Where("clash_template_id = ? OR json_template_id = ?", id, id).Count(&refs).Error; err != nil {
			return err
		}
		if refs > 0 {
			return errors.New("template-still-used-by-legacy-alias")
		}
		if err := tx.Where("id = ?", id).Delete(&model.SubscriptionTemplate{}).Error; err != nil {
			return err
		}
		audit, _ := json.Marshal(map[string]interface{}{"id": id})
		return tx.Create(&model.Changes{DateTime: time.Now().Unix(), Actor: actor, Key: "subscription-templates", Action: "delete", Obj: audit}).Error
	})
	if err == nil {
		SetLastUpdate(time.Now().Unix())
	}
	return err
}
func (*SubscriptionTemplateService) Resolve(clientId uint, format, override string) (*model.SubscriptionTemplate, error) {
	id := override
	if id == "" {
		var assignment model.ClientSubscriptionTemplate
		err := database.GetDB().Where("client_id = ? AND format = ?", clientId, format).First(&assignment).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, ErrSubscriptionPolicyUnavailable
		}
		id = assignment.TemplateId
	}
	var entry model.SubscriptionTemplate
	if err := database.GetDB().Where("id = ? AND format = ? AND enabled = ?", id, format, true).First(&entry).Error; err != nil {
		return nil, ErrSubscriptionPolicyUnavailable
	}
	if ValidateSubscriptionTemplate(format, entry.Content) != nil {
		return nil, ErrSubscriptionPolicyUnavailable
	}
	return &entry, nil
}
