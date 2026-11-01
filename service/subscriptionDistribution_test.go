package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"gorm.io/gorm"
)

const distributionPolicy = "proxy-groups:\n  - name: Policy\n    type: select\n    proxies: [__AUTO_INSERT_NODES__, DIRECT]\nrules: ['MATCH,Policy']\n"

func TestSubscriptionTemplatesAssignmentsRevisionAndRevocation(t *testing.T) {
	setupSnapshotDB(t)
	alice, bob := catalogClient(t, "alice", ""), catalogClient(t, "bob", "")
	var service SubscriptionTemplateService
	content := distributionPolicy
	aliceIds, bobIds := []uint{alice.Id}, []uint{bob.Id}
	first, err := service.Save(SubscriptionTemplateWrite{Name: "first", Format: "clash", Content: &content, Enabled: true, ClientIds: &aliceIds}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Save(SubscriptionTemplateWrite{Name: "second", Format: "clash", Content: &content, Enabled: true, ClientIds: &bobIds}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := service.Resolve(alice.Id, "clash", "")
	if err != nil || assigned.Id != first {
		t.Fatal("wrong policy", err)
	}
	// Moving Alice also changes the displaced template's revision, preventing
	// an old editor of that template from stealing Alice back without reload.
	both := []uint{alice.Id, bob.Id}
	_, err = service.Save(SubscriptionTemplateWrite{Id: second, Revision: 1, Name: "second", Format: "clash", Enabled: true, ClientIds: &both}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Save(SubscriptionTemplateWrite{Id: first, Revision: 1, Name: "stale", Format: "clash", Enabled: true, ClientIds: &aliceIds}, "admin")
	if !errors.Is(err, ErrSubscriptionRevision) {
		t.Fatal("stale overwrite", err)
	}
	if err := service.Delete(second, 2, "admin"); err == nil {
		t.Fatal("deleted assigned policy")
	}
	_, err = service.Save(SubscriptionTemplateWrite{Id: second, Revision: 2, Name: "second", Format: "clash", Enabled: false}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(alice.Id, "clash", ""); !errors.Is(err, ErrSubscriptionPolicyUnavailable) {
		t.Fatal("disabled template fell back", err)
	}
	if _, err := service.Resolve(bob.Id, "clash", first); err != nil {
		t.Fatal("alias template override", err)
	}
	metadata, err := service.List()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(metadata)
	if strings.Contains(string(data), "MATCH") {
		t.Fatal("list exposed template content")
	}
}
func TestSubscriptionTemplateRejectsCredentialInjection(t *testing.T) {
	invalid := []string{
		"proxies: [{name: stolen, type: trojan, password: secret}]\nrules: ['MATCH,DIRECT']",
		"proxy-providers: {hidden: {url: 'https://provider.example/sub'}}\nrules: ['MATCH,DIRECT']",
		"proxy-groups: [{name: Group, type: select, use: [hidden]}]\nrules: ['MATCH,Group']",
		"proxy-groups: [{name: Group, filter: '[', proxies: [DIRECT]}]\nrules: ['MATCH,Group']",
		"proxy-groups: [{name: Group, proxies: [unknown]}]\nrules: ['MATCH,Group']",
		"proxy-groups: [{name: Loop, proxies: [Loop]}]\nrules: ['MATCH,Loop']",
		"rules: ['MATCH,missing']", "rules: ['MATCH,DIRECT']\n---\nrules: []",
	}
	for _, content := range invalid {
		if ValidateSubscriptionTemplate("clash", content) == nil {
			t.Fatal("unsafe/invalid template accepted", content)
		}
	}
	for _, content := range []string{`{"outbounds":[]}`, `{"route":{}}`, `null`} {
		if ValidateSubscriptionTemplate("json", content) == nil {
			t.Fatal("invalid json policy accepted")
		}
	}
	if err := ValidateSubscriptionTemplate("clash", distributionPolicy); err != nil {
		t.Fatal(err)
	}
}
func TestLegacyAliasHashOnlyExactScopeAndClientDeletion(t *testing.T) {
	setupSnapshotDB(t)
	alice := catalogClient(t, "alice", "")
	var aliases LegacySubscriptionAliasService
	input := LegacySubscriptionAliasWrite{Name: "shared historical identity", URL: "https://legacy.example/sub/synthetic-token/shared", ClientId: alice.Id, Enabled: true, DefaultFormat: "auto"}
	id, err := aliases.Save(input, "admin")
	if err != nil {
		t.Fatal(err)
	}
	match, known, err := aliases.Match("LEGACY.EXAMPLE:443", "/sub/synthetic-token/shared", "")
	if err != nil || !known || match == nil || match.ClientId != alice.Id {
		t.Fatal("exact alias missing", err)
	}
	for _, path := range []string{"/sub/wrong-token/shared", "/alice", "/sub/synthetic-token/shared/"} {
		found, known, err := aliases.Match("legacy.example", path, "")
		if found != nil || !known || err != nil {
			t.Fatal("unknown alias fell through", path)
		}
	}
	rows, _ := aliases.List()
	data, _ := json.Marshal(rows)
	if strings.Contains(string(data), "synthetic-token") || strings.Contains(string(data), "lookupKey") {
		t.Fatal("bearer leaked")
	}
	input.Id, input.Revision, input.URL = id, 1, ""
	if _, err = aliases.Save(input, "admin"); err != nil {
		t.Fatal("blank edit lost mapping", err)
	}
	if _, err = aliases.Save(input, "admin"); !errors.Is(err, ErrSubscriptionRevision) {
		t.Fatal("stale alias saved", err)
	}
	var clients ClientService
	for _, action := range []string{"delbulk"} {
		body, _ := json.Marshal([]uint{alice.Id})
		err := database.GetDB().Transaction(func(tx *gorm.DB) error { _, err := clients.Save(tx, action, body, ""); return err })
		if err != nil {
			t.Fatal(err)
		}
	}
	match, known, err = aliases.Match("legacy.example", "/sub/synthetic-token/shared", "")
	if err != nil || !known || match == nil || match.Enabled || match.ClientId != 0 || match.Revision != 3 {
		t.Fatal("deleted client alias not revoked", err, match)
	}
	if err := aliases.Delete(id, 3, "admin"); err != nil {
		t.Fatal(err)
	}
	match, known, err = aliases.Match("legacy.example", "/alice", "")
	if err != nil || !known || match != nil {
		t.Fatal("deleted host reservation missing", err)
	}
}
func TestLegacyAliasQueryTokenAndDedicatedHost(t *testing.T) {
	setupSnapshotDB(t)
	alice := catalogClient(t, "alice", "")
	var aliases LegacySubscriptionAliasService
	_, err := aliases.Save(LegacySubscriptionAliasWrite{Name: "query", URL: "https://legacy.example/?token=synthetic-query", ClientId: alice.Id, Enabled: true, DefaultFormat: "auto", Expiry: time.Now().Unix() + 3600}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	match, _, err := aliases.Match("legacy.example", "/", "token=synthetic-query&clash")
	if err != nil || match == nil {
		t.Fatal("format changed identity", err)
	}
	match, known, err := aliases.Match("legacy.example", "/", "token=wrong")
	if err != nil || !known || match != nil {
		t.Fatal("wrong query token authorized")
	}
	for _, raw := range []string{"http://legacy.example/token", "https://legacy.example/", "https://legacy.example/../token", "https://legacy.example/token?token=a&token=b", "https://user:pass@legacy.example/token"} {
		if _, _, err := legacyURLIdentity(raw); err == nil {
			t.Fatal("unsafe alias accepted", raw)
		}
	}
	database.GetDB().Create(&model.Setting{Key: "subDomain", Value: "current.example"})
	_, err = aliases.Save(LegacySubscriptionAliasWrite{Name: "collision", URL: "https://current.example/token", ClientId: alice.Id, Enabled: true, DefaultFormat: "auto"}, "admin")
	if err == nil {
		t.Fatal("native host captured")
	}
}

func TestSourceNodePrefixExportOnlyAndGrantScoped(t *testing.T) {
	setupSnapshotDB(t)
	alice, bob := catalogClient(t, "alice", ""), catalogClient(t, "bob", "")
	var catalog ExternalSubSourceService
	prefix := "Backup - "
	ids := []uint{alice.Id}
	id, err := catalog.Save(ExternalSubSourceWrite{Name: "provider", URL: snapshotTestURL, Enabled: true, NodePrefix: &prefix, ClientIds: &ids}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	row := model.ExternalSubSnapshot{Key: externalSubKey(snapshotTestURL), Payload: snapshotTestBody, LastSuccess: time.Now().Unix()}
	if err := database.GetDB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	body, err := catalog.Resolve(id, alice.Id)
	if err != nil || !strings.Contains(body, "Backup%20-%20test-node") {
		t.Fatal("prefix missing", body, err)
	}
	if _, err := catalog.Resolve(id, bob.Id); !errors.Is(err, ErrExternalSubCacheUnavailable) {
		t.Fatal("prefix bypassed grant")
	}
	stored := snapshotRow(t, row.Key)
	if stored.Payload != snapshotTestBody {
		t.Fatal("prefix changed stored snapshot")
	}
	// An omitted prefix on an older admin's write must preserve it.
	if _, err = catalog.Save(ExternalSubSourceWrite{Id: id, Revision: 1, Name: "renamed", Enabled: true}, "admin"); err != nil {
		t.Fatal(err)
	}
	if source := catalogSource(t, id); source.NodePrefix != prefix {
		t.Fatal("omitted prefix cleared setting")
	}
}
