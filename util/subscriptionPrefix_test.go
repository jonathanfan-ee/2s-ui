package util

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestSubscriptionNodePrefixPreservesCredentialsAndDependencies(t *testing.T) {
	original := "trojan://p%40ss@node.example:443?security=tls&sni=tls.example#old%20name"
	prefixed, err := PrefixSubscriptionNodes(original, "备用 - ")
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := strings.Cut(original, "#")
	after, fragment, _ := strings.Cut(prefixed, "#")
	name, _ := url.PathUnescape(fragment)
	if before != after || name != "备用 - old name" {
		t.Fatal("URI connection fields changed", prefixed)
	}
	vmess := `{"ps":"old","id":"synthetic-id","add":"node.example","port":"443"}`
	prefixed, err = PrefixSubscriptionNodes("vmess://"+ByteToB64Str([]byte(vmess)), "备用 - ")
	if err != nil {
		t.Fatal(err)
	}
	data, err := B64StrToByte(strings.TrimPrefix(prefixed, "vmess://"))
	if err != nil {
		t.Fatal(err)
	}
	var vm map[string]interface{}
	json.Unmarshal(data, &vm)
	if vm["id"] != "synthetic-id" || vm["ps"] != "备用 - old" {
		t.Fatal("VMess credentials changed")
	}
	native := `proxies:
  - {name: first, type: trojan, server: node.example, port: 443, password: synthetic, dialer-proxy: route}
  - {name: edge, type: ss, server: edge.example, port: 443, cipher: aes-128-gcm, password: edge-secret}
proxy-groups:
  - {name: route, type: select, proxies: [edge, DIRECT]}
`
	prefixed, err = PrefixSubscriptionNodes(native, "backup - ")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseNativeClash(prefixed)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Proxies[0]["name"] != "backup - first" || doc.Proxies[0]["password"] != "synthetic" || doc.Proxies[0]["dialer-proxy"] != "backup - route" || AsStringList(doc.Groups[0]["proxies"])[0] != "backup - edge" {
		t.Fatal("native dependency names inconsistent", prefixed)
	}
}
