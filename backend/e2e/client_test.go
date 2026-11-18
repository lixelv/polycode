package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// browser is one browser profile: it keeps the identity cookie the server
// gives it on the first request, as a real one would.
type browser struct {
	t      *testing.T
	cookie string // "pc_uid=<uuid>"
	id     string // the public id, from /api/me
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	need(t)
	b := &browser{t: t}
	resp, body := b.send(http.MethodGet, "/api/me", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me: %d %s", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "pc_uid" {
			b.cookie = "pc_uid=" + c.Value
		}
	}
	if b.cookie == "" {
		t.Fatal("/api/me set no pc_uid cookie")
	}
	var me struct{ ID string }
	mustJSON(t, body, &me)
	b.id = me.ID
	return b
}

// send makes one request with the browser's cookie (if it has one yet).
func (b *browser) send(method, path string, body any, header http.Header) (*http.Response, []byte) {
	b.t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			b.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	target := path
	if strings.HasPrefix(path, "/") {
		target = base + path
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		b.t.Fatal(err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	if b.cookie != "" && strings.HasPrefix(target, base) {
		req.Header.Set("Cookie", b.cookie)
	}
	if reader != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	return resp, data
}

// call is send for JSON answers: it checks the status and decodes into out.
func (b *browser) call(method, path string, body any, want int, out any) http.Header {
	b.t.Helper()
	resp, data := b.send(method, path, body, nil)
	if resp.StatusCode != want {
		b.t.Fatalf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, want, data)
	}
	if out != nil {
		mustJSON(b.t, data, out)
	}
	return resp.Header
}

// status is send that only looks at the status.
func (b *browser) status(method, path string, body any) int {
	b.t.Helper()
	resp, _ := b.send(method, path, body, nil)
	return resp.StatusCode
}

func mustJSON(t *testing.T, data []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

// ---- projects ----------------------------------------------------------------------

type project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Public    bool   `json:"public"`
	SizeBytes int64  `json:"sizeBytes"`
	OwnerID   string `json:"ownerId"`
	Mine      bool   `json:"mine"`
}

func (b *browser) createProject(name string) project {
	b.t.Helper()
	var p project
	b.call(http.MethodPost, "/api/projects", map[string]string{"name": name}, http.StatusCreated, &p)
	return p
}

func (b *browser) projects() []project {
	b.t.Helper()
	var out struct{ Projects []project }
	b.call(http.MethodGet, "/api/projects", nil, http.StatusOK, &out)
	return out.Projects
}

type presignOp struct {
	Op   string `json:"op"`
	Key  string `json:"key"`
	Size *int64 `json:"size,omitempty"`
}

func getOp(key string) presignOp { return presignOp{Op: "get", Key: key} }
func delOp(key string) presignOp { return presignOp{Op: "delete", Key: key} }
func putOp(key string, size int) presignOp {
	n := int64(size)
	return presignOp{Op: "put", Key: key, Size: &n}
}

// presign asks for URLs; on 200 it returns them by key.
func (b *browser) presign(id, token string, ops ...presignOp) (int, map[string]string) {
	b.t.Helper()
	resp, data := b.send(http.MethodPost, "/api/projects/"+id+"/presign", map[string]any{"saverToken": token, "ops": ops}, nil)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var out struct {
		URLs []struct{ Key, URL string }
	}
	mustJSON(b.t, data, &out)
	urls := map[string]string{}
	for _, u := range out.URLs {
		urls[u.Key] = u.URL
	}
	return resp.StatusCode, urls
}

// s3 sends one request to a presigned URL, as a browser does, without a cookie.
func s3(t *testing.T, method, url string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// ---- WebSockets --------------------------------------------------------------------

// control is a session's JSON message (see session/protocol.go).
type control struct {
	Type    string          `json:"type"`
	You     *peerInfo       `json:"you,omitempty"`
	Session *sessionInfo    `json:"session,omitempty"`
	Peers   []peerInfo      `json:"peers,omitempty"`
	Peer    *peerInfo       `json:"peer,omitempty"`
	To      uint32          `json:"to,omitempty"`
	From    uint32          `json:"from,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
	Owner   *uint32         `json:"owner,omitempty"`
	Level   string          `json:"level,omitempty"`
	Roles   []string        `json:"roles,omitempty"`
	Ban     bool            `json:"ban,omitempty"`
	Saver   *uint32         `json:"saver,omitempty"`
	Token   string          `json:"token,omitempty"`
}

type peerInfo struct {
	Peer   uint32   `json:"peer"`
	UserID string   `json:"userId"`
	Level  string   `json:"level"`
	Roles  []string `json:"roles"`
	Owner  bool     `json:"owner,omitempty"`
}

type sessionInfo struct {
	ID        string `json:"id"`
	Code      string `json:"code"`
	OwnerID   string `json:"ownerId"`
	AskName   bool   `json:"askName"`
	ProjectID string `json:"projectId"`
}

type relayed struct {
	from    uint32
	payload []byte
}

// socket is one WebSocket connection; frames are kept in arrival order.
type socket struct {
	t      *testing.T
	conn   *websocket.Conn
	text   chan []byte
	binary chan relayed
	closed chan struct{}
}

func wsURL(path string) string { return "ws" + strings.TrimPrefix(base, "http") + path }

// dial opens a WebSocket with the browser's cookie and an Origin header
// (empty: none). It returns the HTTP status when the upgrade is refused.
func (b *browser) dial(path, origin string) (*socket, int) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := http.Header{}
	if b.cookie != "" {
		header.Set("Cookie", b.cookie)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	conn, resp, err := websocket.Dial(ctx, wsURL(path), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if resp != nil {
			return nil, resp.StatusCode
		}
		b.t.Fatalf("dial %s: %v", path, err)
	}
	conn.SetReadLimit(4 << 20)
	s := &socket{t: b.t, conn: conn, text: make(chan []byte, 256), binary: make(chan relayed, 256), closed: make(chan struct{})}
	go func() {
		defer close(s.closed)
		for {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				if len(data) >= 4 {
					s.binary <- relayed{binary.BigEndian.Uint32(data[:4]), data[4:]}
				}
				continue
			}
			s.text <- data
		}
	}()
	b.t.Cleanup(func() { _ = conn.CloseNow() })
	return s, http.StatusSwitchingProtocols
}

// mustDial is dial without an Origin, which must succeed.
func (b *browser) mustDial(path string) *socket {
	b.t.Helper()
	s, status := b.dial(path, "")
	if s == nil {
		b.t.Fatalf("dial %s: %d", path, status)
	}
	return s
}

func (s *socket) sendJSON(v any) {
	s.t.Helper()
	data, _ := json.Marshal(v)
	if err := s.conn.Write(context.Background(), websocket.MessageText, data); err != nil {
		s.t.Fatal(err)
	}
}

// relay sends a binary frame to a peer: [to u32][payload].
func (s *socket) relay(to uint32, payload []byte) {
	s.t.Helper()
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, to)
	copy(frame[4:], payload)
	if err := s.conn.Write(context.Background(), websocket.MessageBinary, frame); err != nil {
		s.t.Fatal(err)
	}
}

const wait = 5 * time.Second

// next waits for a session message of this type, skipping others.
func (s *socket) next(kind string) control {
	s.t.Helper()
	for data := range s.texts() {
		var c control
		if json.Unmarshal(data, &c) == nil && c.Type == kind {
			return c
		}
	}
	s.t.Fatalf("no %q message", kind)
	return control{}
}

// nextRaw is next for messages of other protocols (the tab's socket).
func (s *socket) nextRaw(kind string) map[string]json.RawMessage {
	s.t.Helper()
	for data := range s.texts() {
		var m map[string]json.RawMessage
		if json.Unmarshal(data, &m) == nil && string(m["type"]) == `"`+kind+`"` {
			return m
		}
	}
	s.t.Fatalf("no %q message", kind)
	return nil
}

// texts yields text frames as they arrive, ending after a timeout or once the
// connection is closed and every frame read before the close has been seen.
func (s *socket) texts() iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		timeout := time.After(wait)
		for {
			select {
			case data := <-s.text:
				if !yield(data) {
					return
				}
				continue
			default:
			}
			select {
			case data := <-s.text:
				if !yield(data) {
					return
				}
			case <-s.closed:
				// The reader pushes a frame before it closes: drain what is left.
				for {
					select {
					case data := <-s.text:
						if !yield(data) {
							return
						}
					default:
						return
					}
				}
			case <-timeout:
				return
			}
		}
	}
}

func (s *socket) nextBinary() relayed {
	s.t.Helper()
	select {
	case r := <-s.binary:
		return r
	case <-time.After(wait):
		s.t.Fatal("no relayed frame")
	}
	return relayed{}
}

// noBinary checks that nothing is relayed to this socket for a moment.
func (s *socket) noBinary() {
	s.t.Helper()
	select {
	case r := <-s.binary:
		s.t.Fatalf("unexpected frame from %d: %q", r.from, r.payload)
	case <-time.After(300 * time.Millisecond):
	}
}

// waitClosed waits for the server to close the connection.
func (s *socket) waitClosed() {
	s.t.Helper()
	select {
	case <-s.closed:
	case <-time.After(wait):
		s.t.Fatal("the connection stayed open")
	}
}

func (s *socket) close() {
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
	<-s.closed
}
