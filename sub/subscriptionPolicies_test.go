package sub

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/service"
	"gopkg.in/yaml.v3"
)

const markerPolicy = `mixed-port: 7890
proxy-groups:
  - {name: Main, type: select, proxies: [Backup, __AUTO_INSERT_NODES__, DIRECT], filter: 自建}
  - {name: Backup, type: select, proxies: [__AUTO_INSERT_NODES__], filter: 备用}
  - {name: Empty, type: select, proxies: [__AUTO_INSERT_NODES__], filter: missing}
  - {name: AlsoEmpty, type: select, proxies: [Empty]}
rules:
  - DOMAIN,example.com,Empty
  - IP-CIDR,192.0.2.0/24,AlsoEmpty,no-resolve
  - MATCH,Main
`

func policyClient(t *testing.T, name, node string) model.Client {
	t.Helper()
	links, _ := json.Marshal([]map[string]string{{"type": "external", "uri": "trojan://synthetic-password@node.example:443#" + node}})
	client := model.Client{Name: name, Enable: true, Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`), Links: links}
	if err := database.GetDB().Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	return client
}
func assignPolicy(t *testing.T, client model.Client, format, content string) string {
	t.Helper()
	var policies service.SubscriptionTemplateService
	ids := []uint{client.Id}
	id, err := policies.Save(service.SubscriptionTemplateWrite{Name: client.Name + format, Format: format, Content: &content, Enabled: true, ClientIds: &ids}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func TestTemplateMarkerFiltersFallbacksAndEmptyGroups(t *testing.T) {
	setupSubDB(t)
	client := policyClient(t, "alice", "自建-one")
	assignPolicy(t, client, "clash", markerPolicy)
	var exporter ClashService
	result, _, err := exporter.GetClash(client.Name)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]interface{}
	if err := yaml.Unmarshal([]byte(*result), &output); err != nil {
		t.Fatal(err)
	}
	groups := proxyGroupList(output["proxy-groups"])
	if len(groups) != 1 || groups[0]["name"] != "Main" || strings.Contains(*result, autoInsertNodes) || strings.Contains(*result, "filter:") {
		t.Fatal("placeholders or empty categories survived", *result)
	}
	names := proxyNames(groups[0]["proxies"])
	if len(names) != 2 || names[0] != "自建-one" || names[1] != "DIRECT" {
		t.Fatal("fixed member or dynamic node lost", names)
	}
	if strings.Contains(*result, ",Empty") || strings.Contains(*result, ",AlsoEmpty") || !strings.Contains(*result, "192.0.2.0/24,Main,no-resolve") {
		t.Fatal("rule target not repaired", *result)
	}
	// An explicitly disabled policy fails refresh instead of using a global one.
	database.GetDB().Model(&model.SubscriptionTemplate{}).Where("id = ?", mustPolicyId(t, client.Id, "clash")).Update("enabled", false)
	if _, _, err := exporter.GetClash(client.Name); err != service.ErrSubscriptionPolicyUnavailable {
		t.Fatal("disabled policy did not fail closed", err)
	}
}
func mustPolicyId(t *testing.T, client uint, format string) string {
	t.Helper()
	var row model.ClientSubscriptionTemplate
	if err := database.GetDB().Where("client_id = ? AND format = ?", client, format).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.TemplateId
}
func TestPerClientPoliciesDoNotBleedAcrossConcurrentExports(t *testing.T) {
	setupSubDB(t)
	alice, bob := policyClient(t, "alice", "alice-node"), policyClient(t, "bob", "bob-node")
	for _, client := range []model.Client{alice, bob} {
		assignPolicy(t, client, "clash", "proxy-groups: [{name: "+client.Name+", type: select, proxies: [__AUTO_INSERT_NODES__]}]\nrules: ['MATCH,"+client.Name+"']")
		assignPolicy(t, client, "json", `{"final":"proxy","rules":[{"domain":["`+client.Name+`.example"],"outbound":"direct"}]}`)
	}
	var clash ClashService
	var sb JsonService
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		for _, client := range []model.Client{alice, bob} {
			wg.Add(1)
			go func(client model.Client) {
				defer wg.Done()
				other := "alice"
				if client.Name == "alice" {
					other = "bob"
				}
				result, _, err := clash.GetClash(client.Name)
				if err != nil || result == nil || !strings.Contains(*result, "MATCH,"+client.Name) || strings.Contains(*result, other+"-node") {
					t.Errorf("Clash policy isolation: %v", err)
				}
				result, _, err = sb.GetJson(client.Name, "json")
				if err != nil || result == nil || !strings.Contains(*result, client.Name+".example") || strings.Contains(*result, other+".example") {
					t.Errorf("JSON policy isolation: %v", err)
				}
			}(client)
		}
	}
	wg.Wait()
}
func TestLegacyAliasesThroughRealRouter(t *testing.T) {
	setupSubDB(t)
	client := policyClient(t, "alice", "own-node")
	// A native domain check is configured, proving aliases run before it.
	for _, row := range []*model.Setting{{Key: "subDomain", Value: "native.example"}, {Key: "subEncode", Value: "false"}} {
		if err := database.GetDB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	assignPolicy(t, client, "clash", "proxy-groups: [{name: Main, type: select, proxies: [__AUTO_INSERT_NODES__]}]\nrules: ['MATCH,Main']")
	var aliases service.LegacySubscriptionAliasService
	id, err := aliases.Save(service.LegacySubscriptionAliasWrite{Name: "historical shared identity", URL: "https://legacy.example/profiles/self", ClientId: client.Id, Enabled: true, DefaultFormat: "auto"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewServer().initRouter()
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method, host, path, agent string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://"+host+path, nil)
		req.Header.Set("User-Agent", agent)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Header().Get("Location") != "" {
			t.Fatal("legacy URL redirected")
		}
		return w
	}
	for _, agent := range []string{"Clash-Verge/2.4", "mihomo", "OpenClash", "Stash"} {
		w := serve("GET", "legacy.example", "/profiles/self", agent)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "MATCH,Main") {
			t.Fatal("Clash legacy format failed", agent, w.Code, w.Body.String())
		}
	}
	w := serve("GET", "legacy.example", "/profiles/self", "Shadowrocket")
	decoded, err := base64.StdEncoding.DecodeString(w.Body.String())
	if w.Code != 200 || err != nil || !strings.Contains(string(decoded), "own-node") {
		t.Fatal("Shadowrocket not base64", w.Code, err)
	}
	w = serve("GET", "legacy.example", "/profiles/self?target=clash", "Shadowrocket")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "proxy-groups:") {
		t.Fatal("target did not override UA")
	}
	w = serve("GET", "legacy.example", "/profiles/self?plain", "Clash")
	if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "trojan://") {
		t.Fatal("plain explicit format")
	}
	for _, path := range []string{"/profiles/wrong", "/sub/alice", "/sub/alice/", "/profiles/self/"} {
		if w := serve("GET", "legacy.example", path, "Clash"); w.Code != 404 {
			t.Fatal("alias fell through", path, w.Code)
		}
	}
	for _, path := range []string{"/profiles/self?target=surge", "/profiles/self?format=info", "/profiles/self?token=x&token=y"} {
		if w := serve("GET", "legacy.example", path, "Clash"); w.Code != 400 {
			t.Fatal("unsupported format accepted", path, w.Code)
		}
	}
	if w := serve("POST", "legacy.example", "/profiles/self", "Clash"); w.Code != 405 {
		t.Fatal("POST accepted")
	}
	if w := serve("HEAD", "legacy.example", "/profiles/self", "Clash"); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Profile-Title") == "" {
		t.Fatal("HEAD wrong")
	}
	// Native bare URLs keep their old generic representation regardless of UA.
	if w := serve("GET", "native.example", "/sub/alice", "Clash"); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "trojan://") {
		t.Fatal("native URL behavior changed", w.Code)
	}
	for _, update := range []map[string]interface{}{{"enable": false}, {"enable": true, "expiry": time.Now().Unix() - 1}} {
		database.GetDB().Model(&model.Client{}).Where("id = ?", client.Id).Updates(update)
		w := serve("GET", "legacy.example", "/profiles/self", "Clash")
		if w.Code != 403 || strings.Contains(w.Body.String(), "synthetic-password") {
			t.Fatal("inactive client leaked")
		}
	}
	database.GetDB().Model(&model.Client{}).Where("id = ?", client.Id).Update("expiry", 0)
	database.GetDB().Model(&model.LegacySubscriptionAlias{}).Where("id = ?", id).Update("expiry", time.Now().Unix()-1)
	if w := serve("GET", "legacy.example", "/profiles/self", "Clash"); w.Code != 403 {
		t.Fatal("expired alias accepted")
	}
	if err := aliases.Delete(id, 1, "admin"); err != nil {
		t.Fatal(err)
	}
	if w := serve("GET", "legacy.example", "/sub/alice", "Clash"); w.Code != 404 {
		t.Fatal("deleted host fell through")
	}
}

// Optional private input is never embedded in a fixture or committed. Useful
// when verifying a user's actual template against synthetic authorized nodes.
func TestPrivateSubscriptionTemplateDryRun(t *testing.T) {
	path := os.Getenv("SUBSCRIPTION_TEMPLATE_TEST_FILE")
	if path == "" {
		t.Skip("optional private template")
	}
	setupSubDB(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	client := policyClient(t, "private-fixture", "自建-synthetic")
	assignPolicy(t, client, "clash", string(content))
	var exporter ClashService
	result, _, err := exporter.GetClash(client.Name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*result, autoInsertNodes) {
		t.Fatal("unresolved marker")
	}
	binary := os.Getenv("MIHOMO_TEST_BINARY")
	if binary == "" {
		return
	}
	dir := t.TempDir()
	if geodata := os.Getenv("MIHOMO_TEST_GEODATA_DIR"); geodata != "" {
		for _, name := range []string{"geoip.dat", "geosite.dat", "Country.mmdb"} {
			data, err := os.ReadFile(filepath.Join(geodata, name))
			if err == nil {
				if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	file := filepath.Join(dir, "config.yaml")
	if err = os.WriteFile(file, []byte(*result), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(binary, "-t", "-d", dir, "-f", file).CombinedOutput()
	if err != nil {
		t.Fatalf("private template parse: %v %s", err, output)
	}
}
