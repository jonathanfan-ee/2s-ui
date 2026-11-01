package sub

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shenaba/2s-ui/service"
)

func TestPublicCollectorEndpointRequiresOptInTLSHMACAndActualPeer(t *testing.T) {
	setupSubDB(t)
	secret := "public-collector-fixture-key-at-least-32-bytes"
	t.Setenv("SUI_SUB_COLLECTORS", `{"fixture":{"secret":"`+secret+`","expectedIpv4":"8.8.8.8"}}`)
	t.Setenv("SUI_SUB_COLLECTOR_API", "")
	server := NewServer()
	defer server.Stop()
	router, err := server.initRouter()
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/collector/tasks"
	request := func(peer string, signed, encrypted bool, nonce string) *httptest.ResponseRecorder {
		t.Helper()
		body := "{}"
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.RemoteAddr = peer + ":12345"
		if encrypted {
			req.TLS = &tls.ConnectionState{}
		}
		req.Header.Set("X-Forwarded-For", "8.8.8.8")
		req.Header.Set("CF-Connecting-IP", "8.8.8.8")
		req.Header.Set("Token", "admin-is-not-a-collector")
		if signed {
			stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(service.CollectorSigningText("POST", path, "fixture", stamp, nonce, []byte(body))))
			req.Header.Set("X-Collector-Id", "fixture")
			req.Header.Set("X-Collector-Timestamp", stamp)
			req.Header.Set("X-Collector-Nonce", nonce)
			req.Header.Set("X-Collector-Signature", hex.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := request("8.8.8.8", true, true, "fixture-nonce-012345678900"); w.Code != 404 {
		t.Fatal("public machine API enabled by default")
	}
	t.Setenv("SUI_SUB_COLLECTOR_API", "true")
	router, err = server.initRouter()
	if err != nil {
		t.Fatal(err)
	}
	if w := request("8.8.8.8", true, false, "fixture-nonce-012345678901"); w.Code != 404 {
		t.Fatal("plaintext control request accepted")
	}
	if w := request("8.8.8.8", false, true, ""); w.Code != 401 {
		t.Fatal("admin token bypassed machine HMAC")
	}
	if w := request("9.9.9.9", true, true, "fixture-nonce-012345678902"); w.Code != 401 {
		t.Fatal("spoofed forwarding headers selected collector identity")
	}
	if w := request("8.8.8.8", true, true, "fixture-nonce-012345678903"); w.Code != 200 {
		t.Fatalf("signed TLS collector rejected: %d", w.Code)
	}
	if w := request("8.8.8.8", true, true, "fixture-nonce-012345678903"); w.Code != 401 {
		t.Fatal("replayed signature accepted")
	}
	for _, p := range []string{"/api/clients", "/api/save", "/app/api/clients", "/api/collector/tasks/"} {
		req := httptest.NewRequest("POST", p, strings.NewReader("{}"))
		req.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 404 {
			t.Fatalf("public port acquired unrelated/admin route: %s %d", p, w.Code)
		}
	}
}
