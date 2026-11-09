package scrapers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseDropboxLinks(t *testing.T) {
	for s, want := range map[string]string{
		"https://www.dropbox.com/scl/fo/abc123/AKey-9?rlkey=rk1&st=xyz&dl=0":  "scl/fo/abc123/AKey-9||rk1",
		"https://dropbox.com/scl/fo/abc123/AKey-9/Sub%20Dir/?rlkey=rk1":        "scl/fo/abc123/AKey-9|/Sub Dir|rk1",
		"http://www.dropbox.com/sh/oldid1/AAKey_x?dl=0":                       "sh/oldid1/AAKey_x||",
		"https://www.dropbox.com/sh/oldid1/AAKey_x/inner/more?dl=0#frag":      "sh/oldid1/AAKey_x|/inner/more|",
	} {
		base, sub, rk, err := parseDropboxFolderLink(s)
		if err != nil || base+"|"+sub+"|"+rk != want {
			t.Errorf("folder %q: %q %q %q %v, want %s", s, base, sub, rk, err, want)
		}
		if _, _, err := parseDropboxFileLink(s); err == nil {
			t.Errorf("%q is a folder, not a file", s)
		}
		if _, err := (dropboxProvider{}).ID(s); err != nil {
			t.Errorf("%q should be a folder link: %v", s, err)
		}
	}
	for s, want := range map[string]string{
		"https://www.dropbox.com/scl/fi/fileid1/Mod.zipmod?rlkey=rk2&dl=0": "scl/fi/fileid1/Mod.zipmod|rk2",
		"https://www.dropbox.com/s/oldfile9/Mod%20x.zipmod?dl=1":            "s/oldfile9/Mod%20x.zipmod|",
	} {
		path, rk, err := parseDropboxFileLink(s)
		if err != nil || path+"|"+rk != want {
			t.Errorf("file %q: %q %q %v, want %s", s, path, rk, err, want)
		}
		if _, _, _, err := parseDropboxFolderLink(s); err == nil {
			t.Errorf("%q is a file, not a folder", s)
		}
		if _, err := (dropboxProvider{}).FileID(s); err != nil {
			t.Errorf("%q should be a file link: %v", s, err)
		}
	}
	for _, s := range []string{"", "https://www.dropbox.com/", "https://www.dropbox.com/home", "https://example.com/sh/abc/def", "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg"} {
		if _, _, _, err := parseDropboxFolderLink(s); err == nil {
			t.Errorf("%q should be rejected as a folder", s)
		}
		if _, _, err := parseDropboxFileLink(s); err == nil {
			t.Errorf("%q should be rejected as a file", s)
		}
	}
}

func TestFindDropboxLinks(t *testing.T) {
	got := findDropboxLinks(`Folder (https://www.dropbox.com/scl/fo/abc123/AKey-9?rlkey=rk1&amp;st=q&amp;dl=0), old one https://www.dropbox.com/sh/oldid1/AAKey_x?dl=0.
	File: https://www.dropbox.com/scl/fi/fileid1/Mod.zipmod?rlkey=rk2&dl=0, again https://www.dropbox.com/s/oldfile9/Mod.zipmod?dl=0
	but not https://www.dropbox.com/home or https://www.dropbox.com/request/abc`)
	want := []string{
		"https://www.dropbox.com/scl/fo/abc123/AKey-9?rlkey=rk1",
		"https://www.dropbox.com/sh/oldid1/AAKey_x",
		"https://www.dropbox.com/scl/fi/fileid1/Mod.zipmod?rlkey=rk2",
		"https://www.dropbox.com/s/oldfile9/Mod.zipmod",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v\nwant %v", got, want)
	}
	// wrapped in a redirect URL, and de-duplicated by linksIn
	l := forumDropbox.linksIn([]string{
		"https://kknow.cc/proxy.php?link=https%3A%2F%2Fwww.dropbox.com%2Fscl%2Ffo%2Fabc123%2FAKey-9%3Frlkey%3Drk1%26dl%3D0&hash=1",
		"https://www.dropbox.com/scl/fo/abc123/AKey-9?rlkey=rk1&dl=0",
	})
	if len(l) != 1 || l[0] != "https://www.dropbox.com/scl/fo/abc123/AKey-9?rlkey=rk1" {
		t.Errorf("proxied: %v", l)
	}
	a, _ := forumDropbox.id("https://www.dropbox.com/scl/fo/abc123/AKey-9?rlkey=rk1")
	b, _ := forumDropbox.id("https://www.dropbox.com/scl/fi/abc123/AKey-9?rlkey=rk1")
	if a == b {
		t.Errorf("a folder and a file must not be duplicates: %q %q", a, b)
	}
}

func TestDropboxItemURL(t *testing.T) {
	for _, c := range []struct{ base, rlkey, full, want string }{
		{"scl/fo/abc123/AKey-9", "rk1", "/top.zipmod", "https://www.dropbox.com/scl/fo/abc123/AKey-9?preview=top.zipmod&rlkey=rk1"},
		{"scl/fo/abc123/AKey-9", "rk1", "/Sub Dir/a.zipmod", "https://www.dropbox.com/scl/fo/abc123/AKey-9/Sub%20Dir?preview=a.zipmod&rlkey=rk1"},
		{"sh/oldid1/AAKey_x", "", "/inner/b.zipmod", "https://www.dropbox.com/sh/oldid1/AAKey_x/inner/b.zipmod"},
	} {
		if got := dropboxItemURL(c.base, c.rlkey, c.full); got != c.want {
			t.Errorf("%s %s: got %s want %s", c.base, c.full, got, c.want)
		}
	}
}

func TestDropboxHeaderJSON(t *testing.T) {
	got := dropboxHeaderJSON([]byte(`{"path":"/Dossié/😀.zipmod"}`))
	if got != `{"path":"/Dossi\u00e9/\ud83d\ude00.zipmod"}` {
		t.Errorf("got %s", got)
	}
	var back struct{ Path string }
	if err := json.Unmarshal([]byte(got), &back); err != nil || back.Path != "/Dossié/😀.zipmod" {
		t.Errorf("round trip: %q %v", back.Path, err)
	}
}

func TestParseDropboxError(t *testing.T) {
	if e := parseDropboxError(409, []byte(`{"error_summary":"shared_link_not_found/..","error":{".tag":"shared_link_not_found"}}`), ""); !e.permanent() {
		t.Errorf("not found: %+v", e)
	}
	if e := parseDropboxError(409, []byte(`{"error_summary":"path/not_found/.."}`), ""); !e.permanent() {
		t.Errorf("path not found: %+v", e)
	}
	if e := parseDropboxError(409, []byte(`{"error_summary":"shared_link_access_denied/.."}`), ""); e.permanent() || e.retryable() {
		t.Errorf("access denied must neither purge nor be retried within a run: %+v", e)
	}
	if e := parseDropboxError(401, []byte("Error in call to API function: invalid_access_token"), ""); e.permanent() || e.retryable() || e.Message == "" {
		t.Errorf("401: %+v", e)
	}
	if e := parseDropboxError(429, []byte(`{"error_summary":"too_many_requests/.."}`), "7"); e.permanent() || !e.retryable() || e.RetryAfter.Seconds() != 7 {
		t.Errorf("429: %+v", e)
	}
	if e := parseDropboxError(503, []byte("<html>"), ""); e.permanent() || !e.retryable() {
		t.Errorf("503: %+v", e)
	}
}

// ---------- fake Dropbox ----------

type fakeDBNode struct {
	name    string
	dir     bool
	content []byte
	status  int    // non-zero: answer download requests with this error
	summary string // error summary for that status
}

type fakeDBLink struct {
	name string
	dirs map[string][]*fakeDBNode // directory path ("" = root) -> children
	file *fakeDBNode              // a link to one file
}

type fakeDropbox struct {
	mu         sync.Mutex
	links      map[string]*fakeDBLink
	pageSize   int
	valid      string // the access token the API accepts
	tokenCalls int
	downloads  map[string]int
	badHeaders int
	srv        *httptest.Server
}

func newFakeDropbox(t *testing.T) *fakeDropbox {
	p := &fakeDropbox{links: map[string]*fakeDBLink{}, valid: "tok1", downloads: map[string]int{}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	dropboxAPIBase = p.srv.URL + "/2"
	dropboxContentBase = p.srv.URL + "/content/2"
	dropboxTokenURL = p.srv.URL + "/oauth2/token"
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeDropbox) fail(w http.ResponseWriter, status int, summary string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error_summary": summary, "error": map[string]any{}})
}

func (n *fakeDBNode) entry() map[string]any {
	if n.dir {
		return map[string]any{".tag": "folder", "name": n.name}
	}
	return map[string]any{".tag": "file", "name": n.name, "size": len(n.content), "server_modified": "2024-03-01T10:00:00Z"}
}

func (p *fakeDropbox) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path == "/oauth2/token" {
		user, pass, ok := r.BasicAuth()
		r.ParseForm()
		if !ok || user != "appkey" || pass != "appsecret" || r.Form.Get("refresh_token") != "refresh1" || r.Form.Get("grant_type") != "refresh_token" {
			w.WriteHeader(400)
			return
		}
		p.tokenCalls++
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok%d", p.tokenCalls), "expires_in": 14400})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+p.valid {
		w.WriteHeader(401)
		io.WriteString(w, "Error in call to API function: expired_access_token")
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/2/sharing/get_shared_link_metadata":
		var a struct{ URL string `json:"url"` }
		json.Unmarshal(body, &a)
		l := p.links[a.URL]
		if l == nil {
			p.fail(w, 409, "shared_link_not_found/..")
			return
		}
		if l.file != nil {
			json.NewEncoder(w).Encode(l.file.entry())
			return
		}
		json.NewEncoder(w).Encode(map[string]any{".tag": "folder", "name": l.name})
	case "/2/files/list_folder", "/2/files/list_folder/continue":
		var a struct {
			Path       string `json:"path"`
			SharedLink struct{ URL string `json:"url"` } `json:"shared_link"`
			Cursor     string `json:"cursor"`
		}
		json.Unmarshal(body, &a)
		offset := 0
		if a.Cursor != "" { // the cursor is "<link>\n<dir>\n<offset>"
			parts := strings.Split(a.Cursor, "\n")
			a.SharedLink.URL, a.Path = parts[0], parts[1]
			fmt.Sscanf(parts[2], "%d", &offset)
		}
		l := p.links[a.SharedLink.URL]
		if l == nil {
			p.fail(w, 409, "shared_link_not_found/..")
			return
		}
		kids, ok := l.dirs[a.Path]
		if !ok {
			p.fail(w, 409, "path/not_found/..")
			return
		}
		end := len(kids)
		if p.pageSize > 0 && offset+p.pageSize < end {
			end = offset + p.pageSize
		}
		var entries []any
		for _, k := range kids[offset:end] {
			entries = append(entries, k.entry())
		}
		json.NewEncoder(w).Encode(map[string]any{
			"entries": entries, "has_more": end < len(kids),
			"cursor": fmt.Sprintf("%s\n%s\n%d", a.SharedLink.URL, a.Path, end),
		})
	case "/content/2/sharing/get_shared_link_file":
		arg := r.Header.Get("Dropbox-API-Arg")
		for i := 0; i < len(arg); i++ {
			if arg[i] >= 0x80 {
				p.badHeaders++
				p.fail(w, 400, "non-ascii header")
				return
			}
		}
		var a struct{ URL, Path string }
		json.Unmarshal([]byte(arg), &a)
		l := p.links[a.URL]
		if l == nil {
			p.fail(w, 409, "shared_link_not_found/..")
			return
		}
		node := l.file
		if a.Path != "" {
			i := strings.LastIndex(a.Path, "/")
			for _, k := range l.dirs[a.Path[:i]] {
				if k.name == a.Path[i+1:] && !k.dir {
					node = k
				}
			}
		}
		p.downloads[a.URL+"|"+a.Path]++
		if node == nil {
			p.fail(w, 409, "path/not_found/..")
			return
		}
		if node.status != 0 {
			p.fail(w, node.status, node.summary)
			return
		}
		var s, e int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e); err != nil || s > e || e >= len(node.content) {
			p.fail(w, 416, "bad_range")
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(node.content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(node.content[s : e+1])
	default:
		http.NotFound(w, r)
	}
}

func writeDropboxConfig(t *testing.T, dir string, groups ...*linkGroup) string {
	t.Helper()
	cfg := &hostedConfig{Groups: groups}
	path := filepath.Join(dir, "config", "dropbox.json")
	if err := cfg.save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func readIndex(t *testing.T, path string) modpackIndexFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var idx modpackIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestDropboxFolders(t *testing.T) {
	p := newFakeDropbox(t)
	t.Setenv("DROPBOX_ACCESS_TOKEN", "tok1")
	zm := func(name, guid string, big bool) *fakeDBNode {
		return &fakeDBNode{name: name, content: makeZipmod(t, guid, "1.0", big)}
	}
	dirNode := func(name string) *fakeDBNode { return &fakeDBNode{name: name, dir: true} }

	const newLink = "https://www.dropbox.com/scl/fo/abc123/AKey-9?rlkey=rk1"
	const oldLink = "https://www.dropbox.com/sh/oldid1/AAKey_x"
	p.pageSize = 2 // forces the list_folder/continue path for the root
	p.links[newLink] = &fakeDBLink{name: "My Mods", dirs: map[string][]*fakeDBNode{
		"": {zm("top.zipmod", "com.test.top", false), {name: "readme.txt", content: []byte("hi")},
			zm("Mód.zipmod", "com.test.accent", false), dirNode("Sub Dir")},
		"/Sub Dir": {zm("a.zipmod", "com.test.sub", true), dirNode("Dossié"),
			{name: "NotAZip.zip", content: []byte("this is not a zip file at all")}},
		"/Sub Dir/Dossié": {zm("Ünï.zipmod", "com.test.unicode", false)},
	}}
	p.links[oldLink] = &fakeDBLink{name: "Old Style", dirs: map[string][]*fakeDBNode{
		"": {zm("inner.zipmod", "com.test.old", false),
			// an answer that is neither "gone" nor retried within a run (a 429 would sleep and retry)
			{name: "Limited.zipmod", content: make([]byte, 100), status: 409, summary: "shared_link_access_denied/.."}},
	}}

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeDropboxConfig(t, dir, &linkGroup{Name: "manual", Links: []string{
			newLink + "&st=x&dl=0", oldLink + "?dl=0",
		}}),
		dataDir: filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 3,
	}
	n, f := buildHostedIndexes(opt, dropboxProvider{})
	if n != 5 || f != 0 {
		t.Fatalf("first run: newGuids=%d failures=%d (bad headers: %d)", n, f, p.badHeaders)
	}
	idx := readIndex(t, filepath.Join(dir, "data", "dropbox", "My Mods.json"))
	for g, u := range map[string]string{
		"com.test.top": "https://www.dropbox.com/scl/fo/abc123/AKey-9?preview=top.zipmod&rlkey=rk1",
		"com.test.sub": "https://www.dropbox.com/scl/fo/abc123/AKey-9/Sub%20Dir?preview=a.zipmod&rlkey=rk1",
	} {
		if len(idx.Guids[g]) != 1 || idx.Guids[g][0] != u {
			t.Errorf("%s: %v want %s", g, idx.Guids[g], u)
		}
	}
	for _, g := range []string{"com.test.accent", "com.test.unicode"} {
		if len(idx.Guids[g]) != 1 {
			t.Errorf("%s (a non-ASCII path) is missing: %v", g, idx.Guids)
		}
	}
	if len(idx.Guids) != 4 {
		t.Errorf("guids: %v", idx.Guids)
	}
	old := readIndex(t, filepath.Join(dir, "data", "dropbox", "Old Style.json"))
	if u := old.Guids["com.test.old"]; len(u) != 1 || u[0] != "https://www.dropbox.com/sh/oldid1/AAKey_x/inner.zipmod" {
		t.Errorf("old style: %v", old.Guids)
	}
	if e := loadCache(filepath.Join(dir, "cache", "dropbox", "My Mods.json"))[dropboxItemURL("scl/fo/abc123/AKey-9", "rk1", "/Sub Dir/NotAZip.zip")]; e.Error == "" || e.Guid != "" {
		t.Errorf("NotAZip.zip should be recorded as unindexable: %+v", e)
	}
	if _, ok := loadCache(filepath.Join(dir, "cache", "dropbox", "Old Style.json"))[dropboxItemURL("sh/oldid1/AAKey_x", "", "/Limited.zipmod")]; ok {
		t.Error("an access-denied answer must not be cached (it should be retried next run)")
	}

	// Second run: nothing new; only the access-denied file is tried again.
	p.mu.Lock()
	p.downloads = map[string]int{}
	p.mu.Unlock()
	if n, f := buildHostedIndexes(opt, dropboxProvider{}); n != 0 || f != 0 {
		t.Fatalf("second run: %d %d", n, f)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, c := range p.downloads {
		if !strings.HasSuffix(k, "|/Limited.zipmod") {
			t.Errorf("second run downloaded %s again (%d requests)", k, c)
		}
	}
	if p.downloads[oldLink+"|/Limited.zipmod"] == 0 {
		t.Error("the access-denied file was not retried")
	}
}

func TestDropboxSingleFilesAndPurge(t *testing.T) {
	p := newFakeDropbox(t)
	t.Setenv("DROPBOX_ACCESS_TOKEN", "tok1")
	const mod = "https://www.dropbox.com/scl/fi/fileid1/Mod.zipmod?rlkey=rk2"
	const card = "https://www.dropbox.com/scl/fi/fileid2/card.png?rlkey=rk3"
	const pack = "https://www.dropbox.com/s/oldfile9/pack.zip"
	const empty = "https://www.dropbox.com/scl/fo/emptyid/EKey-1?rlkey=rk4"
	p.links[mod] = &fakeDBLink{file: &fakeDBNode{name: "Mod.zipmod", content: makeZipmod(t, "com.test.single", "1.0", false)}}
	p.links[card] = &fakeDBLink{file: &fakeDBNode{name: "card.png", content: []byte("png")}}
	p.links[pack] = &fakeDBLink{file: &fakeDBNode{name: "pack.zip", content: []byte("this is not a zip file at all")}}
	p.links[empty] = &fakeDBLink{name: "Cards only", dirs: map[string][]*fakeDBNode{"": {{name: "c.png", content: []byte("x")}}}}

	dir := t.TempDir()
	scraped := &linkGroup{Name: "kknow.cc", Search: "https://kknow.cc/search/?q=x", Links: []string{
		mod + "&dl=0",
		card, // not a mod archive: purged
		pack, // a zip that is no mod: purged
		"https://www.dropbox.com/scl/fi/missing0/gone.zipmod?rlkey=zz", // gone: purged
		empty, // no mods: purged
	}}
	opt := hostedOptions{
		configPath: writeDropboxConfig(t, dir, scraped),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	n, f := buildHostedIndexes(opt, dropboxProvider{})
	if n != 1 || f != 0 {
		t.Fatalf("newGuids=%d failures=%d", n, f)
	}
	idx := readIndex(t, filepath.Join(dir, "data", "dropbox", singleFilesName+".json"))
	if u := idx.Guids["com.test.single"]; len(u) != 1 || u[0] != mod {
		t.Errorf("single file: %v", idx.Guids)
	}
	cfg, err := loadHostedConfig(opt.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if links := cfg.allLinks(); len(links) != 1 || links[0] != mod+"&dl=0" {
		t.Errorf("links left after purging: %v", links)
	}
}

func TestDropboxCredentials(t *testing.T) {
	p := newFakeDropbox(t)
	for _, k := range []string{"DROPBOX_ACCESS_TOKEN", "DROPBOX_REFRESH_TOKEN", "DROPBOX_APP_KEY", "DROPBOX_APP_SECRET"} {
		t.Setenv(k, "")
	}
	const link = "https://www.dropbox.com/scl/fi/fileid1/Mod.zipmod?rlkey=rk2"
	p.links[link] = &fakeDBLink{file: &fakeDBNode{name: "Mod.zipmod", content: makeZipmod(t, "com.test.single", "1.0", false)}}

	if _, err := newDropboxClient(); err == nil {
		t.Error("no credentials should be an error")
	}
	t.Setenv("DROPBOX_REFRESH_TOKEN", "refresh1")
	if _, err := newDropboxClient(); err == nil {
		t.Error("a refresh token without app key and secret should be an error")
	}
	t.Setenv("DROPBOX_APP_KEY", "appkey")
	t.Setenv("DROPBOX_APP_SECRET", "wrongsecret")
	if _, err := newDropboxClient(); err == nil {
		t.Error("a rejected refresh token request should be an error")
	}
	t.Setenv("DROPBOX_APP_SECRET", "appsecret")
	c, err := newDropboxClient()
	if err != nil {
		t.Fatal(err)
	}
	if md, err := c.metadata(link); err != nil || md.Name != "Mod.zipmod" {
		t.Fatalf("metadata: %+v %v", md, err)
	}
	// the access token expires: the client gets a new one and carries on
	p.mu.Lock()
	p.valid = "tok2"
	p.mu.Unlock()
	f, err := (dropboxProvider{}).LoadFile(link) // makes its own client (token 2)
	if err != nil || len(f.Listing) != 1 {
		t.Fatalf("LoadFile: %v %v", f, err)
	}
	p.mu.Lock()
	p.valid = "tok3"
	p.mu.Unlock()
	if _, err := c.metadata(link); err != nil { // c still holds tok1: 401, refresh, retry
		t.Errorf("expired token was not renewed: %v", err)
	}
}
