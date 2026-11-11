package scrapers

import (
	"archive/zip"
	"bytes"
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

func TestParsePixeldrainDirLinks(t *testing.T) {
	for s, want := range map[string]string{
		"https://pixeldrain.com/d/AbCd1234":                      "AbCd1234|",
		"https://pixeldrain.com/d/AbCd1234/":                     "AbCd1234|",
		"https://www.pixeldrain.net/d/AbCd1234/My%20Mods/sub?x=1": "AbCd1234|/My Mods/sub",
		"https://pixeldrain.com/api/filesystem/AbCd1234/a.zipmod": "AbCd1234|/a.zipmod",
	} {
		share, sub, err := parsePixeldrainDirLink(s)
		if err != nil || share+"|"+sub != want {
			t.Errorf("%q: %q %q %v, want %s", s, share, sub, err, want)
		}
		if _, err := parsePixeldrainListLink(s); err == nil {
			t.Errorf("%q is a directory, not a list", s)
		}
		if _, err := (pixeldrainProvider{}).ID(s); err != nil {
			t.Errorf("%q should be a folder link: %v", s, err)
		}
	}
	for _, s := range []string{"", "https://pixeldrain.com/d/", "https://pixeldrain.com/u/Zy9x8w7v", "https://example.com/d/AbCd1234"} {
		if _, _, err := parsePixeldrainDirLink(s); err == nil {
			t.Errorf("%q should be rejected as a directory", s)
		}
	}
	got := findPixeldrainLinks("see https://pixeldrain.com/d/AbCd1234/My%20Mods/a.zipmod, and (https://pixeldrain.com/d/Zz9988xx).")
	want := []string{"https://pixeldrain.com/d/AbCd1234/My%20Mods/a.zipmod", "https://pixeldrain.com/d/Zz9988xx"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestPixeldrainDirectoryShare(t *testing.T) {
	p := newFakePixeldrain(t)
	add := func(path string, f *fakePDFile) {
		p.fsFiles[path] = f
		dir := path[:strings.LastIndex(path, "/")]
		p.dirs[dir] = append(p.dirs[dir], path)
	}
	p.dirs["/ShareId01"] = []string{"/ShareId01/Mods Pack"}
	p.dirs["/ShareId01/Mods Pack"] = []string{"/ShareId01/Mods Pack/Deep"}
	p.dirs["/ShareId01/Mods Pack/Deep"] = nil
	add("/ShareId01/top.zipmod", &fakePDFile{name: "top.zipmod", content: makeZipmod(t, "com.test.top", "1.0", false)})
	add("/ShareId01/readme.txt", &fakePDFile{name: "readme.txt", content: []byte("hi")})
	add("/ShareId01/Mods Pack/inner.zipmod", &fakePDFile{name: "inner.zipmod", content: makeZipmod(t, "com.test.inner", "1.0", false)})
	add("/ShareId01/Mods Pack/Deep/deep.zipmod", &fakePDFile{name: "deep.zipmod", content: makeZipmod(t, "com.test.deep", "1.0", true)})

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writePixeldrainConfig(t, dir, &linkGroup{Name: "manual", Links: []string{
			"https://pixeldrain.com/d/ShareId01",
			"https://pixeldrain.com/d/ShareId01/Mods%20Pack/Deep/deep.zipmod", // a file behind a /d/ link
		}}),
		dataDir: filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	n, f := buildHostedIndexes(opt, pixeldrainProvider{})
	if n != 4 || f != 0 { // 3 from the share, plus deep.zipmod again as its own folder
		t.Fatalf("newGuids=%d failures=%d", n, f)
	}
	read := func(name string) modpackIndexFile {
		data, err := os.ReadFile(filepath.Join(dir, "data", "pixeldrain", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var idx modpackIndexFile
		if err := json.Unmarshal(data, &idx); err != nil {
			t.Fatal(err)
		}
		return idx
	}
	idx := read("ShareId01")
	for g, u := range map[string]string{
		"com.test.top":   "https://pixeldrain.com/d/ShareId01/top.zipmod",
		"com.test.inner": "https://pixeldrain.com/d/ShareId01/Mods%20Pack/inner.zipmod",
		"com.test.deep":  "https://pixeldrain.com/d/ShareId01/Mods%20Pack/Deep/deep.zipmod",
	} {
		if len(idx.Guids[g]) != 1 || idx.Guids[g][0] != u {
			t.Errorf("%s: %v want %s", g, idx.Guids[g], u)
		}
	}
	if len(idx.Guids) != 3 {
		t.Errorf("guids: %v", idx.Guids)
	}
	if one := read("deep.zipmod"); len(one.Guids["com.test.deep"]) != 1 {
		t.Errorf("single-file /d/ link: %v", one.Guids)
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
	dirs      map[string][]string // file-system directory path -> child paths
	fsFiles   map[string]*fakePDFile // file-system file path -> file
	zipFiles  map[string]map[string][]byte // file id -> zip entry path -> bytes (the zip viewer)
	srv       *httptest.Server
}

func newFakePixeldrain(t *testing.T) *fakePixeldrain {
	p := &fakePixeldrain{files: map[string]*fakePDFile{}, lists: map[string][]string{}, titles: map[string]string{}, downloads: map[string]int{}, dirs: map[string][]string{}, fsFiles: map[string]*fakePDFile{}, zipFiles: map[string]map[string][]byte{}}
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
	case strings.HasPrefix(path, "file/") && strings.Contains(path, "/info/zip"):
		p.serveZip(w, r, path)
	case strings.HasPrefix(path, "filesystem/"):
		p.serveFS(w, r, "/"+strings.TrimPrefix(path, "filesystem/"))
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

// serveZip fakes the zip viewer: .../info/zip lists the entries, .../info/zip/<path> serves one.
func (p *fakePixeldrain) serveZip(w http.ResponseWriter, r *http.Request, path string) {
	id, sub, _ := strings.Cut(strings.TrimPrefix(path, "file/"), "/info/zip")
	entries, ok := p.zipFiles[id]
	if !ok {
		p.fail(w, 404, "file_not_found")
		return
	}
	sub = strings.Trim(sub, "/")
	if sub == "" {
		children := map[string]any{}
		for name, data := range entries {
			children[name] = map[string]any{"size": len(data)}
		}
		json.NewEncoder(w).Encode(map[string]any{"size": len(p.files[id].content), "children": children, "properties": []string{"read_individual_files"}})
		return
	}
	data, ok := entries[sub]
	if !ok {
		p.fail(w, 404, "file_not_found")
		return
	}
	p.downloads[id+"/"+sub]++
	var s, e int
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e); err != nil || s > e || e >= len(data) {
		p.fail(w, 416, "bad_range")
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(data)))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(data[s : e+1])
}

func fsNode(path string, isDir bool, f *fakePDFile) map[string]any {
	n := map[string]any{"type": "dir", "path": path, "name": path[strings.LastIndex(path, "/")+1:], "modified": "2024-03-01T10:00:00Z"}
	if !isDir {
		n["type"], n["name"], n["file_size"] = "file", f.name, len(f.content)
	}
	return n
}

func (p *fakePixeldrain) serveFS(w http.ResponseWriter, r *http.Request, path string) {
	path = strings.TrimRight(path, "/")
	if f, ok := p.fsFiles[path]; ok {
		if _, stat := r.URL.Query()["stat"]; stat {
			json.NewEncoder(w).Encode(map[string]any{"path": []any{fsNode(path, false, f)}, "base_index": 0})
			return
		}
		p.downloads[path]++
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
		return
	}
	kids, ok := p.dirs[path]
	if !ok {
		p.fail(w, 404, "path_not_found")
		return
	}
	var children []any
	for _, k := range kids {
		if f, isFile := p.fsFiles[k]; isFile {
			children = append(children, fsNode(k, false, f))
		} else {
			children = append(children, fsNode(k, true, nil))
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"path": []any{fsNode(path, true, nil)}, "base_index": 0, "children": children})
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

// makePackZip builds a zip holding the given entries, stored or deflated.
func makePackZip(t *testing.T, entries map[string][]byte, deflated map[string]bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		method := zip.Store
		if deflated[name] {
			method = zip.Deflate
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPixeldrainZipPackIsIndexedPerEntry(t *testing.T) {
	p := newFakePixeldrain(t)
	entries := map[string][]byte{
		"A mod.zipmod":         makeZipmod(t, "com.test.a", "1.0", false), // stored
		"b.zipmod":             makeZipmod(t, "com.test.b", "2.0", false), // deflated
		"sub/c.zipmod":         makeZipmod(t, "com.test.c", "3.0", true),  // stored, in a folder
		"readme.txt":           []byte("not a mod"),
		"Bad\uFFFDname.zipmod": makeZipmod(t, "com.test.bad", "1.0", false), // undecodable name: skipped
	}
	// The entry URLs of the real server ignore Range, so the scraper must read the pack
	// itself: the fake serves the pack's bytes on /api/file/<id> and only lists the entries.
	p.addFile(&fakePDFile{id: "packZip01", name: "Pack.zip", content: makePackZip(t, entries, map[string]bool{"b.zipmod": true})})
	p.zipFiles["packZip01"] = entries
	// A plain single mod zipped with its manifest at the root: not browsable here (404), so
	// it is read as one file.
	p.addFile(&fakePDFile{id: "plainZip1", name: "single.zip", content: makeZipmod(t, "com.test.single", "1.0", false)})
	p.lists["PackList1"] = []string{"packZip01", "plainZip1"}
	p.titles["PackList1"] = "Packs"

	dir := t.TempDir()
	manual := &linkGroup{Name: "manual", Links: []string{"https://pixeldrain.com/l/PackList1"}}
	opt := hostedOptions{
		configPath: writePixeldrainConfig(t, dir, manual),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	n, f := buildHostedIndexes(opt, pixeldrainProvider{})
	if n != 4 || f != 0 {
		t.Fatalf("newGuids=%d failures=%d", n, f)
	}
	data, err := os.ReadFile(filepath.Join(dir, "data", "pixeldrain", "Packs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx modpackIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"com.test.a":      "https://pixeldrain.com/api/file/packZip01/info/zip/A%20mod.zipmod",
		"com.test.b":      "https://pixeldrain.com/api/file/packZip01/info/zip/b.zipmod",
		"com.test.c":      "https://pixeldrain.com/api/file/packZip01/info/zip/sub/c.zipmod",
		"com.test.single": "https://pixeldrain.com/u/plainZip1",
	}
	for guid, u := range want {
		if got := idx.Guids[guid]; len(got) != 1 || got[0] != u {
			t.Errorf("%s: got %v want %s", guid, got, u)
		}
	}
	if len(idx.Guids) != len(want) {
		t.Errorf("unexpected GUIDs: %v", idx.Guids)
	}
	for key, n := range p.downloads {
		if strings.Contains(key, "/") && n > 0 {
			t.Errorf("the entry URL %q was requested; entries must be read from the pack (Range is ignored there)", key)
		}
	}
}

// A pack whose mods sit in a folder inside the zip (the shape Pixeldrain's zip viewer returns).
func TestPixeldrainZipNestedFolders(t *testing.T) {
	const listing = `{"size":54811966,"children":{"LeiseHG mods II":{"size":54811966,"children":{
		"[hg]Francesco058.zipmod":{"size":1540271},
		"[hg]NewJersey047 (1).zipmod":{"size":1765314},
		"notes.txt":{"size":10},
		"unzen azur lane.zipmod":{"size":8420925}}}},"properties":["read_individual_files"]}`
	var n pixeldrainZipNode
	if err := json.Unmarshal([]byte(listing), &n); err != nil {
		t.Fatal(err)
	}
	if n.hasRootManifest() {
		t.Fatal("the root has no manifest.xml")
	}
	var got []string
	for _, e := range n.entries() {
		got = append(got, pixeldrainZipEntryURL("WjrTK8j1", e.Path))
	}
	want := []string{
		"https://pixeldrain.com/api/file/WjrTK8j1/info/zip/LeiseHG%20mods%20II/%5Bhg%5DFrancesco058.zipmod",
		"https://pixeldrain.com/api/file/WjrTK8j1/info/zip/LeiseHG%20mods%20II/%5Bhg%5DNewJersey047%20%281%29.zipmod",
		"https://pixeldrain.com/api/file/WjrTK8j1/info/zip/LeiseHG%20mods%20II/unzen%20azur%20lane.zipmod",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
