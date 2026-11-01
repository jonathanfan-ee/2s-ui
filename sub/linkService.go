package sub

import (
	"encoding/json"
	"strings"

	"github.com/shenaba/2s-ui/logger"
	"github.com/shenaba/2s-ui/service"
	"github.com/shenaba/2s-ui/util"
)

type Link struct {
	Type   string `json:"type"`
	Remark string `json:"remark"`
	Uri    string `json:"uri"`
}

type LinkService struct {
}

// The legacy wrappers remain useful to standalone converters. Subscription
// renderers use the checked variants so a cold cache cannot publish a partial
// configuration that replaces the client's last working one.
func (s *LinkService) GetLinks(raw *json.RawMessage, types, info string) []string {
	result, _ := s.GetLinksChecked(raw, types, info)
	return result
}

func (s *LinkService) GetLinksChecked(raw *json.RawMessage, types, info string) ([]string, error) {
	expanded, err := s.GetLinkListChecked(raw, types, info)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, link := range expanded {
		result = append(result, link.Uri)
	}
	return result, nil
}

func (s *LinkService) GetLinkList(raw *json.RawMessage, types, info string) []Link {
	result, _ := s.GetLinkListChecked(raw, types, info)
	return result
}

func (s *LinkService) GetLinkListChecked(raw *json.RawMessage, types, info string) ([]Link, error) {
	var links, result []Link
	if raw == nil || len(*raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(*raw, &links); err != nil {
		return nil, err
	}
	var cache service.ExternalSubCacheService
	for _, link := range links {
		switch link.Type {
		case "external":
			result = append(result, link)
		case "sub":
			body, _, err := cache.Resolve(link.Uri)
			if err != nil {
				return nil, err
			}
			for _, uri := range strings.Split(body, "\n") {
				if uri = strings.TrimSpace(uri); uri == "" {
					continue
				}
				result = append(result, Link{Type: link.Type, Remark: link.Remark, Uri: uri})
			}
		case "local":
			if types == "all" {
				result = append(result, Link{Type: link.Type, Remark: link.Remark, Uri: s.addClientInfo(link.Uri, info)})
			}
		}
	}
	return result, nil
}

func (s *LinkService) GetExternalOutbounds(raw *json.RawMessage) ([]map[string]interface{}, []string) {
	result, tags, _ := s.GetExternalOutboundsChecked(raw, false)
	return result, tags
}

// The Clash converter preserves XHTTP, which the sing-box exporter drops.
func (s *LinkService) GetExternalOutboundsForClash(raw *json.RawMessage) ([]map[string]interface{}, []string) {
	result, tags, _ := s.GetExternalOutboundsChecked(raw, true)
	return result, tags
}

func (s *LinkService) GetExternalOutboundsChecked(raw *json.RawMessage, forClash bool) ([]map[string]interface{}, []string, error) {
	var links []Link
	if raw == nil || len(*raw) == 0 {
		return nil, nil, nil
	}
	if err := json.Unmarshal(*raw, &links); err != nil {
		return nil, nil, err
	}
	var outbounds []map[string]interface{}
	var tags []string
	var cache service.ExternalSubCacheService
	parseLink := util.GetOutbound
	if forClash {
		parseLink = util.GetOutboundForClash
	}
	for _, link := range links {
		switch link.Type {
		case "external":
			outbound, tag, err := parseLink(link.Uri, 0)
			if err == nil && outbound != nil && tag != "" {
				outbounds = append(outbounds, *outbound)
				tags = append(tags, tag)
			}
		case "sub":
			body, cached, err := cache.Resolve(link.Uri)
			if err != nil {
				return nil, nil, err
			}
			subOutbounds, err := util.ParseExternalSubData(body, forClash)
			if err != nil {
				if cached {
					return nil, nil, service.ErrExternalSubCacheUnavailable
				}
				logger.Warning("sub: could not parse external subscription")
				continue
			}
			for _, outbound := range subOutbounds {
				if tag, _ := outbound["tag"].(string); tag != "" {
					outbounds = append(outbounds, outbound)
					tags = append(tags, tag)
				}
			}
		}
	}
	// Uniqueness is settled after all local and external sources are merged.
	return outbounds, tags, nil
}

func (s *LinkService) addClientInfo(uri string, clientInfo string) string {
	if len(clientInfo) == 0 {
		return uri
	}
	protocol := strings.Split(uri, "://")
	if len(protocol) < 2 {
		return uri
	}
	switch protocol[0] {
	case "vmess":
		var vmessJson map[string]interface{}
		config, err := util.B64StrToByte(protocol[1])
		if err != nil {
			logger.Warning("sub: Error decoding vmess content:", err)
			return uri
		}
		err = json.Unmarshal(config, &vmessJson)
		if err != nil {
			logger.Warning("sub: Error decoding vmess content:", err)
			return uri
		}
		// A vmess link carrying no "ps", or one whose "ps" is not a string, used
		// to panic here and take down the whole subscription response.
		//
		// So did a payload that decodes to JSON null or to anything else that
		// is not an object: that yields a nil map, and the write below is a
		// write, not an assertion. The two branches above already hand back an
		// unreadable link untouched; this is the third shape of unreadable.
		if vmessJson == nil {
			logger.Warning("sub: vmess content is not an object, leaving the link as it is")
			return uri
		}
		ps, _ := vmessJson["ps"].(string)
		vmessJson["ps"] = ps + clientInfo
		result, err := json.MarshalIndent(vmessJson, "", "  ")
		if err != nil {
			logger.Warning("sub: Error decoding vmess + clientInfo content:", err)
			return uri
		}
		return "vmess://" + util.ByteToB64Str(result)
	default:
		return uri + clientInfo
	}
}
