package sub

// Endpoint host isolation and default TLS/transport identity preservation are
// adapted from MiSub (MIT); see LICENSES/MiSub.txt. The merge stays source-local.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/shenaba/2s-ui/util"
	"gopkg.in/yaml.v3"
)

func copyClashMap(raw map[string]interface{}) map[string]interface{} {
	data, _ := yaml.Marshal(raw)
	var result map[string]interface{}
	_ = yaml.Unmarshal(data, &result)
	return result
}

func uniqueClashName(name string, used map[string]bool) string {
	base := name
	for i := 2; used[name] || util.ClashBuiltins[strings.ToUpper(name)]; i++ {
		name = base + " [" + strconv.Itoa(i) + "]"
	}
	used[name] = true
	return name
}

func mergeNativeClash(output map[string]interface{}, converted []interface{}, tags []string, documents []util.NativeClash) ([]interface{}, []string, []map[string]interface{}, error) {
	used := map[string]bool{"Proxy": true, "Auto": true}
	for _, group := range proxyGroupList(output["proxy-groups"]) {
		if name, _ := group["name"].(string); name != "" {
			used[name] = true
		}
	}
	if existing, ok := output["proxies"].([]interface{}); ok {
		for _, entry := range existing {
			if p, ok := entry.(map[string]interface{}); ok {
				if name, _ := p["name"].(string); name != "" {
					used[name] = true
				}
			}
		}
	}
	rename := map[string]string{}
	for i, p := range converted {
		proxy := p.(map[string]interface{})
		name := proxy["name"].(string)
		newName := uniqueClashName(name, used)
		rename[name] = newName
		proxy["name"] = newName
		tags[i] = newName
	}
	for _, p := range converted {
		proxy := p.(map[string]interface{})
		if via, _ := proxy["dialer-proxy"].(string); rename[via] != "" {
			proxy["dialer-proxy"] = rename[via]
		}
	}
	adminHosts, _ := output["hosts"].(map[string]interface{})
	importedHosts := map[string]interface{}{}
	var groups []map[string]interface{}
	for sourceIndex, document := range documents {
		names := map[string]string{}
		for _, proxy := range document.Proxies {
			name := proxy["name"].(string)
			names[name] = uniqueClashName(name, used)
		}
		for _, group := range document.Groups {
			name := group["name"].(string)
			names[name] = uniqueClashName(name, used)
		}
		for _, raw := range document.Proxies {
			proxy := copyClashMap(raw)
			oldName := proxy["name"].(string)
			proxy["name"] = names[oldName]
			if via, _ := proxy["dialer-proxy"].(string); via != "" {
				if names[via] != "" {
					proxy["dialer-proxy"] = names[via]
				} else if !util.ClashBuiltins[via] {
					return nil, nil, nil, util.ErrNativeClash
				}
			}
			if err := isolateNativeHost(proxy, document.Hosts, adminHosts, importedHosts, nativeNamespace(document, sourceIndex)); err != nil {
				return nil, nil, nil, err
			}
			converted = append(converted, proxy)
			tags = append(tags, names[oldName])
		}
		for _, raw := range document.Groups {
			group := copyClashMap(raw)
			group["name"] = names[raw["name"].(string)]
			members := util.AsStringList(group["proxies"])
			for i, member := range members {
				if names[member] != "" {
					members[i] = names[member]
				}
			}
			group["proxies"] = members
			groups = append(groups, group)
		}
	}
	if len(importedHosts) > 0 {
		hosts := map[string]interface{}{}
		for k, v := range importedHosts {
			hosts[k] = v
		}
		for k, v := range adminHosts {
			hosts[k] = v
		}
		output["hosts"] = hosts
	}
	return converted, tags, groups, nil
}

func isolateNativeHost(proxy, hosts, admin, imported map[string]interface{}, source string) error {
	original := proxy["server"].(string)
	server := original
	visited := map[string]bool{}
	for steps := 0; steps < 64; steps++ {
		if visited[server] {
			return errors.New("native-host-alias-cycle")
		}
		visited[server] = true
		if _, ok := util.NativeHostValue(admin, server); ok {
			break
		}
		value, exists := util.NativeHostValue(hosts, server)
		if !exists {
			break
		}
		if alias, ok := value.(string); ok && net.ParseIP(alias) == nil {
			server = alias
			continue
		}
		encoded, _ := json.Marshal([]interface{}{source, server, value})
		hash := sha256.Sum256(encoded)
		alias := "sui-" + hex.EncodeToString(hash[:12]) + ".invalid"
		if _, exists := util.NativeHostValue(admin, alias); exists {
			return errors.New("native-host-name-conflict")
		}
		imported[alias] = value
		server = alias
		break
	}
	if server == original {
		return nil
	}
	proxy["server"] = server
	// Changing a dial endpoint must not change its implicit TLS identity or Host.
	kind, _ := proxy["type"].(string)
	tls, _ := proxy["tls"].(bool)
	if kind == "vless" || kind == "vmess" {
		if tls && stringEmpty(proxy["servername"]) {
			proxy["servername"] = original
			if proxy["network"] == "ws" {
				if opts, ok := proxy["ws-opts"].(map[string]interface{}); ok {
					if headers, ok := opts["headers"].(map[string]interface{}); ok {
						for k, v := range headers {
							if strings.EqualFold(k, "Host") {
								if host, ok := v.(string); ok && host != "" {
									proxy["servername"] = host
								}
							}
						}
					}
				}
			}
		}
	} else if tls || kind == "trojan" || kind == "hysteria" || kind == "hysteria2" || kind == "tuic" || kind == "anytls" {
		if stringEmpty(proxy["sni"]) {
			proxy["sni"] = original
		}
	}
	network, _ := proxy["network"].(string)
	if network == "ws" || network == "http" {
		key := network + "-opts"
		opts, _ := proxy[key].(map[string]interface{})
		if opts == nil {
			opts = map[string]interface{}{}
		}
		headers, _ := opts["headers"].(map[string]interface{})
		if headers == nil {
			headers = map[string]interface{}{}
		}
		hasHost := false
		for name := range headers {
			if strings.EqualFold(name, "Host") {
				hasHost = true
			}
		}
		if !hasHost {
			if network == "ws" {
				headers["Host"] = original
			} else {
				headers["Host"] = []string{original}
			}
		}
		opts["headers"] = headers
		proxy[key] = opts
	}
	return nil
}

func nativeNamespace(document util.NativeClash, index int) string {
	if document.Namespace != "" {
		return document.Namespace
	}
	return strconv.Itoa(index)
}

func stringEmpty(value interface{}) bool { text, _ := value.(string); return text == "" }
