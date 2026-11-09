package scrapers

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseGofileLinks(t *testing.T) {
	for _, s := range []string{
		"https://gofile.io/d/aBc123",
		"http://www.gofile.io/d/aBc123/",
		"https://gofile.io/d/aBc123?x=1#y",
		"https://gofile.io/?c=aBc123",
		"https://gofile.io/?foo=1&c=aBc123",
	} {
		if code, err := parseGofileLink(s); err != nil || code != "aBc123" {
			t.Errorf("%q: %q, %v", s, code, err)
		}
		if _, err := (gofileProvider{}).ID(s); err != nil {
			t.Errorf("%q should be a folder link: %v", s, err)
		}
	}
	for _, s := range []string{"", "https://gofile.io/", "https://gofile.io/d/", "https://gofile.io/d/abc/def",
		"https://example.com/d/aBc123", "https://notgofile.io/d/aBc123", "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg"} {
		if _, err := parseGofileLink(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}

func TestFindGofileLinks(t *testing.T) {
	text := `mods: https://gofile.io/d/AbCd12, and (https://gofile.io/d/Zy9x8w) plus
		https://gofile.io/d/eeeeeeee-1111-2222-3333-444444444444 and https://gofile.io/d/Qwerty.`
	got := findGofileLinks(text)
	want := []string{"https://gofile.io/d/AbCd12", "https://gofile.io/d/Zy9x8w", "https://gofile.io/d/Qwerty"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v want %v", got, want)
	}
}

// --- the signing secret ---------------------------------------------------

const gofileTestAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/="

// buildGofileScript lays out a bundle in the shape the real one has: an alphabet, a rotated
// table, and call sites pairing an index with the key the entry was enciphered under.
func buildGofileScript(plain, keys []string, rotate, base int) string {
	n := len(plain)
	encoded := make([]string, n)
	for i, p := range plain {
		enc := gofileRC4Runes(p, keys[i])
		encoded[i] = base64.NewEncoding(gofileTestAlphabet[:64]).WithPadding(rune(gofileTestAlphabet[64])).EncodeToString([]byte(enc))
	}
	rotated := make([]string, n)
	for i := range rotated {
		rotated[i] = encoded[(i+rotate)%n]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "function a0_0x5789(){var _0x1f=['%s'];return _0x1f;}\n", strings.Join(rotated, "','"))
	fmt.Fprintf(&b, "function a0_0x4ed8(_0xa,_0xb){_0xa=_0xa-0x%x;var _0xc='%s';}\n", base, gofileTestAlphabet)
	for i, key := range keys {
		fmt.Fprintf(&b, "var _0x%d=_0xalias(0x%x,'%s');\n", i, base+i, key)
	}
	return b.String()
}

func TestGofileSecretFromSyntheticScript(t *testing.T) {
	const secret = "9c1de4a70b2f83"
	table := []string{"userAgent", "createElement", "application/json", "X-Website-Token", secret, "getTime",
		"toString", "substring", "addEventListener", "querySelector", "innerHTML", "https://api.gofile.io"}
	keys := []string{"C@M)", "XMs%", "J)Qr", "Du4t", "kL2#", "9zPq", "aB!7", "mN$3", "wE6^", "rT8&", "yU0*", "iO5("}
	for _, rotate := range []int{0, 1, 7, 11} {
		got, err := gofileSecretFrom(buildGofileScript(table, keys, rotate, 0x1de))
		if err != nil || got != secret {
			t.Errorf("rotate %d: %q, %v", rotate, got, err)
		}
	}
}

func TestGofileSecretFromRefusesToGuess(t *testing.T) {
	keys := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff", "gggg", "hhhh", "iiii", "jjjj"}
	filler := []string{"userAgent", "createElement", "application/json", "X-Website-Token", "getTime", "toString", "substring", "addEventListener"}
	two := append([]string{"9c1de4a70b2f83", "0af31c9e5d7b26"}, filler...)
	if _, err := gofileSecretFrom(buildGofileScript(two, keys, 3, 0x1de)); err == nil {
		t.Error("two secret-shaped strings were resolved to one answer")
	}
	none := append([]string{"hello", "world"}, filler...)
	if _, err := gofileSecretFrom(buildGofileScript(none, keys, 3, 0x1de)); err == nil {
		t.Error("a script without a secret gave one")
	}
	if _, err := gofileSecretFrom("var x = 1;"); err == nil {
		t.Error("garbage gave a secret")
	}
}

// The saved script is a real copy of Gofile's wt.obf.js (taken from HeapLeach's test data).
func TestGofileSecretFromSavedScript(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("testdata", "gofile-wt.obf.js"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := gofileSecretFrom(string(script))
	if err != nil {
		t.Fatalf("gofileSecretFrom: %v", err)
	}
	if got != "12af056dacea0b" {
		t.Errorf("recovered %q", got)
	}
}

func TestGofileWebsiteToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	slot := strconv.FormatInt(1_700_000_000/(4*3600), 10)
	sum := sha256.Sum256([]byte("UA::en-US::tok::" + slot + "::sec"))
	want := hex.EncodeToString(sum[:])
	if got := gofileWebsiteToken("UA", "en-US", "tok", "sec", now); got != want {
		t.Errorf("got %s want %s", got, want)
	}
	if gofileWebsiteToken("UA", "en-US", "tok", "sec", now.Add(4*time.Hour)) == want {
		t.Error("the signature must change with the 4-hour slot")
	}
}

// --- end to end against a fake Gofile ---------------------------------------

type fakeGofile struct {
	mu        sync.Mutex
	t         *testing.T
	secret    string
	script    string // served as /js/wt.obf.js
	accounts  int
	contents  int
	badSig    bool // answer every content request as wrongly signed
	files     map[string][]byte
	srv       *httptest.Server
	downloads map[string]int
}

func newFakeGofile(t *testing.T) *fakeGofile {
	g := &fakeGofile{t: t, files: map[string][]byte{}, downloads: map[string]int{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	gofileAPIBase, gofileSiteBase = g.srv.URL+"/api", g.srv.URL
	gofileMinInterval, gofileRetryPause = 0, time.Millisecond
	gofileState.reset()
	t.Cleanup(func() { g.srv.Close(); gofileState.reset() })
	return g
}

func (g *fakeGofile) json(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (g *fakeGofile) file(id, name string) map[string]any {
	return map[string]any{"id": id, "type": "file", "name": name, "size": len(g.files[id]), "createTime": 1700000000,
		"link": g.srv.URL + "/download/" + id + "/" + name, "servers": []string{"store1"}}
}

func (g *fakeGofile) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/js/wt.obf.js":
		if g.script == "" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(g.script))
	case path == "/api/accounts" && r.Method == http.MethodPost:
		g.accounts++
		g.json(w, 200, map[string]any{"status": "ok", "data": map[string]any{"token": "guest" + strconv.Itoa(g.accounts)}})
	case strings.HasPrefix(path, "/api/contents/"):
		g.contents++
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		want := gofileWebsiteToken(gofileUserAgent, gofileLanguage, token, g.secret, time.Now())
		if g.badSig || r.Header.Get("X-Website-Token") != want {
			g.json(w, 401, map[string]any{"status": "error-notPremium", "data": map[string]any{}})
			return
		}
		g.contentsFor(w, strings.TrimPrefix(path, "/api/contents/"))
	case strings.HasPrefix(path, "/download/"):
		parts := strings.Split(strings.TrimPrefix(path, "/download/"), "/")
		data, ok := g.files[parts[0]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if c, err := r.Cookie(gofileCookie); err != nil || !strings.HasPrefix(c.Value, "guest") {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		g.downloads[parts[0]]++
		var s, e int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e); err != nil || s > e || e >= len(data) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[s : e+1])
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGofile) contentsFor(w http.ResponseWriter, id string) {
	folder := func(id, name string, kids ...map[string]any) map[string]any {
		children, ids := map[string]any{}, []string{}
		for _, k := range kids {
			children[k["id"].(string)] = k
			ids = append(ids, k["id"].(string))
		}
		return map[string]any{"id": id, "type": "folder", "name": name, "canAccess": true, "children": children, "childrenIds": ids}
	}
	ok := func(data map[string]any) { g.json(w, 200, map[string]any{"status": "ok", "data": data, "metadata": map[string]any{"hasNextPage": false}}) }
	switch id {
	case "AbCd12":
		sub := map[string]any{"id": "sub-1", "type": "folder", "name": "Nested"}
		ok(folder("root-1", "My mods", g.file("f1", "A.zipmod"), g.file("f2", "readme.txt"), g.file("f3", "NotAZip.zip"), sub))
	case "sub-1":
		ok(folder("sub-1", "Nested", g.file("f4", "B.ZIPMOD")))
	case "Locked":
		g.json(w, 200, map[string]any{"status": "ok", "data": map[string]any{"id": "x", "type": "folder", "canAccess": false, "passwordStatus": "passwordRequired"}})
	case "Gone01":
		g.json(w, 200, map[string]any{"status": "error-notFound", "data": map[string]any{}})
	default:
		http.NotFound(w, nil)
	}
}

func writeGofileConfig(t *testing.T, dir string, groups ...*linkGroup) string {
	t.Helper()
	path := filepath.Join(dir, "config", "gofile.json")
	if err := (&hostedConfig{Groups: groups}).save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func gofileOpts(t *testing.T, links ...string) (hostedOptions, string) {
	dir := t.TempDir()
	return hostedOptions{
		configPath: writeGofileConfig(t, dir, &linkGroup{Name: "manual", Links: links}),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 3,
	}, dir
}

func TestGofileEndToEnd(t *testing.T) {
	t.Setenv(gofileSecretEnv, "")
	g := newFakeGofile(t)
	// The secret is read from the script, not configured.
	const secret = "9c1de4a70b2f83"
	g.secret = secret
	table := []string{"userAgent", "createElement", "application/json", "X-Website-Token", secret, "getTime",
		"toString", "substring", "addEventListener", "querySelector", "innerHTML", "https://api.gofile.io"}
	keys := []string{"C@M)", "XMs%", "J)Qr", "Du4t", "kL2#", "9zPq", "aB!7", "mN$3", "wE6^", "rT8&", "yU0*", "iO5("}
	g.script = buildGofileScript(table, keys, 5, 0x1de)
	g.files["f1"] = makeZipmod(t, "com.test.a", "1.0", false)
	g.files["f2"] = []byte("hi")
	g.files["f3"] = []byte("this is not a zip file at all")
	g.files["f4"] = makeZipmod(t, "com.test.big", "2.0", true)

	opt, dir := gofileOpts(t, "https://gofile.io/d/AbCd12")
	newGuids, failures := buildHostedIndexes(opt, gofileProvider{})
	if newGuids != 2 || failures != 0 {
		t.Fatalf("newGuids=%d failures=%d", newGuids, failures)
	}
	data, err := os.ReadFile(filepath.Join(dir, "data", "gofile", "My mods.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx modpackIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"com.test.a":   "https://gofile.io/d/AbCd12#f1",
		"com.test.big": "https://gofile.io/d/AbCd12#f4",
	}
	for guid, u := range want {
		if len(idx.Guids[guid]) != 1 || idx.Guids[guid][0] != u {
			t.Errorf("%s: %v want %s", guid, idx.Guids[guid], u)
		}
	}
	if g.downloads["f2"] != 0 {
		t.Error("readme.txt must not be downloaded")
	}

	// Second run: everything is cached, nothing is downloaded again.
	g.mu.Lock()
	g.downloads = map[string]int{}
	g.mu.Unlock()
	if n, f := buildHostedIndexes(opt, gofileProvider{}); n != 0 || f != 0 {
		t.Fatalf("second run: %d %d", n, f)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, n := range g.downloads {
		if id != "f3" { // the one recorded as unindexable is not retried either, but be lenient
			t.Errorf("second run downloaded %s (%d requests)", id, n)
		}
	}
}

func TestGofileFallsBackWhenScriptFails(t *testing.T) {
	t.Setenv(gofileSecretEnv, "")
	g := newFakeGofile(t) // no script: 404
	g.secret = gofileFallbackSecret
	g.files["f1"] = makeZipmod(t, "com.test.a", "1.0", false)
	g.files["f4"] = makeZipmod(t, "com.test.big", "2.0", true)
	opt, _ := gofileOpts(t, "https://gofile.io/d/AbCd12")
	if n, f := buildHostedIndexes(opt, gofileProvider{}); n != 2 || f != 0 {
		t.Fatalf("newGuids=%d failures=%d", n, f)
	}
}

func TestGofileSecretEnvOverrides(t *testing.T) {
	t.Setenv(gofileSecretEnv, "feedfacefeed00")
	g := newFakeGofile(t)
	g.secret = "feedfacefeed00"
	g.script = "not a script"
	g.files["f1"] = makeZipmod(t, "com.test.a", "1.0", false)
	g.files["f4"] = makeZipmod(t, "com.test.big", "2.0", true)
	opt, _ := gofileOpts(t, "https://gofile.io/d/AbCd12")
	if n, f := buildHostedIndexes(opt, gofileProvider{}); n != 2 || f != 0 {
		t.Fatalf("newGuids=%d failures=%d", n, f)
	}
}

// A refused signature must cost one request, not one per link, and must fail the run.
func TestGofileRejectedSignatureStopsTheRun(t *testing.T) {
	t.Setenv(gofileSecretEnv, "")
	g := newFakeGofile(t)
	g.badSig = true
	opt, _ := gofileOpts(t, "https://gofile.io/d/AbCd12", "https://gofile.io/d/Zzzzzz", "https://gofile.io/d/Yyyyyy")
	_, failures := buildHostedIndexes(opt, gofileProvider{})
	if failures == 0 {
		t.Error("a refused signature must be reported as a failure")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.contents != 1 {
		t.Errorf("%d content requests were sent after the signature was refused, want 1", g.contents)
	}
}

func TestGofilePurgeAndPasswords(t *testing.T) {
	t.Setenv(gofileSecretEnv, "")
	g := newFakeGofile(t)
	g.secret = gofileFallbackSecret
	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeGofileConfig(t, dir,
			&linkGroup{Name: "manual", Links: []string{"https://gofile.io/d/Gone01"}},
			&linkGroup{Name: "forum", Search: "https://example.com/search", Links: []string{"https://gofile.io/d/Gone01", "https://gofile.io/d/Locked"}}),
		dataDir: filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	// "Gone01" is listed in the manual group first, so the scraped duplicate is deduped away;
	// the manual one is never purged, and fails every run.
	_, failures := buildHostedIndexes(opt, gofileProvider{})
	if failures == 0 {
		t.Error("a vanished manual link should be reported")
	}
	cfg, err := loadHostedConfig(opt.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Groups[0].Links; len(got) != 1 {
		t.Errorf("a manual link was purged: %v", got)
	}
	if got := cfg.Groups[1].Links; len(got) != 0 {
		t.Errorf("the password protected link of a scraped group should be purged: %v", got)
	}
}
