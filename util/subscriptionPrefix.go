package util

import (
	"encoding/json"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// PrefixSubscriptionNodes changes labels at export time, keeping the stored
// provider snapshot and all connection credentials untouched.
func PrefixSubscriptionNodes(body, prefix string) (string, error) {
	if prefix == "" {
		return body, nil
	}
	if doc, err := ParseNativeClash(body); err == nil {
		names := map[string]string{}
		for _, proxy := range doc.Proxies {
			name := proxy["name"].(string)
			names[name] = prefix + name
		}
		for _, group := range doc.Groups {
			name := group["name"].(string)
			names[name] = prefix + name
		}
		for _, proxy := range doc.Proxies {
			proxy["name"] = names[proxy["name"].(string)]
			if via, _ := proxy["dialer-proxy"].(string); names[via] != "" {
				proxy["dialer-proxy"] = names[via]
			}
		}
		for _, group := range doc.Groups {
			group["name"] = names[group["name"].(string)]
			members := AsStringList(group["proxies"])
			for i, name := range members {
				if names[name] != "" {
					members[i] = names[name]
				}
			}
			group["proxies"] = members
		}
		data, err := yaml.Marshal(map[string]interface{}{"proxies": doc.Proxies, "proxy-groups": doc.Groups, "hosts": doc.Hosts})
		return string(data), err
	}
	decoded := strings.TrimSpace(DecodeExternalSubBody(body))
	if strings.HasPrefix(decoded, "{") {
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(decoded), &doc); err != nil {
			return "", err
		}
		entries, _ := doc["outbounds"].([]interface{})
		names := map[string]string{}
		for _, entry := range entries {
			if ob, ok := entry.(map[string]interface{}); ok {
				if name, _ := ob["tag"].(string); name != "" {
					names[name] = prefix + name
				}
			}
		}
		for _, entry := range entries {
			ob, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			if name, _ := ob["tag"].(string); names[name] != "" {
				ob["tag"] = names[name]
			}
			if via, _ := ob["detour"].(string); names[via] != "" {
				ob["detour"] = names[via]
			}
			members := AsStringList(ob["outbounds"])
			for i, name := range members {
				if names[name] != "" {
					members[i] = names[name]
				}
			}
			if len(members) > 0 {
				ob["outbounds"] = members
			}
			if selected, _ := ob["default"].(string); names[selected] != "" {
				ob["default"] = names[selected]
			}
		}
		data, err := json.Marshal(doc)
		return string(data), err
	}
	lines := strings.Split(decoded, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "vmess://") {
			var doc map[string]interface{}
			data, err := B64StrToByte(strings.TrimPrefix(line, "vmess://"))
			if err != nil || json.Unmarshal(data, &doc) != nil || doc == nil {
				continue
			}
			name, _ := doc["ps"].(string)
			doc["ps"] = prefix + name
			data, _ = json.Marshal(doc)
			lines[i] = "vmess://" + ByteToB64Str(data)
		} else if strings.Contains(line, "://") {
			before, fragment, _ := strings.Cut(line, "#")
			name, err := url.PathUnescape(fragment)
			if err != nil {
				return "", err
			}
			// Preserve the original authority and query byte-for-byte.
			lines[i] = before + "#" + url.PathEscape(prefix+name)
		}
	}
	return strings.Join(lines, "\n"), nil
}
