package service

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/shenaba/2s-ui/util"
)

const maxCachedSubBytes = 8 << 20

var errSubRequestBudget = errors.New("request-budget-exhausted")

type externalSubFetchResult struct {
	body     string
	status   int
	retryAt  int64
	clash    *externalSubFormatResult
	userInfo map[string]uint64
}

type externalSubFormatResult struct {
	body     string
	status   int
	retryAt  int64
	err      error
	userInfo map[string]uint64
}

// New scheduled sources are deliberately limited to public HTTPS over IPv4.
// Existing live external links keep their old transport while caching is off.
// The original hostname remains the HTTP/TLS identity; only the dial address
// is pinned to a checked public IPv4. Proxy environment variables are ignored.
var cachedSubTransport = &http.Transport{
	TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	TLSHandshakeTimeout: 5 * time.Second,
	IdleConnTimeout:     90 * time.Second,
	DisableCompression:  true,
	MaxIdleConns:        8,
	DialContext:         dialPublicSubscription,
}

var blockedSubNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

func publicSubscriptionIPv4(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range blockedSubNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func validateCachedSubURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return errors.New("source-requires-public-https")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicSubscriptionIPv4(ip) {
		return errors.New("source-address-not-public-ipv4")
	}
	return nil
}

func dialPublicSubscription(ctx context.Context, _ string, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return nil, errors.New("source-port-not-allowed")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("source-dns-failed")
	}
	for _, ip := range ips {
		if !publicSubscriptionIPv4(ip) {
			return nil, errors.New("source-dns-not-public")
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ips[0].Unmap().String(), port))
}

func fetchCachedExternalSub(ctx context.Context, raw string, reserve func() error) (externalSubFetchResult, error) {
	return fetchCachedFormatsWithTransport(ctx, raw, reserve, cachedSubTransport)
}

func fetchCachedFormatsWithTransport(ctx context.Context, raw string, reserve func() error, transport http.RoundTripper) (externalSubFetchResult, error) {
	var wait sync.WaitGroup
	wait.Add(2)
	var universal, native externalSubFetchResult
	var universalErr, nativeErr error
	go func() {
		defer wait.Done()
		universal, universalErr = fetchCachedFormat(ctx, raw, reserve, transport, false)
	}()
	go func() {
		defer wait.Done()
		native, nativeErr = fetchCachedFormat(ctx, raw, reserve, transport, true)
	}()
	wait.Wait()
	universal.clash = &externalSubFormatResult{body: native.body, status: native.status, retryAt: native.retryAt, err: nativeErr, userInfo: native.userInfo}
	return universal, universalErr
}

func fetchCachedSubWithTransport(ctx context.Context, raw string, reserve func() error, transport http.RoundTripper) (externalSubFetchResult, error) {
	return fetchCachedFormat(ctx, raw, reserve, transport, false)
}

func fetchCachedFormat(ctx context.Context, raw string, reserve func() error, transport http.RoundTripper, native bool) (externalSubFetchResult, error) {
	var result externalSubFetchResult
	if err := validateCachedSubURL(raw); err != nil {
		return result, err
	}
	if err := reserve(); err != nil {
		return result, err
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("source-too-many-redirects")
			}
			if err := validateCachedSubURL(req.URL.String()); err != nil {
				return err
			}
			return reserve()
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return result, errors.New("source-request-invalid")
	}
	if native {
		req.Header.Set("User-Agent", "ClashMeta/1.19.32")
	} else {
		req.Header.Set("User-Agent", "SubscriptionFetcher/1.0")
	}
	req.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(req)
	if err != nil {
		// net/http errors include the URL, which commonly holds a secret. Keep
		// the persisted and user-visible error vocabulary independent of it.
		if errors.Is(err, errSubRequestBudget) {
			return result, errSubRequestBudget
		}
		return result, errors.New("source-request-failed")
	}
	defer response.Body.Close()
	result.status = response.StatusCode
	result.retryAt = externalSubRetryAt(response.Header.Get("Retry-After"), time.Now().Unix())
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, errors.New("source-http-" + strconv.Itoa(response.StatusCode))
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return result, errors.New("source-content-encoding-unsupported")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCachedSubBytes+1))
	if err != nil {
		return result, errors.New("source-body-read-failed")
	}
	if len(body) > maxCachedSubBytes || !utf8.Valid(body) {
		return result, errors.New("source-body-invalid-or-too-large")
	}
	result.body = string(body)
	result.userInfo = parseSubscriptionUserInfo(response.Header.Get("Subscription-Userinfo"))
	if !native {
		result.body = util.DecodeExternalSubBody(result.body)
	}
	if err := validateExternalSubFormat(result.body, native); err != nil {
		return result, err
	}
	return result, nil
}

func validateCachedSubBody(body string) error {
	// Clash's converter also accepts XHTTP links, unlike the sing-box export.
	// An unsupported format must not erase a previously usable snapshot.
	if strings.TrimSpace(body) == "" || len(body) > maxCachedSubBytes {
		return errors.New("source-content-invalid")
	}
	proxies, err := util.ParseExternalSubData(body, true)
	if err != nil || len(proxies) == 0 {
		return errors.New("source-content-invalid")
	}
	for _, proxy := range proxies {
		tag, _ := proxy["tag"].(string)
		kind, _ := proxy["type"].(string)
		if tag != "" && kind != "" {
			return nil
		}
	}
	return errors.New("source-content-invalid")
}

func externalSubRetryAt(header string, now int64) int64 {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64); err == nil && seconds > 0 {
		const maxInt64 = int64(^uint64(0) >> 1)
		if seconds > maxInt64-now {
			return maxInt64
		}
		return now + seconds
	}
	if deadline, err := http.ParseTime(header); err == nil && deadline.Unix() > now {
		return deadline.Unix()
	}
	return 0
}

func validateExternalSubFormat(body string, native bool) error {
	if native {
		if _, err := util.ParseNativeClash(body); err != nil {
			return errors.New("source-native-clash-invalid")
		}
		return nil
	}
	return validateCachedSubBody(body)
}

func parseSubscriptionUserInfo(header string) map[string]uint64 {
	result := map[string]uint64{}
	for _, part := range strings.Split(header, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "upload", "download", "total", "expire":
			if n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil && n <= 1<<53 {
				result[key] = n
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
