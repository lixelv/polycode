package e2e

import (
	"net/http"
	"net/url"
	"testing"
)

// The proxy is reached only by a user's own Python; it must never become a
// way into the server's network. Only refusals are tested: they need no
// internet.
func TestProxyRefusesLocalTargets(t *testing.T) {
	b := newBrowser(t)
	cases := map[string]int{
		"":                                http.StatusBadRequest,
		"ftp://example.com/":              http.StatusBadRequest,
		"/relative":                       http.StatusBadRequest,
		"http://127.0.0.1/":               http.StatusForbidden,
		"http://[::1]:8090/api/health":    http.StatusForbidden,
		"http://10.0.0.1/":                http.StatusForbidden,
		"http://169.254.169.254/latest/":  http.StatusForbidden,
		"http://192.168.1.1/":             http.StatusForbidden,
		"http://localhost:5432/":          http.StatusForbidden,
		"http://db.internal/":             http.StatusForbidden,
		"http://printer.local/":           http.StatusForbidden,
		"https://app.localhost/":          http.StatusForbidden,
		"http://" + base[len("http://"):]: http.StatusForbidden, // this very server
	}
	for target, want := range cases {
		if got := b.status(http.MethodGet, "/api/proxy?url="+url.QueryEscape(target), nil); got != want {
			t.Errorf("%q: %d, want %d", target, got, want)
		}
	}
}

func TestProxyRateLimitIsPerUser(t *testing.T) {
	b := newBrowser(t)
	path := "/api/proxy?url=" + url.QueryEscape("http://127.0.0.1/")
	for i := 0; i < 256; i++ {
		if got := b.status(http.MethodGet, path, nil); got != http.StatusForbidden {
			t.Fatalf("request %d: %d", i+1, got)
		}
	}
	resp, _ := b.send(http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("257th request: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	// Someone else is not affected.
	if got := newBrowser(t).status(http.MethodGet, path, nil); got != http.StatusForbidden {
		t.Fatalf("another user: %d", got)
	}
}
