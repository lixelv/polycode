package e2e

import (
	"bytes"
	"net/http"
	"slices"
	"testing"
)

type code struct {
	Role  string `json:"role"`
	Level string `json:"level"`
	Code  string `json:"code"`
}

type sessionAnswer struct {
	Session sessionInfo `json:"session"`
	Level   string      `json:"level"`
	Roles   []string    `json:"roles"`
	Owner   bool        `json:"owner"`
	Codes   []code      `json:"codes"`
}

func codeFor(t *testing.T, codes []code, role string) string {
	t.Helper()
	for _, c := range codes {
		if c.Role == role {
			return c.Code
		}
	}
	t.Fatalf("no code for %q in %+v", role, codes)
	return ""
}

func (b *browser) join(code string) (*socket, control) {
	b.t.Helper()
	s := b.mustDial("/api/sessions/" + code + "/ws")
	return s, s.next("welcome")
}

// A lesson: the teacher shares a project, students join by code, data flows
// through the relay, roles change and stick, a troublemaker is banned and
// ending the session sends the guests away while the teacher's room stays.
func TestLiveSessionOfAProject(t *testing.T) {
	teacher := newBrowser(t)
	p := teacher.createProject("Lesson")
	room, _, _ := teacher.openRoom(p.ID)

	var share sessionAnswer
	teacher.call(http.MethodPost, "/api/projects/"+p.ID+"/session",
		map[string]any{"roles": []map[string]string{{"name": "group-a", "level": "write"}}}, http.StatusOK, &share)
	if !share.Owner || share.Level != "admin" || share.Session.ProjectID != p.ID {
		t.Fatalf("share: %+v", share)
	}
	writeCode := codeFor(t, share.Codes, "write")
	readCode := codeFor(t, share.Codes, "read")
	groupCode := codeFor(t, share.Codes, "group-a")
	if got := newBrowser(t).status(http.MethodPost, "/api/projects/"+p.ID+"/session", nil); got != http.StatusNotFound {
		t.Fatalf("a stranger shares the project: %d", got)
	}

	// A student looks the code up, then joins.
	alice := newBrowser(t)
	var lookup sessionAnswer
	alice.call(http.MethodGet, "/api/sessions/"+groupCode, nil, http.StatusOK, &lookup)
	if lookup.Owner || lookup.Level != "write" || !slices.Equal(lookup.Roles, []string{"group-a"}) {
		t.Fatalf("lookup: %+v", lookup)
	}
	if got := alice.status(http.MethodGet, "/api/sessions/NOSUCHCODE", nil); got != http.StatusNotFound {
		t.Fatalf("unknown code: %d", got)
	}
	aliceWS, welcome := alice.join(groupCode)
	me := welcome.You
	if me == nil || me.Level != "write" || !slices.Equal(me.Roles, []string{"group-a"}) || me.Owner || me.UserID != alice.id {
		t.Fatalf("alice's welcome: %+v", me)
	}
	if welcome.Session.Code != groupCode {
		t.Fatalf("alice is told the code %q, not her own", welcome.Session.Code)
	}
	if len(welcome.Peers) != 1 || !welcome.Peers[0].Owner || welcome.Peers[0].UserID != teacher.id {
		t.Fatalf("alice's peers: %+v", welcome.Peers)
	}
	teacherPeer := welcome.Peers[0].Peer
	if welcome.Owner == nil || *welcome.Owner != teacherPeer {
		t.Fatalf("acting owner %v, want the teacher %d", welcome.Owner, teacherPeer)
	}
	joined := room.next("join")
	if joined.Peer.Peer != me.Peer || joined.Peer.UserID != alice.id {
		t.Fatalf("teacher hears %+v", joined.Peer)
	}
	if saver := aliceWS.next("saver"); saver.Token != "" || saver.Saver == nil || *saver.Saver != teacherPeer {
		t.Fatalf("a guest learns the saver token or a wrong saver: %+v", saver)
	}

	// Relay both ways, stamped with the sender; nobody can spoof or loop.
	aliceWS.relay(teacherPeer, []byte("yjs update"))
	if got := room.nextBinary(); got.from != me.Peer || !bytes.Equal(got.payload, []byte("yjs update")) {
		t.Fatalf("relayed to the teacher: %+v", got)
	}
	room.relay(me.Peer, []byte("sync step 2"))
	if got := aliceWS.nextBinary(); got.from != teacherPeer || string(got.payload) != "sync step 2" {
		t.Fatalf("relayed to alice: %+v", got)
	}
	aliceWS.relay(me.Peer, []byte("to myself"))
	aliceWS.relay(9999, []byte("to nobody"))
	aliceWS.noBinary()
	room.noBinary()

	// WebRTC signaling goes to one peer, stamped with the sender.
	room.sendJSON(map[string]any{"type": "signal", "to": me.Peer, "data": map[string]string{"sdp": "offer"}})
	if sig := aliceWS.next("signal"); sig.From != teacherPeer || string(sig.Data) != `{"sdp":"offer"}` {
		t.Fatalf("signal: %+v %s", sig, sig.Data)
	}

	// A student may not reset the project, see the codes or add roles.
	aliceWS.sendJSON(map[string]string{"type": "reset"})
	if e := aliceWS.next("error"); e.Error == "" {
		t.Fatal("reset by a student is refused without a reason")
	}
	if got := alice.status(http.MethodGet, "/api/sessions/"+groupCode+"/codes", nil); got != http.StatusForbidden {
		t.Fatalf("a student reads the codes: %d", got)
	}
	if got := alice.status(http.MethodPost, "/api/sessions/"+groupCode+"/roles", map[string]string{"name": "x", "level": "write"}); got != http.StatusForbidden {
		t.Fatalf("a student adds a role: %d", got)
	}
	room.sendJSON(map[string]string{"type": "reset"})
	if r := aliceWS.next("reset"); r.From != teacherPeer {
		t.Fatalf("reset from %d", r.From)
	}

	// The teacher adds a role, then makes alice a reader of it. Everyone hears.
	var added code
	teacher.call(http.MethodPost, "/api/sessions/"+writeCode+"/roles", map[string]string{"name": "group-b", "level": "read"}, http.StatusOK, &added)
	if added.Role != "group-b" || added.Code == "" {
		t.Fatalf("added role: %+v", added)
	}
	room.sendJSON(map[string]any{"type": "setRole", "to": me.Peer, "level": "read", "roles": []string{"group-b"}})
	for _, s := range []*socket{room, aliceWS} {
		if r := s.next("role"); r.Peer.Peer != me.Peer || r.Peer.Level != "read" || !slices.Equal(r.Peer.Roles, []string{"group-b"}) {
			t.Fatalf("role change: %+v", r.Peer)
		}
	}
	room.sendJSON(map[string]any{"type": "setRole", "to": me.Peer, "level": "write", "roles": []string{"no-such-role"}})
	if e := room.next("error"); e.Error == "" {
		t.Fatal("an unknown role is refused without a reason")
	}
	aliceWS.sendJSON(map[string]any{"type": "setRole", "to": teacherPeer, "level": "read"})
	if e := aliceWS.next("error"); e.Error == "" {
		t.Fatal("a student changing the teacher's role is refused without a reason")
	}

	// The given role wins over the join code when she comes back.
	aliceWS.close()
	room.next("leave")
	aliceWS, welcome = alice.join(groupCode)
	if welcome.You.Level != "read" || !slices.Equal(welcome.You.Roles, []string{"group-b"}) {
		t.Fatalf("after reconnecting: %+v", welcome.You)
	}
	me = welcome.You
	room.next("join")

	// A troublemaker is banned: removed at once, refused from then on.
	bob := newBrowser(t)
	bobWS, bobWelcome := bob.join(readCode)
	room.next("join")
	room.sendJSON(map[string]any{"type": "kick", "to": bobWelcome.You.Peer, "ban": true})
	if r := bobWS.next("removed"); !r.Ban {
		t.Fatal("bob is not told he is banned")
	}
	bobWS.waitClosed()
	if l := room.next("leave"); l.Peer.Peer != bobWelcome.You.Peer {
		t.Fatalf("leave of %d", l.Peer.Peer)
	}
	bobWS = bob.mustDial("/api/sessions/" + writeCode + "/ws")
	if r := bobWS.next("removed"); !r.Ban {
		t.Fatal("a banned user gets back in with another code")
	}
	bobWS.waitClosed()

	// The teacher sees the codes; ending the session sends the guests away
	// and revokes the codes, but the teacher's room goes on.
	var codes struct{ Codes []code }
	teacher.call(http.MethodGet, "/api/sessions/"+writeCode+"/codes", nil, http.StatusOK, &codes)
	if len(codes.Codes) < 5 {
		t.Fatalf("codes: %+v", codes.Codes)
	}
	if got := alice.status(http.MethodPost, "/api/sessions/"+writeCode+"/end", nil); got != http.StatusForbidden {
		t.Fatalf("a student ends the session: %d", got)
	}
	teacher.call(http.MethodPost, "/api/sessions/"+writeCode+"/end", nil, http.StatusNoContent, nil)
	if r := aliceWS.next("removed"); r.Ban {
		t.Fatal("ending the session is not a ban")
	}
	aliceWS.waitClosed()
	if l := room.next("leave"); l.Peer.Peer != me.Peer {
		t.Fatalf("leave of %d", l.Peer.Peer)
	}
	for _, c := range []string{writeCode, readCode, groupCode, added.Code} {
		if got := alice.status(http.MethodGet, "/api/sessions/"+c, nil); got != http.StatusNotFound {
			t.Errorf("revoked code %s: %d", c, got)
		}
	}
	room.sendJSON(map[string]string{"type": "reset"})
	select {
	case <-room.closed:
		t.Fatal("the teacher's room closed with the session")
	default:
	}

	// Share again: new codes into the same room.
	teacher.call(http.MethodPost, "/api/projects/"+p.ID+"/session", nil, http.StatusOK, &share)
	newCode := codeFor(t, share.Codes, "write")
	if newCode == writeCode {
		t.Fatal("the old code came back")
	}
	carol := newBrowser(t)
	_, cw := carol.join(newCode)
	if len(cw.Peers) != 1 || cw.Peers[0].Peer != teacherPeer {
		t.Fatalf("carol's peers: %+v", cw.Peers)
	}
}

// A session without a project: an admin stands in for the owner while the
// owner is away, and ending it closes everyone and the codes for good.
func TestStandaloneSession(t *testing.T) {
	owner := newBrowser(t)
	var created sessionAnswer
	owner.call(http.MethodPost, "/api/sessions", map[string]any{"askName": true}, http.StatusCreated, &created)
	if !created.Session.AskName || created.Session.ProjectID != "" || !created.Owner {
		t.Fatalf("created: %+v", created)
	}
	if got := owner.status(http.MethodPost, "/api/sessions", map[string]any{"roles": []map[string]string{{"name": "admin", "level": "write"}}}); got != http.StatusBadRequest {
		t.Fatalf("a built-in role name as a custom role: %d", got)
	}
	adminCode := codeFor(t, created.Codes, "admin")
	writeCode := codeFor(t, created.Codes, "write")

	ownerWS, ow := owner.join(writeCode)
	if !ow.You.Owner || ow.You.Level != "admin" {
		t.Fatalf("the owner joining with the write code: %+v", ow.You)
	}

	helper := newBrowser(t)
	helperWS, hw := helper.join(adminCode)
	if hw.You.Level != "admin" {
		t.Fatalf("helper: %+v", hw.You)
	}
	// A connected admin may read the codes.
	helper.call(http.MethodGet, "/api/sessions/"+adminCode+"/codes", nil, http.StatusOK, nil)

	student := newBrowser(t)
	studentWS, sw := student.join(writeCode)
	if *sw.Owner != ow.You.Peer {
		t.Fatalf("acting owner %d, want the owner %d", *sw.Owner, ow.You.Peer)
	}

	// An admin cannot make admins.
	helperWS.sendJSON(map[string]any{"type": "setRole", "to": sw.You.Peer, "level": "admin"})
	if e := helperWS.next("error"); e.Error == "" {
		t.Fatal("refused without a reason")
	}

	// The owner leaves: the admin stands in, and may reset.
	ownerWS.close()
	for _, s := range []*socket{helperWS, studentWS} {
		if o := s.next("owner"); *o.Owner != hw.You.Peer {
			t.Fatalf("acting owner %d, want the helper %d", *o.Owner, hw.You.Peer)
		}
	}
	helperWS.sendJSON(map[string]string{"type": "reset"})
	studentWS.next("reset")

	// The owner comes back and takes over again.
	ownerWS, ow = owner.join(writeCode)
	if *ow.Owner != ow.You.Peer {
		t.Fatalf("acting owner after return: %d", *ow.Owner)
	}
	if o := studentWS.next("owner"); *o.Owner != ow.You.Peer {
		t.Fatalf("student hears acting owner %d", *o.Owner)
	}

	owner.call(http.MethodPost, "/api/sessions/"+writeCode+"/end", nil, http.StatusNoContent, nil)
	for _, s := range []*socket{ownerWS, helperWS, studentWS} {
		s.next("ended")
		s.waitClosed()
	}
	if got := student.status(http.MethodGet, "/api/sessions/"+writeCode+"/ws", nil); got != http.StatusGone && got != http.StatusNotFound {
		t.Fatalf("joining an ended session: %d", got)
	}
}

// Browsers send Origin: another site must not open a socket as the user.
func TestWebSocketOrigins(t *testing.T) {
	owner := newBrowser(t)
	p := owner.createProject("Origins")
	path := "/api/projects/" + p.ID + "/ws"
	if s, status := owner.dial(path, "http://evil.example"); s != nil || status != http.StatusForbidden {
		t.Fatalf("a foreign origin: %d", status)
	}
	if s, status := owner.dial(path, allowedOrigin); s == nil {
		t.Fatalf("the allowed dev origin: %d", status)
	}
	if s, status := owner.dial(path, base); s == nil {
		t.Fatalf("the server's own origin: %d", status)
	}
	if s, status := owner.dial("/api/mcp/tabs/ws?tab=t1", "http://evil.example"); s != nil || status != http.StatusForbidden {
		t.Fatalf("a foreign origin on the tab socket: %d", status)
	}
}
