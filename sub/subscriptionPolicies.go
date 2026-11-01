package sub

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shenaba/2s-ui/database"
	"github.com/shenaba/2s-ui/database/model"
	"github.com/shenaba/2s-ui/service"
)

const autoInsertNodes = "__AUTO_INSERT_NODES__"

// MiSub-style placeholders expand in place. Filter selection applies only to
// dynamic nodes, never to explicitly retained fallback groups or DIRECT.
func expandTemplateGroups(output map[string]interface{}, tags []string) {
	for _, group := range proxyGroupList(output["proxy-groups"]) {
		matched := tags
		filter, hasFilter := group["filter"].(string)
		if hasFilter {
			matched = filteredProxyTags(tags, filter)
		}
		var names []string
		inserted := false
		for _, name := range proxyNames(group["proxies"]) {
			if name == autoInsertNodes || strings.EqualFold(name, "all") {
				names = appendProxyNames(names, matched)
				inserted = true
			} else {
				names = appendProxyNames(names, []string{name})
			}
		}
		if hasFilter && !inserted {
			names = appendProxyNames(names, matched)
		}
		group["proxies"] = names
		delete(group, "filter")
	}
}

// Empty categories can occur when a client is not granted a source. Remove
// them transitively and retarget rules instead of publishing an invalid group.
func pruneEmptyTemplateGroups(output map[string]interface{}) {
	groups := proxyGroupList(output["proxy-groups"])
	removed := map[string]bool{}
	for {
		changed := false
		kept := make([]map[string]interface{}, 0, len(groups))
		for _, group := range groups {
			names := []string{}
			for _, name := range proxyNames(group["proxies"]) {
				if !removed[name] {
					names = append(names, name)
				}
			}
			group["proxies"] = names
			if len(names) == 0 && len(proxyNames(group["use"])) == 0 {
				removed[group["name"].(string)] = true
				changed = true
			} else {
				kept = append(kept, group)
			}
		}
		groups = kept
		if !changed {
			break
		}
	}
	output["proxy-groups"] = groups
	fallback := "DIRECT"
	if len(groups) > 0 {
		fallback, _ = groups[0]["name"].(string)
	}
	if rules, ok := output["rules"].([]interface{}); ok {
		for i, raw := range rules {
			rule, ok := raw.(string)
			if !ok {
				continue
			}
			parts := strings.Split(rule, ",")
			target := len(parts) - 1
			if target > 0 && strings.EqualFold(strings.TrimSpace(parts[target]), "no-resolve") {
				target--
			}
			if removed[strings.TrimSpace(parts[target])] {
				parts[target] = fallback
				rules[i] = strings.Join(parts, ",")
			}
		}
	}
}

func legacySubscriptionFormat(query url.Values, agent, defaultFormat string) (string, error) {
	selected := query.Get("target")
	if selected == "" {
		selected = query.Get("format")
	}
	if selected == "" {
		for _, flag := range []string{"clash", "singbox", "base64", "v2ray", "trojan", "plain"} {
			if _, ok := query[flag]; ok {
				selected = flag
				break
			}
		}
	}
	if selected == "" && defaultFormat != "auto" {
		selected = defaultFormat
	}
	if selected == "" {
		agent = strings.ToLower(agent)
		switch {
		case strings.Contains(agent, "clash"), strings.Contains(agent, "mihomo"), strings.Contains(agent, "meta"), strings.Contains(agent, "stash"), strings.Contains(agent, "nekoray"):
			selected = "clash"
		case strings.Contains(agent, "sing-box"), strings.Contains(agent, "singbox"):
			selected = "json"
		default:
			selected = "base64"
		}
	}
	switch strings.ToLower(selected) {
	case "clash", "clashmeta", "mihomo":
		return "clash", nil
	case "json", "singbox", "sing-box":
		return "json", nil
	case "base64", "v2ray", "trojan":
		return "base64", nil
	case "plain":
		return "plain", nil
	default:
		return "", errors.New("legacy-format-unsupported")
	}
}

// Only dedicated, explicitly registered legacy hosts are intercepted. Unknown
// paths on them never fall through to native client names or dashboard routes.
func (s *SubHandler) legacySubscriptions() gin.HandlerFunc {
	return func(c *gin.Context) {
		var aliases service.LegacySubscriptionAliasService
		alias, knownHost, err := aliases.Match(c.Request.Host, c.Request.URL.EscapedPath(), c.Request.URL.RawQuery)
		if err != nil {
			if errors.Is(err, service.ErrLegacySubscriptionQuery) {
				c.AbortWithStatus(http.StatusBadRequest)
				return
			}
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if !knownHost {
			c.Next()
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		if alias == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Header("Allow", "GET, HEAD")
			c.AbortWithStatus(http.StatusMethodNotAllowed)
			return
		}
		now := time.Now().Unix()
		if !alias.Enabled || alias.ClientId == 0 || alias.Expiry != 0 && alias.Expiry <= now {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		var client model.Client
		if err := database.GetDB().Where("id = ? AND enable = ? AND (expiry = 0 OR expiry > ?)", alias.ClientId, true, now).First(&client).Error; err != nil {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		format, err := legacySubscriptionFormat(c.Request.URL.Query(), c.Request.UserAgent(), alias.DefaultFormat)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		var result *string
		var headers []string
		switch format {
		case "clash":
			result, headers, err = s.ClashService.GetClash(client.Name, service.SubscriptionRenderOptions{TemplateId: alias.ClashTemplateId})
		case "json":
			result, headers, err = s.JsonService.GetJson(client.Name, "json", service.SubscriptionRenderOptions{TemplateId: alias.JsonTemplateId})
		default:
			encode := format == "base64"
			result, headers, err = s.SubService.GetSubs(client.Name, service.SubscriptionRenderOptions{Encode: &encode})
		}
		if err != nil || result == nil {
			s.subscriptionError(c, err)
			c.Abort()
			return
		}
		headers[2] = alias.Name
		s.addHeaders(c, headers)
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusOK)
		} else {
			c.String(http.StatusOK, *result)
		}
		c.Abort()
	}
}
