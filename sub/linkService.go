package sub

import (
	"encoding/json"
	"strings"

	"github.com/shenaba/2s-ui/database/model"
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

// LinksForClient combines existing entries with panel-owned grants at export
// time. It never copies provider URLs into the client's persisted links.
func (s *LinkService) LinksForClient(client *model.Client) (json.RawMessage, error) {
	var links []Link
	if len(client.Links) > 0 {
		if err := json.Unmarshal(client.Links, &links); err != nil {
			return nil, err
		}
	}
	var catalog service.ExternalSubSourceService
	sources, err := catalog.AuthorizedSources(client.Id)
	if err != nil {
		return nil, err
	}
	catalogURLs := map[string]bool{}
	catalogIDs := map[string]bool{}
	for _, source := range sources {
		catalogURLs[source.URL] = true
		catalogIDs[source.Id] = true
	}
	result := make([]Link, 0, len(links)+len(sources))
	for _, link := range links {
		// Prefer the snapshot-only catalog grant for the same URL. Without
		// explicit adoption the legacy authorization remains stored separately.
		if link.Type == "sub" && catalogURLs[link.Uri] {
			continue
		}
		if link.Type == "source" && catalogIDs[link.Uri] {
			continue
		}
		result = append(result, link)
	}
	seenURLs := map[string]bool{}
	for _, source := range sources {
		if seenURLs[source.URL] {
			continue
		}
		seenURLs[source.URL] = true
		result = append(result, Link{Type: "source", Remark: source.Name, Uri: source.Id})
	}
	return json.Marshal(result)
}

func externalLinkBody(link Link, clientId []uint, native ...bool) (string, bool, error) {
	if link.Type == "source" {
		if len(clientId) == 0 {
			return "", true, service.ErrExternalSubCacheUnavailable
		}
		var catalog service.ExternalSubSourceService
		body, err := catalog.Resolve(link.Uri, clientId[0], native...)
		return body, true, err
	}
	var cache service.ExternalSubCacheService
	return cache.Resolve(link.Uri, native...)
}

// The legacy wrappers remain useful to standalone converters. Subscription
// renderers use the checked variants so a cold cache cannot publish a partial
// configuration that replaces the client's last working one.
func (s *LinkService) GetLinks(raw *json.RawMessage, types, info string) []string {
	result, _ := s.GetLinksChecked(raw, types, info)
	return result
}

func (s *LinkService) GetLinksChecked(raw *json.RawMessage, types, info string, clientId ...uint) ([]string, error) {
	expanded, err := s.GetLinkListChecked(raw, types, info, clientId...)
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

func (s *LinkService) GetLinkListChecked(raw *json.RawMessage, types, info string, clientId ...uint) ([]Link, error) {
	var links, result []Link
	if raw == nil || len(*raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(*raw, &links); err != nil {
		return nil, err
	}
	for _, link := range links {
		switch link.Type {
		case "external":
			result = append(result, link)
		case "sub", "source":
			body, _, err := externalLinkBody(link, clientId)
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

func (s *LinkService) GetExternalOutboundsChecked(raw *json.RawMessage, forClash bool, clientId ...uint) ([]map[string]interface{}, []string, error) {
	var links []Link
	if raw == nil || len(*raw) == 0 {
		return nil, nil, nil
	}
	if err := json.Unmarshal(*raw, &links); err != nil {
		return nil, nil, err
	}
	var outbounds []map[string]interface{}
	var tags []string
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
		case "sub", "source":
			body, cached, err := externalLinkBody(link, clientId)
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

// Clash exports select a native snapshot before considering universal fallback.
// Keeping this separate avoids a lossy native -> sing-box -> Clash round trip.
func (s *LinkService) ClashExternalSources(raw *json.RawMessage, clientId uint) ([]map[string]interface{}, []string, []util.NativeClash, error) {
	var links []Link
	if raw == nil || len(*raw) == 0 {
		return nil, nil, nil, nil
	}
	if err := json.Unmarshal(*raw, &links); err != nil {
		return nil, nil, nil, err
	}
	var outbounds []map[string]interface{}
	var tags []string
	var native []util.NativeClash
	for _, link := range links {
		switch link.Type {
		case "external":
			outbound, tag, err := util.GetOutboundForClash(link.Uri, 0)
			if err == nil && outbound != nil && tag != "" {
				outbounds = append(outbounds, *outbound)
				tags = append(tags, tag)
			}
		case "sub", "source":
			body, cached, err := externalLinkBody(link, []uint{clientId}, true)
			if err != nil {
				return nil, nil, nil, err
			}
			if document, err := util.ParseNativeClash(body); err == nil {
				document.Namespace = link.Uri
				native = append(native, *document)
				continue
			}
			parsed, err := util.ParseExternalSubData(body, true)
			if err != nil {
				if cached {
					return nil, nil, nil, service.ErrExternalSubCacheUnavailable
				}
				continue
			}
			for _, outbound := range parsed {
				if tag, _ := outbound["tag"].(string); tag != "" {
					outbounds = append(outbounds, outbound)
					tags = append(tags, tag)
				}
			}
		}
	}
	return outbounds, tags, native, nil
}
