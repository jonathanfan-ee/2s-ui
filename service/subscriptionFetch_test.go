package service

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

type snapshotRoundTripper func(*http.Request) (*http.Response, error)

func (f snapshotRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func snapshotResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestExternalSubFetchValidation(t *testing.T) {
	for _, raw := range []string{"http://provider.example/sub", "https://user:pass@provider.example/sub", "https://provider.example:8443/sub", "https://provider.example/sub#fragment", "https://127.0.0.1/sub", "https://10.0.0.1/sub", "https://100.64.1.2/sub", "https://192.0.2.1/sub", "https://198.18.0.1/sub", "https://[::1]/sub", "https://[2606:4700::1111]/sub"} {
		if err := validateCachedSubURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{snapshotTestURL, "https://8.8.8.8/sub", "https://provider.example:443/sub"} {
		if err := validateCachedSubURL(raw); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"0.0.0.0", "169.254.169.254", "172.16.1.1", "192.168.1.1", "192.0.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255"} {
		if publicSubscriptionIPv4(netip.MustParseAddr(raw)) {
			t.Errorf("accepted nonpublic IP %q", raw)
		}
	}
	// Literal addresses do not need a DNS/network query; this tests the actual
	// dial guard, rather than relying only on the input URL guard.
	if conn, err := dialPublicSubscription(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", "443")); err == nil || conn != nil {
		t.Fatal("dial guard allowed loopback")
	}
}

func TestExternalSubFetchRejectsErrorBodiesAndDecodes(t *testing.T) {
	for _, code := range []int{200, 302, 401, 403, 429, 500, 503} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			requests, budget := 0, 0
			fetch := snapshotRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Header.Get("User-Agent") != "SubscriptionFetcher/1.0" || r.Header.Get("Accept-Encoding") != "identity" {
					t.Fatal("wrong request headers")
				}
				response := snapshotResponse(code, base64.StdEncoding.EncodeToString([]byte(snapshotTestBody)))
				response.Header.Set("Retry-After", "900")
				return response, nil
			})
			result, err := fetchCachedSubWithTransport(context.Background(), snapshotTestURL, func() error { budget++; return nil }, fetch)
			if (err == nil) != (code == 200) {
				t.Fatalf("status %d err=%v", code, err)
			}
			if requests != 1 || budget != 1 {
				t.Fatalf("requests=%d budget=%d", requests, budget)
			}
			if result.retryAt < time.Now().Unix()+895 {
				t.Fatal("lost Retry-After")
			}
			if code == 200 && result.body != snapshotTestBody {
				t.Fatal("body not decoded")
			}
		})
	}
	for name, body := range map[string]string{"empty": "", "json-error": `{"outbounds":[{"error":"suspended"}]}`, "html": "<html>Login required</html>", "invalid": "unsupported://node", "oversized": strings.Repeat("a", maxCachedSubBytes+1), "utf8": string([]byte{0xff})} {
		t.Run(name, func(t *testing.T) {
			_, err := fetchCachedSubWithTransport(context.Background(), snapshotTestURL, func() error { return nil }, snapshotRoundTripper(func(*http.Request) (*http.Response, error) { return snapshotResponse(200, body), nil }))
			if err == nil {
				t.Fatal("invalid body accepted")
			}
		})
	}
}

func TestExternalSubFetchRedirectsSpendBudgetAndCannotDialPrivate(t *testing.T) {
	for _, target := range []string{"https://127.0.0.1/sub", "http://provider.example/sub", "https://provider.example:8443/sub"} {
		calls, budget := 0, 0
		_, err := fetchCachedSubWithTransport(context.Background(), snapshotTestURL, func() error { budget++; return nil }, snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			response := snapshotResponse(302, "")
			response.Header.Set("Location", target)
			return response, nil
		}))
		if err == nil || calls != 1 || budget != 1 {
			t.Fatalf("redirect allowed: %q calls=%d budget=%d err=%v", target, calls, budget, err)
		}
	}
	calls, budget := 0, 0
	_, err := fetchCachedSubWithTransport(context.Background(), snapshotTestURL, func() error {
		budget++
		if budget > 2 {
			return errSubRequestBudget
		}
		return nil
	}, snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		response := snapshotResponse(302, "")
		response.Header.Set("Location", "https://provider.example/next")
		return response, nil
	}))
	if !errors.Is(err, errSubRequestBudget) || calls != 2 || budget != 3 {
		t.Fatalf("redirect budget: %d %d %v", calls, budget, err)
	}
	_, err = fetchCachedSubWithTransport(context.Background(), snapshotTestURL, func() error { return nil }, snapshotRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("failed " + snapshotTestURL) }))
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("URL-bearing error leaked", err)
	}
}

func TestExternalSubRetryAfter(t *testing.T) {
	now := time.Now().Unix()
	for _, test := range []struct {
		header string
		want   int64
	}{{"120", now + 120}, {"-1", 0}, {"garbage", 0}, {time.Unix(now+300, 0).UTC().Format(http.TimeFormat), now + 300}, {"9223372036854775807", int64(^uint64(0) >> 1)}} {
		if got := externalSubRetryAt(test.header, now); got != test.want {
			t.Errorf("Retry-After %q=%d want %d", test.header, got, test.want)
		}
	}
}
