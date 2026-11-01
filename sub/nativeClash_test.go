package sub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shenaba/2s-ui/util"
	"gopkg.in/yaml.v3"
)

const nativeSubFixture = `proxies:
  - name: edge
    type: ss
    server: alias.example
    port: 443
    cipher: aes-128-gcm
    password: synthetic-password
    plugin: shadow-tls
    client-fingerprint: chrome
    plugin-opts: {host: tls.example, password: synthetic-plugin-password, version: 3}
  - name: landing
    type: trojan
    server: alias.example
    port: 443
    password: synthetic-password
    dialer-proxy: provider-group
proxy-groups:
  - name: provider-group
    type: select
    proxies: [edge, DIRECT]
hosts:
  alias.example: endpoint.example
  endpoint.example: [192.0.2.11, 192.0.2.12]
  unrelated.example: 192.0.2.99
rules: [MATCH,upstream-should-not-win]
dns: {nameserver: [https://upstream.example/dns-query]}
`

func buildNativeFixture(t *testing.T) string {
	t.Helper()
	doc, err := util.ParseNativeClash(nativeSubFixture)
	if err != nil {
		t.Fatal(err)
	}
	doc.Namespace = "first-provider"
	second, err := util.ParseNativeClash(strings.ReplaceAll(nativeSubFixture, "192.0.2.11", "192.0.2.21"))
	if err != nil {
		t.Fatal(err)
	}
	second.Namespace = "second-provider"
	outbounds := []map[string]interface{}{{"type": "trojan", "tag": "edge", "server": "self.example", "server_port": 443, "password": "self-password"}}
	var service ClashService
	result, err := service.ConvertToClashMeta(&outbounds, "dns: {enable: true, nameserver: [1.1.1.1]}\nrules: ['MATCH,Proxy']\n", *doc, *second)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestNativeClashMergePreservesPluginsHostsAndReferences(t *testing.T) {
	setupSubDB(t)
	result := buildNativeFixture(t)
	if strings.Contains(result, "upstream-should-not-win") || strings.Contains(result, "unrelated.example") || strings.Contains(result, "upstream.example/dns-query") {
		t.Fatal("source global configuration leaked into template")
	}
	var output map[string]interface{}
	if err := yaml.Unmarshal([]byte(result), &output); err != nil {
		t.Fatal(err)
	}
	proxies := output["proxies"].([]interface{})
	if len(proxies) != 5 {
		t.Fatalf("got %d proxies", len(proxies))
	}
	names := map[string]bool{}
	for _, entry := range proxies {
		name := entry.(map[string]interface{})["name"].(string)
		if names[name] {
			t.Fatal("duplicate name", name)
		}
		names[name] = true
	}
	first := proxies[1].(map[string]interface{})
	other := proxies[3].(map[string]interface{})
	if first["plugin"] != "shadow-tls" || first["plugin-opts"].(map[string]interface{})["host"] != "tls.example" {
		t.Fatal("native plugin changed")
	}
	if first["server"] == other["server"] {
		t.Fatal("conflicting provider hosts shared a global alias")
	}
	hosts := output["hosts"].(map[string]interface{})
	if len(hosts) != 2 || hosts[first["server"].(string)] == nil || hosts[other["server"].(string)] == nil {
		t.Fatal("required endpoint address lists lost")
	}
	landing := proxies[2].(map[string]interface{})
	if landing["sni"] != "alias.example" {
		t.Fatal("dial endpoint change lost TLS identity")
	}
	groups := proxyGroupList(output["proxy-groups"])
	found := false
	for _, group := range groups {
		if group["name"] == landing["dialer-proxy"] {
			found = true
			members := util.AsStringList(group["proxies"])
			if len(members) != 2 || members[0] != first["name"] {
				t.Fatal("dependency group references not rewritten", group)
			}
		}
	}
	if !found {
		t.Fatal("native dependency group missing")
	}
	// An explicit administrator endpoint override takes precedence.
	doc, _ := util.ParseNativeClash(nativeSubFixture)
	empty := []map[string]interface{}{}
	var service ClashService
	overridden, err := service.ConvertToClashMeta(&empty, "hosts: {alias.example: 192.0.2.200}\nrules: ['MATCH,DIRECT']\n", *doc)
	if err != nil || strings.Contains(overridden, "sui-") {
		t.Fatal("admin host override was replaced", err)
	}
}
func TestNativeClashMihomoValidation(t *testing.T) {
	binary := os.Getenv("MIHOMO_TEST_BINARY")
	if binary == "" {
		t.Skip("optional installed mihomo config validator")
	}
	setupSubDB(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte(buildNativeFixture(t)), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(binary, "-t", "-d", directory, "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("mihomo rejected native merge: %v\n%s", err, output)
	}
}

func TestNativeClashSnapshotIsUsedByAuthorizedExport(t *testing.T) {
	setupSubDB(t)
	raw := "https://provider.example/sub?token=synthetic"
	sum := sha256.Sum256([]byte(raw))
	key := hex.EncodeToString(sum[:])
	client := model.Client{Name: "native-user", Enable: true, Config: json.RawMessage(`{}`), Inbounds: json.RawMessage(`[]`), Links: json.RawMessage(`[]`)}
	if err := database.GetDB().Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []interface{}{&model.ExternalSubSource{Id: "native-source", Name: "provider", URL: raw, Enabled: true, Revision: 1}, &model.ExternalSubGrant{SourceId: "native-source", ClientId: client.Id}, &model.ExternalSubSnapshot{Key: key, Payload: "trojan://universal-password@node.example:443#universal-node", LastSuccess: time.Now().Unix(), ClashPayload: nativeSubFixture, ClashLastSuccess: time.Now().Unix()}} {
		if err := database.GetDB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	var exporter ClashService
	result, _, err := exporter.GetClash(client.Name)
	if err != nil || !strings.Contains(*result, "shadow-tls") || strings.Contains(*result, "universal-node") {
		t.Fatal("Clash export did not choose native snapshot", err)
	}
	database.GetDB().Where("source_id = ? AND client_id = ?", "native-source", client.Id).Delete(&model.ExternalSubGrant{})
	result, _, err = exporter.GetClash(client.Name)
	if err != nil || strings.Contains(*result, "shadow-tls") {
		t.Fatal("native snapshot escaped revoked grant", err)
	}
}
