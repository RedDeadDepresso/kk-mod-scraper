package scrapers

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseCatboxLinks(t *testing.T) {
	for _, s := range []string{
		"https://catbox.moe/c/ab12cd",
		"http://www.catbox.moe/c/ab12cd/",
		"https://catbox.moe/c/ab12cd?x=1#y",
	} {
		if id, err := parseCatboxAlbumLink(s); err != nil || id != "ab12cd" {
			t.Errorf("album %q: %q, %v", s, id, err)
		}
		if _, err := parseCatboxFileLink(s); err == nil {
			t.Errorf("%q is an album, not a file", s)
		}
		if _, err := (catboxProvider{}).ID(s); err != nil {
			t.Errorf("%q should be a folder link: %v", s, err)
		}
	}
	for _, s := range []string{
		"https://files.catbox.moe/ab12cd.zip",
		"http://files.catbox.moe/ab12cd.zip?download=1",
		"HTTPS://FILES.CATBOX.MOE/ab12cd.zip",
	} {
		if n, err := parseCatboxFileLink(s); err != nil || strings.ToLower(n) != "ab12cd.zip" {
			t.Errorf("file %q: %q, %v", s, n, err)
		}
		if _, err := parseCatboxAlbumLink(s); err == nil {
			t.Errorf("%q is a file, not an album", s)
		}
		if _, err := (catboxProvider{}).ID(s); err == nil {
			t.Errorf("%q must not be taken for a folder", s)
		}
		if _, err := (catboxProvider{}).FileID(s); err != nil {
			t.Errorf("%q should be a file link: %v", s, err)
		}
	}
	for _, s := range []string{
		"", "https://catbox.moe/", "https://catbox.moe/c/", "https://catbox.moe/c/ab", "https://files.catbox.moe/",
		"https://files.catbox.moe/noextension", "https://litter.catbox.moe/ab12cd.zip", "https://example.com/c/ab12cd",
		"https://notcatbox.moe/c/ab12cd", "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg",
	} {
		if _, err := parseCatboxAlbumLink(s); err == nil {
			t.Errorf("%q should be rejected as an album", s)
		}
		if _, err := parseCatboxFileLink(s); err == nil {
			t.Errorf("%q should be rejected as a file", s)
		}
	}
}

func TestFindCatboxLinks(t *testing.T) {
	got := findCatboxLinks(`Album: https://catbox.moe/c/ab12cd, mod (https://files.catbox.moe/zz99yy.zipmod),
	pack https://files.catbox.moe/qq11ww.ZIP. and again https://catbox.moe/c/ab12cd/.
	Images are ignored: https://files.catbox.moe/img001.png https://files.catbox.moe/card02.jpg
	and so are lookalikes: https://files.catbox.moe/abcd12.zipper https://litter.catbox.moe/abcd12.zip`)
	want := []string{
		"https://catbox.moe/c/ab12cd",
		"https://catbox.moe/c/ab12cd",
		"https://files.catbox.moe/zz99yy.zipmod",
		"https://files.catbox.moe/qq11ww.ZIP",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v want %v", got, want)
	}
	l := forumCatbox.linksIn([]string{
		"https://kknow.cc/proxy.php?link=https%3A%2F%2Ffiles.catbox.moe%2Fzz99yy.zipmod&hash=1",
		"https://files.catbox.moe/zz99yy.zipmod",
	})
	if len(l) != 1 || l[0] != "https://files.catbox.moe/zz99yy.zipmod" {
		t.Errorf("proxied: %v", l)
	}
	a, _ := forumCatbox.id("https://catbox.moe/c/ab12cd")
	b, _ := forumCatbox.id("https://files.catbox.moe/ab12cd.zip")
	if a == b {
		t.Errorf("an album and a file must not be duplicates: %q %q", a, b)
	}
}

func TestParseCatboxAlbum(t *testing.T) {
	title, names := parseCatboxAlbum([]byte(`<html><head><title>Catbox - My Koikatsu mods</title></head><body>
		<a href="https://files.catbox.moe/aaaa11.zipmod">one</a>
		<a href="//files.catbox.moe/bbbb22.zip">two</a>
		<img src="https://files.catbox.moe/pic001.png">
		<a href="https://files.catbox.moe/aaaa11.zipmod">one again</a>
		<script>var more = "https:\/\/example.com/x"; var u = "https://files.catbox.moe/cccc33.zip";</script></body></html>`))
	if title != "My Koikatsu mods" {
		t.Errorf("title %q", title)
	}
	if strings.Join(names, " ") != "aaaa11.zipmod bbbb22.zip pic001.png cccc33.zip" {
		t.Errorf("names %v", names)
	}
	for _, page := range []string{`<title>Catbox</title>`, `<title>catbox.moe</title>`, `<title>Catbox.moe | </title>`, `<html></html>`} {
		if title, _ := parseCatboxAlbum([]byte(page)); title != "" {
			t.Errorf("%q: a title that only says Catbox is no title, got %q", page, title)
		}
	}
	if title, _ := parseCatboxAlbum([]byte(`<title>Cool pack | Catbox</title>`)); title != "Cool pack" {
		t.Errorf("suffix not trimmed: %q", title)
	}
}

func TestCatboxErrors(t *testing.T) {
	if !(&catboxError{Status: 404}).permanent() || !(&catboxError{Status: 410}).permanent() {
		t.Error("404 and 410 are permanent")
	}
	for _, s := range []int{403, 429, 500, 502} {
		if (&catboxError{Status: s}).permanent() {
			t.Errorf("%d must not purge anything", s)
		}
	}
	if !(&catboxError{Status: 429}).retryable() || (&catboxError{Status: 403}).retryable() {
		t.Error("429 is retried, 403 is not")
	}
	if !isGoneError(fmt.Errorf("wrapped: %w", &catboxError{Status: 404})) || isGoneError(&catboxError{Status: 403}) {
		t.Error("isGoneError should follow permanent()")
	}
}

// ---------- fake Catbox ----------

type fakeCBFile struct {
	content []byte
	date    string // Last-Modified
	status  int    // non-zero: every request is answered with this status
}

type fakeCatbox struct {
	mu     sync.Mutex
	files  map[string]*fakeCBFile
	albums map[string]string // album id -> page
	hits   map[string]int    // file name -> requests
	srv    *httptest.Server
}

func newFakeCatbox(t *testing.T) *fakeCatbox {
	c := &fakeCatbox{files: map[string]*fakeCBFile{}, albums: map[string]string{}, hits: map[string]int{}}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	catboxSiteBase, catboxFilesBase = c.srv.URL, c.srv.URL
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCatbox) addFile(name string, content []byte) *fakeCBFile {
	f := &fakeCBFile{content: content, date: "Thu, 15 Feb 2024 16:30:51 GMT"}
	c.files[name] = f
	return f
}

// addAlbum makes an album page that links to the given files with real-host URLs, as the real page does.
func (c *fakeCatbox) addAlbum(id, title string, names ...string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<html><head><title>Catbox - %s</title></head><body>", title)
	for _, n := range names {
		fmt.Fprintf(&sb, `<a href="https://files.catbox.moe/%s">%s</a>`, n, n)
	}
	sb.WriteString("</body></html>")
	c.albums[id] = sb.String()
}

func (c *fakeCatbox) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id, ok := strings.CutPrefix(r.URL.Path, "/c/"); ok {
		page, ok := c.albums[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, page)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	f := c.files[name]
	c.hits[name]++
	if f == nil {
		http.NotFound(w, r)
		return
	}
	if f.status != 0 {
		http.Error(w, "nope", f.status)
		return
	}
	var s, e int
	rng := r.Header.Get("Range")
	switch {
	case strings.HasPrefix(rng, "bytes=-"):
		var n int
		fmt.Sscanf(rng, "bytes=-%d", &n)
		n = min(n, len(f.content))
		s, e = len(f.content)-n, len(f.content)-1
	default:
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &s, &e); err != nil || s > e {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		e = min(e, len(f.content)-1)
	}
	w.Header().Set("Last-Modified", f.date)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(f.content)))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(f.content[s : e+1])
}

func writeCatboxConfig(t *testing.T, dir string, groups ...*linkGroup) string {
	t.Helper()
	path := filepath.Join(dir, "config", "catbox.json")
	if err := (&hostedConfig{Groups: groups}).save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCatboxAlbumEndToEnd(t *testing.T) {
	c := newFakeCatbox(t)
	c.addFile("aaaa11.zipmod", makeZipmod(t, "com.test.a", "1.0", false))
	c.addFile("bbbb22.ZIP", makeZipmod(t, "com.test.big", "2.0", true))
	c.addFile("cccc33.zip", []byte("this is not a zip file at all"))
	c.addFile("dddd44.zipmod", make([]byte, 100)).status = 404 // deleted
	c.addFile("eeee55.zipmod", make([]byte, 100)).status = 429 // rate limited
	c.addFile("pic001.png", []byte("png"))
	c.addAlbum("alb001", "My mods", "aaaa11.zipmod", "bbbb22.ZIP", "cccc33.zip", "dddd44.zipmod", "eeee55.zipmod", "pic001.png")

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeCatboxConfig(t, dir, &linkGroup{Name: "manual", Links: []string{"https://catbox.moe/c/alb001"}}),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 3,
	}
	cachePath := filepath.Join(dir, "cache", "catbox", "My mods.json")

	n, f := buildHostedIndexes(opt, catboxProvider{})
	if n != 2 || f != 0 {
		t.Fatalf("first run: newGuids=%d failures=%d", n, f)
	}
	idx := readMediaFireIndexKind(t, dir, "catbox", "My mods")
	want := map[string]string{
		"com.test.a":   "https://files.catbox.moe/aaaa11.zipmod",
		"com.test.big": "https://files.catbox.moe/bbbb22.ZIP",
	}
	if idx.Count != len(want) || len(idx.Guids) != len(want) {
		t.Errorf("index: %+v", idx)
	}
	for g, u := range want {
		if len(idx.Guids[g]) != 1 || idx.Guids[g][0] != u {
			t.Errorf("%s: %v want %s", g, idx.Guids[g], u)
		}
	}
	cache := loadCache(cachePath)
	for _, name := range []string{"cccc33.zip", "dddd44.zipmod"} {
		if e := cache[catboxFileURL(name)]; e.Error == "" || e.Guid != "" {
			t.Errorf("%s should be recorded as unindexable: %+v", name, e)
		}
	}
	if _, ok := cache[catboxFileURL("eeee55.zipmod")]; ok {
		t.Error("a rate limited file must not be cached (it is retried next run)")
	}
	if _, ok := cache[catboxFileURL("pic001.png")]; ok || c.hits["pic001.png"] != 0 {
		t.Error("the image should have been ignored without a request")
	}

	// Second run: files never change under their id, so only the rate limited one is tried again.
	c.mu.Lock()
	c.hits = map[string]int{}
	c.mu.Unlock()
	if n, f := buildHostedIndexes(opt, catboxProvider{}); n != 0 || f != 0 {
		t.Fatalf("second run: %d %d", n, f)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, hits := range c.hits {
		if name != "eeee55.zipmod" {
			t.Errorf("second run requested %s again (%d requests)", name, hits)
		}
	}
	if c.hits["eeee55.zipmod"] == 0 {
		t.Error("the rate limited file was not retried")
	}
}

func TestCatboxSingleFilesAndPurge(t *testing.T) {
	c := newFakeCatbox(t)
	c.addFile("modfile.zipmod", makeZipmod(t, "com.test.single", "1.0", false))
	c.addFile("packfl.zip", []byte("this is not a zip file at all"))
	c.addFile("card01.png", []byte("png"))
	c.addFile("emptyf.zip", nil)
	c.addAlbum("imgsonly", "Cards", "card01.png")

	dir := t.TempDir()
	scraped := &linkGroup{Name: "kknow.cc", Search: "https://kknow.cc/search/?q=x", Links: []string{
		"https://files.catbox.moe/modfile.zipmod",
		"https://files.catbox.moe/card01.png",     // not a mod archive: purged without a request
		"https://files.catbox.moe/packfl.zip",     // a zip that is no mod: purged
		"https://files.catbox.moe/missing.zipmod", // gone: purged
		"https://catbox.moe/c/imgsonly",           // no zips: purged
		"https://catbox.moe/c/nosuchalbum",        // gone: purged
	}}
	opt := hostedOptions{
		configPath: writeCatboxConfig(t, dir, scraped),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	n, f := buildHostedIndexes(opt, catboxProvider{})
	if n != 1 || f != 0 {
		t.Fatalf("newGuids=%d failures=%d", n, f)
	}
	if c.hits["card01.png"] != 0 {
		t.Error("a link that isn't a zip must not be requested")
	}
	idx := readMediaFireIndexKind(t, dir, "catbox", singleFilesName)
	if u := idx.Guids["com.test.single"]; len(u) != 1 || u[0] != "https://files.catbox.moe/modfile.zipmod" {
		t.Errorf("single file: %v", idx.Guids)
	}
	cfg, err := loadHostedConfig(opt.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if links := cfg.allLinks(); len(links) != 1 || links[0] != "https://files.catbox.moe/modfile.zipmod" {
		t.Errorf("links left after purging: %v", links)
	}
}

func TestCatboxHandMaintainedLinksAreNeverPurged(t *testing.T) {
	newFakeCatbox(t)
	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeCatboxConfig(t, dir, &linkGroup{Name: "manual", Links: []string{"https://catbox.moe/c/nosuchalbum"}}),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	if _, f := buildHostedIndexes(opt, catboxProvider{}); f != 1 {
		t.Errorf("a dead link in a manual group is a failure, got %d", f)
	}
	cfg, err := loadHostedConfig(opt.configPath)
	if err != nil || len(cfg.allLinks()) != 1 {
		t.Errorf("manual link must stay: %v %v", cfg, err)
	}
}

func TestCatboxProbeOfADeletedFileIsPermanent(t *testing.T) {
	c := newFakeCatbox(t)
	c.addFile("gone01.zip", make([]byte, 10)).status = 404
	_, err := catboxRanger{"gone01.zip"}.probe()
	var pe *permanentProbeError
	if !errors.As(err, &pe) {
		t.Errorf("a 404 should be a permanent probe error: %v", err)
	}
}
