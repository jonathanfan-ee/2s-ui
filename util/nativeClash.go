package util

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// NativeClash contains only source-local proxies, their required groups and
// endpoint hosts. Global rules, DNS settings and provider URLs are not imported.
type NativeClash struct {
	Proxies   []map[string]interface{}
	Groups    []map[string]interface{}
	Hosts     map[string]interface{}
	Namespace string
}

var ErrNativeClash = errors.New("source-native-clash-invalid")

var ClashBuiltins = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true, "GLOBAL": true}

func ParseNativeClash(body string) (*NativeClash, error) {
	if len(body) == 0 || len(body) > 8<<20 {
		return nil, ErrNativeClash
	}
	dec := yaml.NewDecoder(strings.NewReader(body))
	var node yaml.Node
	if dec.Decode(&node) != nil {
		return nil, ErrNativeClash
	}
	var extra yaml.Node
	if dec.Decode(&extra) != io.EOF {
		return nil, ErrNativeClash
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
		for _, child := range n.Content {
			if !guard(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !guard(&node, 0) {
		return nil, ErrNativeClash
	}
	var doc map[string]interface{}
	if node.Decode(&doc) != nil {
		return nil, ErrNativeClash
	}
	raw, ok := doc["proxies"].([]interface{})
	if !ok || len(raw) == 0 || len(raw) > 10000 {
		return nil, ErrNativeClash
	}
	result := &NativeClash{Hosts: map[string]interface{}{}}
	names := map[string]bool{}
	for _, entry := range raw {
		proxy, ok := entry.(map[string]interface{})
		if !ok {
			return nil, ErrNativeClash
		}
		name, _ := proxy["name"].(string)
		kind, _ := proxy["type"].(string)
		server, _ := proxy["server"].(string)
		port, err := strconv.Atoi(stringValue(proxy["port"]))
		if name == "" || kind == "" || server == "" || err != nil || port < 1 || port > 65535 || names[name] {
			return nil, ErrNativeClash
		}
		if via, exists := proxy["dialer-proxy"]; exists {
			if value, ok := via.(string); !ok || value == "" {
				return nil, ErrNativeClash
			}
		}
		names[name] = true
		result.Proxies = append(result.Proxies, proxy)
	}
	groups := map[string]map[string]interface{}{}
	if entries, ok := doc["proxy-groups"].([]interface{}); ok {
		for _, entry := range entries {
			if group, ok := entry.(map[string]interface{}); ok {
				name, _ := group["name"].(string)
				if name != "" {
					if names[name] || groups[name] != nil {
						return nil, ErrNativeClash
					}
					groups[name] = group
				}
			}
		}
	}
	// Import only dependency groups reachable from a node's dialer-proxy. Their
	// members must be self-contained; remote proxy-providers are never fetched.
	state := map[string]int{}
	needed := map[string]bool{}
	byName := map[string]map[string]interface{}{}
	for _, proxy := range result.Proxies {
		byName[proxy["name"].(string)] = proxy
	}
	var visit func(string) bool
	visit = func(name string) bool {
		if byName[name] == nil && groups[name] == nil && ClashBuiltins[name] {
			return true
		}
		if state[name] == 1 {
			return false
		}
		if state[name] == 2 {
			return true
		}
		state[name] = 1
		if proxy := byName[name]; proxy != nil {
			if via, _ := proxy["dialer-proxy"].(string); via != "" && !visit(via) {
				return false
			}
		} else if group := groups[name]; group != nil {
			for _, flag := range []string{"include-all", "include-all-proxies", "include-all-providers"} {
				if AsBool(group[flag]) {
					return false
				}
			}
			if use, exists := group["use"]; exists && len(AsStringList(use)) > 0 {
				return false
			}
			members := AsStringList(group["proxies"])
			if len(members) == 0 {
				return false
			}
			for _, member := range members {
				if !visit(member) {
					return false
				}
			}
			needed[name] = true
		} else {
			return false
		}
		state[name] = 2
		return true
	}
	for _, proxy := range result.Proxies {
		if !visit(proxy["name"].(string)) {
			return nil, ErrNativeClash
		}
	}
	// Stable group ordering follows the original document, not map iteration.
	if entries, ok := doc["proxy-groups"].([]interface{}); ok {
		for _, entry := range entries {
			if group, ok := entry.(map[string]interface{}); ok {
				if name, _ := group["name"].(string); needed[name] {
					result.Groups = append(result.Groups, group)
				}
			}
		}
	}
	if hosts, ok := doc["hosts"].(map[string]interface{}); ok {
		for name, value := range hosts {
			switch v := value.(type) {
			case string:
				if v == "" {
					return nil, ErrNativeClash
				}
				result.Hosts[name] = v
			case []interface{}:
				if len(v) == 0 {
					return nil, ErrNativeClash
				}
				for _, address := range v {
					ip, ok := address.(string)
					if !ok || net.ParseIP(ip) == nil {
						return nil, ErrNativeClash
					}
				}
				result.Hosts[name] = v
			default:
				return nil, ErrNativeClash
			}
		}
	}
	for _, proxy := range result.Proxies {
		current := proxy["server"].(string)
		visited := map[string]bool{}
		for steps := 0; steps < 64; steps++ {
			if visited[current] {
				return nil, ErrNativeClash
			}
			visited[current] = true
			value, _ := NativeHostValue(result.Hosts, current)
			alias, ok := value.(string)
			if !ok || net.ParseIP(alias) != nil {
				break
			}
			current = alias
			if steps == 63 {
				return nil, ErrNativeClash
			}
		}
	}

	if _, err := json.Marshal(result); err != nil {
		return nil, ErrNativeClash
	}
	return result, nil
}

func stringValue(value interface{}) string {
	switch v := value.(type) {
	case int:
		return strconv.Itoa(v)
	case uint16:
		return strconv.Itoa(int(v))
	case float64:
		if v == float64(int(v)) {
			return strconv.Itoa(int(v))
		}
	case string:
		return v
	}
	return ""
}

// NativeHostValue scopes wildcard hosts lookup to a single endpoint. Ties use
// lexical order so the same snapshot cannot choose a different dial address.
func NativeHostValue(hosts map[string]interface{}, name string) (interface{}, bool) {
	if value, ok := hosts[name]; ok {
		return value, true
	}
	keys := make([]string, 0, len(hosts))
	for key := range hosts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) == len(keys[j]) {
			return keys[i] < keys[j]
		}
		return len(keys[i]) > len(keys[j])
	})
	for _, key := range keys {
		suffix := ""
		matchRoot := false
		if strings.HasPrefix(key, "+.") {
			suffix = key[2:]
			matchRoot = true
		} else if strings.HasPrefix(key, "*.") {
			suffix = key[2:]
		} else if strings.HasPrefix(key, ".") {
			suffix = key[1:]
			matchRoot = true
		}
		if suffix != "" && (strings.HasSuffix(name, "."+suffix) || (matchRoot && name == suffix)) {
			return hosts[key], true
		}
	}
	return nil, false
}
