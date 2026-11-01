package util

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shenaba/2s-ui/logger"
	"github.com/shenaba/2s-ui/util/common"
)

// maxExternalSubBytes bounds one external subscription body.
const maxExternalSubBytes = 8 << 20

// One client for the process, not one per call: a custom Transport holds its
// idle connections forever (zero IdleConnTimeout) and is never collected, so
// building one per call leaked a socket for every external link in every
// subscription fetch (issue #176). Sharing it also lets repeat fetches of the
// same upstream reuse a connection.
//
// It verifies the certificate: the response is turned into the outbound
// configurations this panel serves to its own subscribers, so whoever can
// intercept the fetch chooses the servers every one of those clients connects
// to -- and the panel would report success the whole time. This drops support
// for a source with a self-signed certificate, or one reached by bare IP.
// There is no setting to turn that off, deliberately: a source worth trusting
// with that is worth a real certificate, and a toggle would be found by
// exactly the operators who should not use it.
var externalSubClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		IdleConnTimeout: 90 * time.Second,
	},
}

func GetExternalLink(url string) string {
	response, err := externalSubClient.Get(url)
	if err != nil {
		logger.Warning("sub: Error making HTTP request:", err)
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		logger.Warning("sub: external subscription returned HTTP status ", response.StatusCode)
		return ""
	}

	// The body is an operator-configured URL's answer, which is not this
	// panel's to size: an unbounded ReadAll lets a redirect to a large file --
	// or a source that simply never stops -- take the process's memory with it,
	// on a fetch that runs on every subscription request. Eight mebibytes is
	// far past any real subscription; what does not fit is treated as a source
	// that cannot be read, since a truncated one would be served as if it were
	// the whole list.
	body, err := io.ReadAll(io.LimitReader(response.Body, maxExternalSubBytes+1))
	if err != nil {
		logger.Warning("sub: Error reading response body:", err)
		return ""
	}
	if len(body) > maxExternalSubBytes {
		logger.Warning("sub: external subscription is larger than ",
			maxExternalSubBytes, " bytes, ignoring it: ", url)
		return ""
	}

	// Any base64 form: providers serve unpadded and URL-safe bodies as well,
	// and standard-only decoding handed those on as one line of base64, from
	// which no link could be read (#1281). Trimmed first, since the decoders
	// skip line breaks but not other whitespace; a body that is not base64 is
	// returned as it came.
	return DecodeExternalSubBody(string(body))
}

// DecodeExternalSubBody accepts the same plain and base64 body forms as the
// live fetch path. Snapshot readers use it without contacting the provider.
func DecodeExternalSubBody(body string) string {
	if decoded, err := decodeAnyBase64(strings.TrimSpace(body)); err == nil {
		return string(decoded)
	}
	return body
}

func GetExternalSub(url string) ([]map[string]interface{}, error) {
	return getExternalSub(url, GetOutbound)
}

// GetExternalSubForClash reads a text subscription's links with
// GetOutboundForClash, so its XHTTP nodes reach the Clash export intact. A
// sing-box JSON subscription is read the same either way.
func GetExternalSubForClash(url string) ([]map[string]interface{}, error) {
	return getExternalSub(url, GetOutboundForClash)
}

func getExternalSub(url string, parseLink func(string, int) (*map[string]interface{}, string, error)) ([]map[string]interface{}, error) {
	if len(url) == 0 {
		return nil, common.NewError("no url")
	}
	return parseExternalSubData(GetExternalLink(url), parseLink)
}

// ParseExternalSubData parses a saved response using the requested export's
// link converter. In particular Clash keeps XHTTP options that sing-box cannot
// represent. This function performs no network requests.
func ParseExternalSubData(data string, forClash bool) ([]map[string]interface{}, error) {
	if forClash {
		return parseExternalSubData(data, GetOutboundForClash)
	}
	return parseExternalSubData(data, GetOutbound)
}

func parseExternalSubData(body string, parseLink func(string, int) (*map[string]interface{}, string, error)) ([]map[string]interface{}, error) {
	var err error
	var result []map[string]interface{}

	// Trimmed: a JSON body ending in a newline failed the brace check below and
	// was read as a list of links.
	data := strings.TrimSpace(DecodeExternalSubBody(body))
	if len(data) == 0 {
		return nil, common.NewError("no result")
	}

	// if the data is a JSON object
	if strings.HasPrefix(data, "{") && strings.HasSuffix(data, "}") {
		var jsonData map[string]interface{}
		err = json.Unmarshal([]byte(data), &jsonData)
		if err != nil {
			logger.Warning("sub: Error unmarshalling JSON:", err)
			return nil, err
		}
		outbounds, ok := jsonData["outbounds"].([]any)
		if !ok {
			return nil, common.NewError("no outbounds in external subscription")
		}
		for _, outbound := range outbounds {
			outboundMap, ok := outbound.(map[string]interface{})
			if ok && len(outboundMap) > 0 {
				oType, _ := outboundMap["type"].(string)
				switch oType {
				case "urltest":
				case "direct":
				case "selector":
				case "block":
					continue
				default:
					result = append(result, outboundMap)
				}
			}
		}
		if len(result) == 0 {
			return nil, common.NewError("no result")
		}
		return result, nil
	} else {
		// if data is a text
		links := strings.Split(data, "\n")
		for _, link := range links {
			// A CRLF body left a '\r' on every line, and url.Parse refuses a
			// control character, so not one link in it was read.
			link = strings.TrimSpace(link)
			if link == "" {
				continue
			}
			linkToJson, _, err := parseLink(link, 0)
			if err == nil {
				result = append(result, *linkToJson)
			}
		}
	}
	if len(result) == 0 {
		return nil, common.NewError("no result")
	}
	return result, nil
}
