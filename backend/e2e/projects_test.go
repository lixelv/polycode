package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// openRoom joins the owner's tab to its project's room and returns the socket
// and the saver token, if this tab is the saver.
func (b *browser) openRoom(id string) (*socket, control, string) {
	b.t.Helper()
	s := b.mustDial("/api/projects/" + id + "/ws")
	welcome := s.next("welcome")
	saver := s.next("saver")
	return s, welcome, saver.Token
}

func (b *browser) keys(id string) map[string]int64 {
	b.t.Helper()
	var out struct {
		Keys []struct {
			Key  string
			Size int64
		}
	}
	b.call(http.MethodGet, "/api/projects/"+id+"/keys", nil, http.StatusOK, &out)
	keys := map[string]int64{}
	for _, k := range out.Keys {
		keys[k.Key] = k.Size
	}
	return keys
}

// upload puts bytes under a key the way the saver does: a presigned PUT.
func (b *browser) upload(id, token, key string, data []byte) {
	b.t.Helper()
	status, urls := b.presign(id, token, putOp(key, len(data)))
	if status != http.StatusOK {
		b.t.Fatalf("presign put %s: %d", key, status)
	}
	if got, body := s3(b.t, http.MethodPut, urls[key], data); got != http.StatusOK {
		b.t.Fatalf("PUT %s to S3: %d %s", key, got, body)
	}
}

// download reads a key through a presigned GET.
func (b *browser) download(id, key string) (int, []byte) {
	b.t.Helper()
	status, urls := b.presign(id, "", getOp(key))
	if status != http.StatusOK {
		return status, nil
	}
	return s3(b.t, http.MethodGet, urls[key], nil)
}

func TestProjectCRUD(t *testing.T) {
	owner := newBrowser(t)
	p := owner.createProject("Lesson 1")
	if !p.Mine || p.Public || p.OwnerID != owner.id || p.SizeBytes != 0 {
		t.Fatalf("new project: %+v", p)
	}
	if got := owner.status(http.MethodPost, "/api/projects", map[string]string{"name": "Lesson 1"}); got != http.StatusConflict {
		t.Errorf("duplicate name: %d", got)
	}
	if got := owner.status(http.MethodPost, "/api/projects", map[string]string{"name": ""}); got != http.StatusBadRequest {
		t.Errorf("empty name: %d", got)
	}
	if got := owner.status(http.MethodPost, "/api/projects", []byte("{")); got != http.StatusBadRequest {
		t.Errorf("broken JSON: %d", got)
	}
	owner.createProject("Lesson 2")

	var renamed project
	owner.call(http.MethodPatch, "/api/projects/"+p.ID, map[string]any{"name": "Intro", "settings": map[string]bool{"askName": true}}, http.StatusOK, &renamed)
	if renamed.Name != "Intro" {
		t.Fatalf("rename: %+v", renamed)
	}
	if got := owner.status(http.MethodPatch, "/api/projects/"+p.ID, map[string]string{"name": "Lesson 2"}); got != http.StatusConflict {
		t.Errorf("rename onto another project's name: %d", got)
	}
	var names []string
	for _, q := range owner.projects() {
		names = append(names, q.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Intro", "Lesson 2"}) {
		t.Fatalf("list: %v", names)
	}

	// A stranger sees none of it, and cannot tell a private project from none.
	stranger := newBrowser(t)
	if len(stranger.projects()) != 0 {
		t.Fatal("a stranger lists someone's projects")
	}
	for _, path := range []string{"/api/projects/" + p.ID, "/api/projects/00000000-0000-7000-8000-000000000000", "/api/projects/not-a-uuid"} {
		if got := stranger.status(http.MethodGet, path, nil); got != http.StatusNotFound {
			t.Errorf("stranger GET %s: %d", path, got)
		}
	}
	if got := stranger.status(http.MethodPatch, "/api/projects/"+p.ID, map[string]string{"name": "mine"}); got != http.StatusNotFound {
		t.Errorf("stranger rename: %d", got)
	}
	if got := stranger.status(http.MethodDelete, "/api/projects/"+p.ID, nil); got != http.StatusNotFound {
		t.Errorf("stranger delete: %d", got)
	}
	if got := stranger.status(http.MethodGet, "/api/projects/"+p.ID+"/ws", nil); got != http.StatusNotFound {
		t.Errorf("stranger joins the owner's room: %d", got)
	}
}

// The whole saving path: the room names the owner's first tab the saver, only
// its token gets write URLs, the browser talks to S3 itself, and a stranger
// reads what is public once the project is.
func TestSaveThroughTheRoomAndS3ThenShare(t *testing.T) {
	owner := newBrowser(t)
	p := owner.createProject("Shared")
	first, welcome, token := owner.openRoom(p.ID)
	if welcome.You == nil || !welcome.You.Owner || welcome.You.Level != "admin" || token == "" {
		t.Fatalf("owner's welcome %+v, token %q", welcome.You, token)
	}
	if welcome.Session == nil || welcome.Session.ProjectID != p.ID {
		t.Fatalf("the room is not the project's: %+v", welcome.Session)
	}
	second, _, secondToken := owner.openRoom(p.ID)
	if secondToken != "" {
		t.Fatal("only the first tab is the saver")
	}

	structure := gzipped(t, []byte("public structure"))
	private := gzipped(t, []byte("the full structure"))
	if status, _ := owner.presign(p.ID, "", putOp("public/structure.ybin.gz", len(structure))); status != http.StatusForbidden {
		t.Fatalf("a put without the token: %d", status)
	}
	if status, _ := owner.presign(p.ID, "wrong", putOp("public/structure.ybin.gz", len(structure))); status != http.StatusForbidden {
		t.Fatalf("a put with a wrong token: %d", status)
	}
	owner.upload(p.ID, token, "public/structure.ybin.gz", structure)
	owner.upload(p.ID, token, "private/structure.ybin.gz", private)
	hiddenTest := gzipped(t, []byte("def test_sum(): ..."))
	owner.upload(p.ID, token, "hidden/f/hid.ybin.gz", hiddenTest)
	owner.upload(p.ID, token, "hidden/manifest.json.gz", manifestOf(t, []manifestEntry{{Path: "sum.test.py", ID: "hid", Kind: "text", Size: 19}}))

	// S3 holds it to the signed size.
	status, urls := owner.presign(p.ID, token, putOp("public/board.ybin.gz", 10))
	if status != http.StatusOK {
		t.Fatalf("presign: %d", status)
	}
	if got, _ := s3(t, http.MethodPut, urls["public/board.ybin.gz"], make([]byte, 11)); got == http.StatusOK {
		t.Fatal("S3 took a body larger than the signed size")
	}

	keys := owner.keys(p.ID)
	if keys["public/structure.ybin.gz"] != int64(len(structure)) || keys["private/structure.ybin.gz"] != int64(len(private)) {
		t.Fatalf("keys: %v", keys)
	}
	if got, data := owner.download(p.ID, "private/structure.ybin.gz"); got != http.StatusOK || !bytes.Equal(data, private) {
		t.Fatalf("owner reads back: %d %q", got, data)
	}

	// The saver leaves: the other tab takes over with a new token, the old one is dead.
	first.close()
	handover := second.next("saver")
	if handover.Token == "" || handover.Token == token {
		t.Fatalf("handover token %q", handover.Token)
	}
	if status, _ := owner.presign(p.ID, token, delOp("public/board.ybin.gz")); status != http.StatusForbidden {
		t.Fatalf("the old token still writes: %d", status)
	}
	if status, _ := owner.presign(p.ID, handover.Token, delOp("public/board.ybin.gz")); status != http.StatusOK {
		t.Fatalf("the new token: %d", status)
	}

	// Private until made public.
	stranger := newBrowser(t)
	if got, _ := stranger.download(p.ID, "public/structure.ybin.gz"); got != http.StatusNotFound {
		t.Fatalf("stranger reads a private project: %d", got)
	}
	if got := stranger.status(http.MethodPost, "/api/projects/"+p.ID+"/visibility", map[string]bool{"public": true}); got != http.StatusNotFound {
		t.Fatalf("stranger makes it public: %d", got)
	}
	var shared project
	owner.call(http.MethodPost, "/api/projects/"+p.ID+"/visibility", map[string]bool{"public": true}, http.StatusOK, &shared)
	if !shared.Public {
		t.Fatal("not public")
	}

	var seen project
	stranger.call(http.MethodGet, "/api/projects/"+p.ID, nil, http.StatusOK, &seen)
	if seen.Mine || seen.OwnerID != owner.id || seen.Name != "Shared" {
		t.Fatalf("stranger's view: %+v", seen)
	}
	if got, data := stranger.download(p.ID, "public/structure.ybin.gz"); got != http.StatusOK || !bytes.Equal(data, structure) {
		t.Fatalf("stranger reads the public part: %d %q", got, data)
	}
	for _, key := range []string{"private/structure.ybin.gz", "hidden/manifest.json.gz", "hidden/f/hid.ybin.gz"} {
		if got, _ := stranger.download(p.ID, key); got != http.StatusNotFound {
			t.Errorf("stranger reads %s: %d", key, got)
		}
	}
	if status, _ := stranger.presign(p.ID, handover.Token, putOp("public/structure.ybin.gz", 1)); status != http.StatusNotFound {
		t.Fatalf("stranger writes with a stolen token: %d", status)
	}
	if got := stranger.status(http.MethodGet, "/api/projects/"+p.ID+"/keys", nil); got != http.StatusNotFound {
		t.Fatalf("stranger lists the keys: %d", got)
	}

	// A copy is the stranger's own, with the public part and nothing private.
	var copied project
	stranger.call(http.MethodPost, "/api/projects/"+p.ID+"/copy", nil, http.StatusCreated, &copied)
	if !copied.Mine || copied.ID == p.ID || copied.Public {
		t.Fatalf("copy: %+v", copied)
	}
	copyKeys := stranger.keys(copied.ID)
	if _, ok := copyKeys["public/structure.ybin.gz"]; !ok {
		t.Fatalf("the copy has no public structure: %v", copyKeys)
	}
	for key := range copyKeys {
		if key == "private/structure.ybin.gz" {
			t.Fatalf("the copy took the private structure: %v", copyKeys)
		}
	}
	if got, data := stranger.download(copied.ID, "public/structure.ybin.gz"); got != http.StatusOK || !bytes.Equal(data, structure) {
		t.Fatalf("the copy reads back: %d %q", got, data)
	}
	// The hidden test comes along sealed: its new owner may read it (to run
	// checks) but never write it.
	if got, data := stranger.download(copied.ID, "sealed/f/hid.ybin.gz"); got != http.StatusOK || !bytes.Equal(data, hiddenTest) {
		t.Fatalf("the sealed test: %d %q", got, data)
	}
	if _, ok := copyKeys["sealed/manifest.json.gz"]; !ok {
		t.Fatalf("the copy has no sealed manifest: %v", copyKeys)
	}
	_, _, copyToken := stranger.openRoom(copied.ID)
	if status, _ := stranger.presign(copied.ID, copyToken, putOp("sealed/f/hid.ybin.gz", 3)); status != http.StatusForbidden {
		t.Fatalf("the copy's owner rewrites the sealed test: %d", status)
	}
}

func TestDeleteEndsTheRoomAndTheObjects(t *testing.T) {
	owner := newBrowser(t)
	p := owner.createProject("Doomed")
	tab, _, token := owner.openRoom(p.ID)
	data := gzipped(t, []byte("x"))
	owner.upload(p.ID, token, "public/structure.ybin.gz", data)
	owner.call(http.MethodPost, "/api/projects/"+p.ID+"/visibility", map[string]bool{"public": true}, http.StatusOK, nil)
	_, urls := owner.presign(p.ID, "", getOp("public/structure.ybin.gz"))

	stranger := newBrowser(t)
	stranger.call(http.MethodGet, "/api/projects/"+p.ID, nil, http.StatusOK, nil)

	owner.call(http.MethodDelete, "/api/projects/"+p.ID, nil, http.StatusNoContent, nil)
	tab.next("ended")
	tab.waitClosed()

	// Only the owner hears that it was deleted, not just missing.
	resp, _ := owner.send(http.MethodGet, "/api/projects/"+p.ID, nil, nil)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Project-Deleted") != "1" {
		t.Fatalf("owner after delete: %d, X-Project-Deleted %q", resp.StatusCode, resp.Header.Get("X-Project-Deleted"))
	}
	resp, _ = stranger.send(http.MethodGet, "/api/projects/"+p.ID, nil, nil)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Project-Deleted") != "" {
		t.Fatalf("stranger after delete: %d, X-Project-Deleted %q", resp.StatusCode, resp.Header.Get("X-Project-Deleted"))
	}
	if len(owner.projects()) != 0 {
		t.Fatal("still listed")
	}
	if got, _ := s3(t, http.MethodGet, urls["public/structure.ybin.gz"], nil); got == http.StatusOK {
		t.Fatal("the object is still in S3")
	}
	if got := owner.status(http.MethodGet, "/api/projects/"+p.ID+"/ws", nil); got != http.StatusNotFound {
		t.Fatalf("room of a deleted project: %d", got)
	}
}

func TestProjectQuota(t *testing.T) {
	owner := newBrowser(t)
	p := owner.createProject("Big")
	_, _, token := owner.openRoom(p.ID)
	const mb = 1 << 20
	if status, _ := owner.presign(p.ID, token, putOp("public/b/"+hex64('a')+".gz", 40*mb), putOp("public/b/"+hex64('b')+".gz", 25*mb)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("65 MB at once: %d", status)
	}
	if status, _ := owner.presign(p.ID, token, putOp("public/b/"+hex64('a')+".gz", 40*mb)); status != http.StatusOK {
		t.Fatalf("40 MB: %d", status)
	}
	// The URL is out and may still be used: it counts.
	if status, _ := owner.presign(p.ID, token, putOp("public/b/"+hex64('b')+".gz", 25*mb)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("40 + 25 MB: %d", status)
	}
	var got project
	owner.call(http.MethodGet, "/api/projects/"+p.ID, nil, http.StatusOK, &got)
	if got.SizeBytes != 40*mb {
		t.Fatalf("sizeBytes %d", got.SizeBytes)
	}
}

func TestPresignRejectsBadRequests(t *testing.T) {
	owner := newBrowser(t)
	p := owner.createProject("Keys")
	_, _, token := owner.openRoom(p.ID)
	bad := [][]presignOp{
		{getOp("../other/public/structure.ybin.gz")},
		{getOp("public/anything.txt")},
		{{Op: "post", Key: "public/structure.ybin.gz"}},
		{{Op: "put", Key: "public/structure.ybin.gz"}}, // no size
	}
	for _, ops := range bad {
		if status, _ := owner.presign(p.ID, token, ops...); status != http.StatusBadRequest {
			t.Errorf("%+v: %d", ops, status)
		}
	}
	many := make([]presignOp, 101)
	for i := range many {
		many[i] = getOp("public/structure.ybin.gz")
	}
	if status, _ := owner.presign(p.ID, token, many...); status != http.StatusBadRequest {
		t.Errorf("101 operations: %d", status)
	}
	if status, _ := owner.presign(p.ID, token, putOp("sealed/manifest.json.gz", 10)); status != http.StatusForbidden {
		t.Errorf("writing sealed/: %d", status)
	}
}

func hex64(c byte) string { return string(bytes.Repeat([]byte{c}, 64)) }

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(data)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type manifestEntry struct {
	Path string `json:"path"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

// manifestOf builds a hidden manifest as the browser writes it.
func manifestOf(t *testing.T, entries any) []byte {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return gzipped(t, data)
}
