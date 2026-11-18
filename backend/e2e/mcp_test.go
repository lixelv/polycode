package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// agent speaks JSON-RPC to /mcp with a bearer token, as an MCP client does.
type agent struct {
	t       *testing.T
	token   string
	session string
}

func (b *browser) issueToken() string {
	b.t.Helper()
	var out struct{ Token string }
	b.call(http.MethodPost, "/api/mcp/token", nil, http.StatusOK, &out)
	if len(out.Token) != 64 {
		b.t.Fatalf("token %q", out.Token)
	}
	return out.Token
}

type rpcAnswer struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *agent) post(origin string, body any) (*http.Response, []byte) {
	a.t.Helper()
	header := http.Header{}
	if a.token != "" {
		header.Set("Authorization", "Bearer "+a.token)
	}
	if a.session != "" {
		header.Set("Mcp-Session-Id", a.session)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	anon := &browser{t: a.t} // agents carry no cookie
	return anon.send(http.MethodPost, "/mcp", body, header)
}

func (a *agent) rpc(method string, params any) rpcAnswer {
	a.t.Helper()
	resp, data := a.post("", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if resp.StatusCode != http.StatusOK {
		a.t.Fatalf("%s: %d %s", method, resp.StatusCode, data)
	}
	if method == "initialize" {
		a.session = resp.Header.Get("Mcp-Session-Id")
	}
	var answer rpcAnswer
	mustJSON(a.t, data, &answer)
	return answer
}

type toolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (r toolResult) text() string {
	var parts []string
	for _, c := range r.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

func (a *agent) tool(name string, args map[string]any) toolResult {
	a.t.Helper()
	answer := a.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if answer.Error != nil {
		a.t.Fatalf("%s: rpc error %+v", name, answer.Error)
	}
	var r toolResult
	mustJSON(a.t, answer.Result, &r)
	return r
}

func (a *agent) mustTool(name string, args map[string]any) toolResult {
	a.t.Helper()
	r := a.tool(name, args)
	if r.IsError {
		a.t.Fatalf("%s: %s", name, r.text())
	}
	return r
}

// The agent's whole path: the page issues a token, the agent creates a
// project the page then lists, opens it in the user's /mcp-driven tab, runs a
// tool there, and loses everything when the page reissues the token.
func TestAgentDrivesAUsersTab(t *testing.T) {
	user := newBrowser(t)
	var status struct{ Exists bool }
	user.call(http.MethodGet, "/api/mcp/token", nil, http.StatusOK, &status)
	if status.Exists {
		t.Fatal("a new user has a token")
	}
	a := &agent{t: t, token: user.issueToken()}
	user.call(http.MethodGet, "/api/mcp/token", nil, http.StatusOK, &status)
	if !status.Exists {
		t.Fatal("the token is not recorded")
	}

	var init struct {
		ProtocolVersion string
		ServerInfo      struct{ Name string }
		Instructions    string
	}
	answer := a.rpc("initialize", map[string]any{"protocolVersion": "2025-03-26"})
	mustJSON(t, answer.Result, &init)
	if a.session == "" || init.ProtocolVersion != "2025-03-26" || init.ServerInfo.Name != "polycode" || init.Instructions == "" {
		t.Fatalf("initialize: session %q, %+v", a.session, init)
	}
	if resp, _ := a.post("", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a notification: %d", resp.StatusCode)
	}
	if answer := a.rpc("no/such", nil); answer.Error == nil || answer.Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", answer.Error)
	}
	var tools struct {
		Tools []struct{ Name string }
	}
	mustJSON(t, a.rpc("tools/list", nil).Result, &tools)
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"polycode_guide", "polycode_tests_guide", "list_tabs", "open_project", "read_file", "write_file"} {
		if !names[want] {
			t.Errorf("tools/list has no %s", want)
		}
	}
	if guide := a.mustTool("polycode_guide", nil); !strings.Contains(guide.text(), "polycode") {
		t.Fatalf("guide: %.200s", guide.text())
	}

	// Without a tab a browser tool cannot run.
	if r := a.tool("read_file", map[string]any{"path": "main.py"}); !r.IsError {
		t.Fatalf("read_file without a tab: %s", r.text())
	}

	// The agent's project is the user's: the page lists it.
	var created struct{ ID, Name string }
	mustJSON(t, []byte(a.mustTool("create_project", map[string]any{"project": "From the agent"}).Content[0].Text), &created)
	var listed bool
	for _, p := range user.projects() {
		listed = listed || (p.ID == created.ID && p.Name == "From the agent")
	}
	if !listed {
		t.Fatal("the page does not list the agent's project")
	}

	// The user opens a /mcp-driven tab; the agent sees it and opens the project there.
	tab := user.mustDial("/api/mcp/tabs/ws?tab=lesson-tab")
	tabs := a.mustTool("list_tabs", nil).text()
	if !strings.Contains(tabs, `"tab_id":"lesson-tab"`) {
		t.Fatalf("list_tabs: %s", tabs)
	}
	opened := make(chan toolResult, 1)
	go func() {
		opened <- a.tool("open_project", map[string]any{"project": "From the agent", "tab_id": "lesson-tab"})
	}()
	open := tab.nextRaw("open")
	if string(open["project"]) != `"`+created.ID+`"` {
		t.Fatalf("open frame: %s", open["project"])
	}
	// The page reloads into the project and says so.
	tab.sendJSON(map[string]any{"type": "claim", "project": created.ID})
	if claimed := tab.nextRaw("claimed"); string(claimed["ok"]) != "true" {
		t.Fatalf("claim: %v", claimed)
	}
	tab.sendJSON(map[string]any{"type": "ready", "project": created.ID})
	select {
	case r := <-opened:
		if r.IsError || !strings.Contains(r.text(), created.ID) {
			t.Fatalf("open_project: %s", r.text())
		}
	case <-time.After(wait):
		t.Fatal("open_project did not return")
	}

	// A browser tool goes to the tab, and its answer comes back stamped.
	result := make(chan toolResult, 1)
	go func() { result <- a.tool("read_file", map[string]any{"path": "main.py"}) }()
	call := tab.nextRaw("call")
	if string(call["name"]) != `"read_file"` || !strings.Contains(string(call["args"]), `"main.py"`) {
		t.Fatalf("call frame: %v", call)
	}
	var id uint64
	_ = json.Unmarshal(call["id"], &id)
	tab.sendJSON(map[string]any{"type": "result", "id": id, "data": map[string]any{
		"content": []map[string]string{{"type": "text", "text": "print('hi')"}},
	}})
	select {
	case r := <-result:
		if r.IsError || len(r.Content) != 2 || r.Content[0].Text != "print('hi')" {
			t.Fatalf("read_file: %+v", r)
		}
		if !strings.Contains(r.Content[1].Text, `"tab_id":"lesson-tab"`) || !strings.Contains(r.Content[1].Text, created.ID) {
			t.Fatalf("stamp: %s", r.Content[1].Text)
		}
	case <-time.After(wait):
		t.Fatal("read_file did not return")
	}

	// Another user's agent sees none of it.
	other := &agent{t: t, token: newBrowser(t).issueToken()}
	other.rpc("initialize", nil)
	if tabs := other.mustTool("list_tabs", nil).text(); strings.Contains(tabs, "lesson-tab") {
		t.Fatalf("another user sees the tab: %s", tabs)
	}
	if r := other.tool("open_project", map[string]any{"project": created.ID, "tab_id": "lesson-tab"}); !r.IsError {
		t.Fatalf("another user opens the project: %s", r.text())
	}

	// Deleting through the agent goes through the projects API: the tab is
	// left without a project and the page no longer lists it.
	a.mustTool("delete_project", map[string]any{"project": created.ID})
	tab.nextRaw("none")
	for _, p := range user.projects() {
		if p.ID == created.ID {
			t.Fatal("the deleted project is still listed")
		}
	}

	// A new token kills the old one.
	fresh := user.issueToken()
	if resp, _ := a.post("", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old token after reissue: %d", resp.StatusCode)
	}
	a.token = fresh
	a.rpc("ping", nil)
}

func TestMCPRefusesBrowsersFromOtherSites(t *testing.T) {
	a := &agent{t: t, token: newBrowser(t).issueToken()}
	ping := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}
	if resp, _ := a.post("http://evil.example", ping); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a foreign origin: %d", resp.StatusCode)
	}
	if resp, _ := a.post(allowedOrigin, ping); resp.StatusCode != http.StatusOK {
		t.Fatalf("the allowed dev origin: %d", resp.StatusCode)
	}
	if resp, _ := a.post("", []byte("not json")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage: %d", resp.StatusCode)
	}
	a.token = strings.Repeat("0", 64)
	if resp, _ := a.post("", ping); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a made-up token: %d", resp.StatusCode)
	}
}
