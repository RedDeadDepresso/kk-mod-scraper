package scrapers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParsePixeldrainLinks(t *testing.T) {
	for _, s := range []string{
		"https://pixeldrain.com/l/AbCd1234",
		"http://www.pixeldrain.com/l/AbCd1234?embed",
		"https://pixeldrain.net/l/AbCd1234#item=2",
		"https://pixeldrain.com/api/list/AbCd1234",
		"https://pixeldra.in/l/AbCd1234",
	} {
		if id, err := parsePixeldrainListLink(s); err != nil || id != "AbCd1234" {
			t.Errorf("list %q: %q, %v", s, id, err)
		}
		if _, err := parsePixeldrainFileLink(s); err == nil {
			t.Errorf("%q is a list, not a file", s)
		}
	}
	for _, s := range []string{
		"https://pixeldrain.com/u/Zy9x8w7v",
		"https://pixeldrain.com/u/Zy9x8w7v?download",
		"https://pixeldrain.dev/api/file/Zy9x8w7v",
	} {
		if id, err := parsePixeldrainFileLink(s); err != nil || id != "Zy9x8w7v" {
			t.Errorf("file %q: %q, %v", s, id, err)
		}
		if _, err := parsePixeldrainListLink(s); err == nil {
			t.Errorf("%q is a file, not a list", s)
		}
	}
	for _, s := range []string{"", "https://pixeldrain.com/", "https://pixeldrain.com/u/", "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg", "https://example.com/u/Zy9x8w7v"} {
		if _, err := parsePixeldrainFileLink(s); err == nil {
			t.Errorf("%q should be rejected as a file", s)
		}
		if _, err := parsePixeldrainListLink(s); err == nil {
			t.Errorf("%q should be rejected as a list", s)
		}
	}
}

func TestFindPixeldrainLinks(t *testing.T) {
	got := findPixeldrainLinks(`Grab it here (https://pixeldrain.com/u/Zy9x8w7v), or the pack: https://pixeldrain.com/l/AbCd1234.
	Again https://PIXELDRAIN.com/u/Zy9x8w7v?download and https://pixeldrain.net/api/file/Qq11Ww22,
	but not https://pixeldrain.com/u/waytoolongtobeanid or https://pixeldrain.com/user/api_keys`)
	want := []string{
		"https://pixeldrain.com/u/Zy9x8w7v",
		"https://pixeldrain.com/l/AbCd1234",
		"https://pixeldrain.com/u/Zy9x8w7v",
		"https://pixeldrain.com/u/Qq11Ww22",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v want %v", got, want)
	}
	// wrapped in a redirect URL, and de-duplicated by linksIn
	l := forumPixeldrain.linksIn([]string{
		"https://kknow.cc/proxy.php?link=https%3A%2F%2Fpixeldrain.com%2Fu%2FZy9x8w7v&hash=1",
		"https://pixeldrain.com/u/Zy9x8w7v",
	})
	if len(l) != 1 || l[0] != "https://pixeldrain.com/u/Zy9x8w7v" {
		t.Errorf("proxied: %v", l)
	}
	// the dedupe id tells lists and files apart
	a, _ := forumPixeldrain.id("https://pixeldrain.com/l/AbCd1234")
	b, _ := forumPixeldrain.id("https://pixeldrain.com/u/AbCd1234")
	if a == b {
		t.Errorf("a list and a file with the same id must not be duplicates: %q %q", a, b)
	}
}

func TestParsePixeldrainError(t *testing.T) {
	if e := parsePixeldrainError(404, []byte(`{"success":false,"value":"file_not_found","message":"The file was not found"}`)); !e.permanent() || e.Value != "file_not_found" {
		t.Errorf("404: %+v", e)
	}
	if e := parsePixeldrainError(403, []byte(`{"success":false,"value":"file_rate_limited_captcha_required"}`)); e.permanent() || e.retryable() {
		t.Error("a captcha 403 is neither permanent nor retried within a run: it is tried again next run")
	}
	if e := parsePixeldrainError(429, nil); e.permanent() || !e.retryable() {
		t.Error("429 should be retried, not permanent")
	}
	if e := parsePixeldrainError(502, []byte("<html>")); e.permanent() || !e.retryable() || e.Message == "" {
		t.Errorf("502: %+v", e)
	}
}

// ---------- fake Pixeldrain ----------

type fakePDFile struct {
	id, name, date string
	content        []byte
	status         int    // non-zero: answer download requests with this error
	value          string // error value for that status
}

type fakePixeldrain struct {
	mu        sync.Mutex
	files     map[string]*fakePDFile
	lists     map[string][]string // list id -> file ids
	titles    map[string]string
	downloads map[string]int
	srv       *httptest.Server
}

func newFakePixeldrain(t *testing.T) *fakePixeldrain {
	p := &fakePixeldrain{files: map[string]*fakePDFile{}, lists: map[string][]string{}, titles: map[string]string{}, downloads: map[string]int{}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	pixeldrainAPIBase = p.srv.URL + "/api"
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePixeldrain) addFile(f *fakePDFile) *fakePDFile {
	if f.date == "" {
		f.date = "2024-02-15T16:30:51.123Z"
	}
	p.files[f.id] = f
	return f
}

func (p *fakePixeldrain) fail(w http.ResponseWriter, status int, value string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"success": false, "value": value, "message": "fake " + value})
}

func (p *fakePixeldrain) meta(f *fakePDFile) map[string]any {
	return map[string]any{"id": f.id, "name": f.name, "size": len(f.content), "date_upload": f.date}
}

func (p *fakePixeldrain) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Authorization") != "" {
		if _, pass, ok := r.BasicAuth(); !ok || pass != "testkey" {
			p.fail(w, 401, "authentication_required")
			return
		}
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	switch {
	case strings.HasPrefix(path, "list/"):
		id := strings.TrimPrefix(path, "list/")
		ids, ok := p.lists[id]
		if !ok {
			p.fail(w, 404, "list_not_found")
			return
		}
		var out []map[string]any
		for _, fid := range ids {
			out = append(out, p.meta(p.files[fid]))
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "id": id, "title": p.titles[id], "files": out})
	case strings.HasSuffix(path, "/info") && strings.HasPrefix(path, "file/"):
		f := p.files[strings.TrimSuffix(strings.TrimPrefix(path, "file/"), "/info")]
		if f == nil {
			p.fail(w, 404, "file_not_found")
			return
		}
		json.NewEncoder(w).Encode(p.meta(f))
	case strings.HasPrefix(path, "file/"):
		id := strings.TrimPrefix(path, "file/")
		f := p.files[id]
		p.downloads[id]++
		if f == nil {
			p.fail(w, 404, "file_not_found")
			return
		}
		if f.status != 0 {
			p.fail(w, f.status, f.value)
			return
		}
		var s, e int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e); err != nil || s > e || e >= len(f.content) {
			p.fail(w, 416, "bad_range")
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(f.content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(f.content[s : e+1])
	default:
		http.NotFound(w, r)
	}
}

func writePixeldrainConfig(t *testing.T, dir string, groups ...*linkGroup) string {
	t.Helper()
	cfg := &hostedConfig{Groups: groups}
	path := filepath.Join(dir, "config", "pixeldrain.json")
	if err := cfg.save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPixeldrainEndToEnd(t *testing.T) {
	p := newFakePixeldrain(t)
	p.titles["AbCd1234"] = "My mods"
	p.lists["AbCd1234"] = []string{"fileAAAA", "fileBBBB", "fileTxtX", "fileBadX", "fileGone", "fileLmtd"}
	p.addFile(&fakePDFile{id: "fileAAAA", name: "A.zipmod", content: makeZipmod(t, "com.test.a", "1.0", false)})
	p.addFile(&fakePDFile{id: "fileBBBB", name: "B.ZIPMOD", content: makeZipmod(t, "com.test.big", "2.0", true)})
	p.addFile(&fakePDFile{id: "fileTxtX", name: "readme.txt", content: []byte("hi")})
	p.addFile(&fakePDFile{id: "fileBadX", name: "NotAZip.zip", content: []byte("this is not a zip file at all")})
	p.addFile(&fakePDFile{id: "fileGone", name: "Gone.zipmod", content: make([]byte, 100), status: 404, value: "file_not_found"})
	p.addFile(&fakePDFile{id: "fileLmtd", name: "Limited.zipmod", content: make([]byte, 100), status: 403, value: "file_rate_limited_captcha_required"})

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writePixeldrainConfig(t, dir, &linkGroup{Name: "manual", Links: []string{"https://pixeldrain.com/l/AbCd1234"}}),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 3,
	}
	indexPath := filepath.Join(dir, "data", "pixeldrain", "My mods.json")
	cachePath := filepath.Join(dir, "cache", "pixeldrain", "My mods.json")

	newGuids, failures := buildHostedIndexes(opt, pixeldrainProvider{})
	if newGuids != 2 || failures != 0 {
		t.Fatalf("first run: newGuids=%d failures=%d", newGuids, failures)
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var idx modpackIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"com.test.a":   "https://pixeldrain.com/u/fileAAAA",
		"com.test.big": "https://pixeldrain.com/u/fileBBBB",
	}
	if idx.Count != 2 {
		t.Errorf("header: %+v", idx)
	}
	for g, u := range want {
		if len(idx.Guids[g]) != 1 || idx.Guids[g][0] != u {
			t.Errorf("%s: %v want %s", g, idx.Guids[g], u)
		}
	}

	cache := loadCache(cachePath)
	for _, id := range []string{"fileGone", "fileBadX"} {
		if e := cache[pixeldrainFileURL(id)]; e.Error == "" || e.Guid != "" {
			t.Errorf("%s should be recorded as unindexable: %+v", id, e)
		}
	}
	if _, ok := cache[pixeldrainFileURL("fileLmtd")]; ok {
		t.Error("a captcha/rate-limit answer must not be cached (it should be retried next run)")
	}
	if _, ok := cache[pixeldrainFileURL("fileTxtX")]; ok {
		t.Error("readme.txt should have been ignored")
	}

	// Second run: only the rate-limited file is tried again.
	p.mu.Lock()
	p.downloads = map[string]int{}
	p.mu.Unlock()
	if n, f := buildHostedIndexes(opt, pixeldrainProvider{}); n != 0 || f != 0 {
		t.Fatalf("second run: %d %d", n, f)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, n := range p.downloads {
		if id != "fileLmtd" {
			t.Errorf("second run downloaded %s again (%d requests)", id, n)
		}
	}
	if p.downloads["fileLmtd"] == 0 {
		t.Error("the rate-limited file was not retried")
	}
}

func TestPixeldrainSingleFilesAndPurge(t *testing.T) {
	p := newFakePixeldrain(t)
	p.addFile(&fakePDFile{id: "modFileA", name: "Mod.zipmod", content: makeZipmod(t, "com.test.single", "1.0", false)})
	p.addFile(&fakePDFile{id: "cardFile", name: "card.png", content: []byte("png")})
	p.addFile(&fakePDFile{id: "packFile", name: "pack.zip", content: []byte("this is not a zip file at all")})
	p.lists["EmptyLst"] = nil
	p.titles["EmptyLst"] = "Cards only"

	dir := t.TempDir()
	scraped := &linkGroup{Name: "kknow.cc", Search: "https://kknow.cc/search/?q=x", Links: []string{
		"https://pixeldrain.com/u/modFileA",
		"https://pixeldrain.com/u/cardFile",  // not a mod archive: purged
		"https://pixeldrain.com/u/packFile",  // a zip that is no mod: purged
		"https://pixeldrain.com/u/missing00", // gone: purged
		"https://pixeldrain.com/l/EmptyLst",  // no mods: purged
	}}
	opt := hostedOptions{
		configPath: writePixeldrainConfig(t, dir, scraped),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	n, f := buildHostedIndexes(opt, pixeldrainProvider{})
	if n != 1 || f != 0 {
		t.Fatalf("newGuids=%d failures=%d", n, f)
	}
	data, err := os.ReadFile(filepath.Join(dir, "data", "pixeldrain", singleFilesName+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx modpackIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	if u := idx.Guids["com.test.single"]; len(u) != 1 || u[0] != "https://pixeldrain.com/u/modFileA" {
		t.Errorf("single file: %v", idx.Guids)
	}
	cfg, err := loadHostedConfig(opt.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if links := cfg.allLinks(); len(links) != 1 || links[0] != "https://pixeldrain.com/u/modFileA" {
		t.Errorf("links left after purging: %v", links)
	}
}

func TestPixeldrainAPIKeyIsSentAsBasicAuth(t *testing.T) {
	p := newFakePixeldrain(t)
	p.addFile(&fakePDFile{id: "modFileA", name: "Mod.zipmod", content: makeZipmod(t, "com.test.single", "1.0", false)})
	t.Setenv(pixeldrainKeyEnv, "wrongkey")
	if _, err := (pixeldrainProvider{}).LoadFile("https://pixeldrain.com/u/modFileA"); err == nil {
		t.Error("a rejected key should be an error")
	}
	t.Setenv(pixeldrainKeyEnv, "testkey")
	f, err := (pixeldrainProvider{}).LoadFile("https://pixeldrain.com/u/modFileA")
	if err != nil || len(f.Listing) != 1 {
		t.Fatalf("with the key: %v %v", f, err)
	}
	info, err := f.Probe("https://pixeldrain.com/u/modFileA")
	if err != nil || info.Guid != "com.test.single" {
		t.Errorf("probe: %+v %v", info, err)
	}
}
