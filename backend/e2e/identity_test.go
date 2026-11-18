package e2e

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var publicID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestHealth(t *testing.T) {
	need(t)
	b := &browser{t: t}
	resp, body := b.send(http.MethodGet, "/api/health", nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("health: %d %q", resp.StatusCode, body)
	}
}

func TestCookieIsIssuedOnceAndNeverShown(t *testing.T) {
	need(t)
	anon := &browser{t: t}
	resp, body := anon.send(http.MethodGet, "/api/me", nil, nil)
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "pc_uid" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no pc_uid cookie on the first request")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge < 365*24*3600 {
		t.Fatalf("cookie attributes: %+v", cookie)
	}
	if id, err := uuid.Parse(cookie.Value); err != nil || id.Version() != 7 {
		t.Fatalf("cookie %q is not a UUIDv7", cookie.Value)
	}
	var me struct{ ID string }
	mustJSON(t, body, &me)
	if !publicID.MatchString(me.ID) || strings.Contains(me.ID, strings.ReplaceAll(cookie.Value, "-", "")) {
		t.Fatalf("public id %q must be a hash, not the cookie", me.ID)
	}

	// The same browser keeps its id and gets no new cookie.
	known := &browser{t: t, cookie: "pc_uid=" + cookie.Value}
	resp, body = known.send(http.MethodGet, "/api/me", nil, nil)
	if len(resp.Cookies()) != 0 {
		t.Fatalf("a known browser got a new cookie: %v", resp.Cookies())
	}
	var again struct{ ID string }
	mustJSON(t, body, &again)
	if again.ID != me.ID {
		t.Fatalf("id changed: %s → %s", me.ID, again.ID)
	}

	// Another browser is someone else.
	if other := newBrowser(t); other.id == me.ID {
		t.Fatal("two browsers share an id")
	}
}

func TestForgedCookiesAreReplaced(t *testing.T) {
	need(t)
	for _, value := range []string{"nonsense", uuid.NewString() /* v4 */, ""} {
		b := &browser{t: t, cookie: "pc_uid=" + value}
		resp, _ := b.send(http.MethodGet, "/api/me", nil, nil)
		var fresh string
		for _, c := range resp.Cookies() {
			if c.Name == "pc_uid" {
				fresh = c.Value
			}
		}
		if id, err := uuid.Parse(fresh); err != nil || id.Version() != 7 {
			t.Errorf("cookie %q: replaced by %q, want a new UUIDv7", value, fresh)
		}
	}
}

func TestAgentRequestsMakeNoUser(t *testing.T) {
	need(t)
	anon := &browser{t: t}
	resp, _ := anon.send(http.MethodPost, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/mcp without a token: %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate: %q", resp.Header.Get("WWW-Authenticate"))
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("/mcp must not hand out an identity cookie")
	}
}

func TestUnknownRoutes(t *testing.T) {
	b := newBrowser(t)
	if got := b.status(http.MethodGet, "/api/nothing", nil); got != http.StatusNotFound {
		t.Errorf("unknown path: %d", got)
	}
	if got := b.status(http.MethodPut, "/api/projects", nil); got != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: %d", got)
	}
}
