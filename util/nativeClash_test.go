package util

import "testing"

const nativeExample = `proxies:
  - name: edge
    type: ss
    server: alias.example
    port: 443
    cipher: aes-128-gcm
    password: synthetic-password
    plugin: shadow-tls
    plugin-opts:
      host: tls.example
      password: synthetic-plugin-password
      version: 3
  - name: landing
    type: trojan
    server: landing.example
    port: 443
    password: synthetic-password
    dialer-proxy: outbound-group
proxy-groups:
  - name: outbound-group
    type: select
    proxies: [edge, DIRECT]
  - name: unused
    type: select
    use: [provider-we-must-not-fetch]
hosts:
  alias.example: real.example
  real.example: [192.0.2.11, 192.0.2.12]
rules: [MATCH,provider-policy]
`

func TestNativeClashPreservesProxyFieldsAndOnlyDependencyGroups(t *testing.T) {
	doc, err := ParseNativeClash(nativeExample)
	if err != nil || len(doc.Proxies) != 2 || len(doc.Groups) != 1 || len(doc.Hosts) != 2 {
		t.Fatalf("native parse: %+v %v", doc, err)
	}
	if doc.Proxies[0]["plugin"] != "shadow-tls" || doc.Proxies[0]["plugin-opts"].(map[string]interface{})["version"] != 3 {
		t.Fatal("plugin fields lost")
	}
	bounded := `defaults: &node {type: trojan, server: node.example, port: 443, password: synthetic}
proxies: [{<<: *node, name: one}, {<<: *node, name: two}]`
	if doc, err := ParseNativeClash(bounded); err != nil || len(doc.Proxies) != 2 {
		t.Fatal("bounded YAML anchors rejected", err)
	}
}
func TestNativeClashRejectsBrokenDependenciesAndDocuments(t *testing.T) {
	bodies := []string{"<html>login</html>", "proxies: []", `proxies: [{name: x, type: trojan, server: node.example, port: 443, password: .nan}]`, `proxies: [{name: x, type: trojan, server: a.example, port: 443}]
hosts: {"*.example": b.example}`, `proxies: [{name: x, type: trojan, server: node.example, port: 443, dialer-proxy: group}]
proxy-groups: [{name: group, type: select, proxies: [DIRECT], include-all: true}]`, `proxies: [{name: x, type: ss, server: node.example, port: 0}]`,
		`proxies: [{name: x, type: trojan, server: node.example, port: 443, dialer-proxy: missing}]`,
		`proxies: [{name: x, type: trojan, server: node.example, port: 443, dialer-proxy: x}]`,
		`proxies: [{name: x, type: trojan, server: node.example, port: 443, dialer-proxy: group}]
proxy-groups: [{name: group, type: select, use: [remote]}]`,
		`proxies: [{name: x, type: trojan, server: node.example, port: 443}]
hosts: {node.example: alias.example, alias.example: node.example}`,
		"proxies: [{name: x, type: trojan, server: node.example, port: 443}]\n---\nproxies: []",
		`proxies: [{name: x, type: trojan, server: node.example, port: 443}, {name: x, type: trojan, server: node.example, port: 443}]`}
	for _, body := range bodies {
		if _, err := ParseNativeClash(body); err == nil {
			t.Errorf("invalid native source accepted: %.100s", body)
		}
	}
}
