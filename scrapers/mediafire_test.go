package scrapers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestParseMediaFireLinks(t *testing.T) {
	for _, s := range []string{
		"https://www.mediafire.com/folder/abc123def4567",
		"http://mediafire.com/folder/abc123def4567/My_Mods",
		"https://www.mediafire.com/folder/abc123def4567/My_Mods?dkey=1#x",
		"https://m.mediafire.com/folder/abc123def4567",
	} {
		if k, err := parseMediaFireFolderLink(s); err != nil || k != "abc123def4567" {
			t.Errorf("folder %q: %q, %v", s, k, err)
		}
		if _, err := parseMediaFireFileLink(s); err == nil {
			t.Errorf("%q is a folder, not a file", s)
		}
		if _, err := (mediafireProvider{}).ID(s); err != nil {
			t.Errorf("%q should be a folder link: %v", s, err)
		}
	}
	for _, s := range []string{
		"https://www.mediafire.com/file/qk1234567890abc",
		"https://www.mediafire.com/file/qk1234567890abc/Mod.zipmod/file",
		"https://www.mediafire.com/file_premium/qk1234567890abc/Mod.zipmod/file",
		"https://www.mediafire.com/view/qk1234567890abc/Mod.zipmod",
		"https://www.mediafire.com/download/qk1234567890abc",
		"HTTPS://WWW.MEDIAFIRE.COM/file/qk1234567890abc?x=1",
	} {
		if k, err := parseMediaFireFileLink(s); err != nil || k != "qk1234567890abc" {
			t.Errorf("file %q: %q, %v", s, k, err)
		}
		if _, err := parseMediaFireFolderLink(s); err == nil {
			t.Errorf("%q is a file, not a folder", s)
		}
		if _, err := (mediafireProvider{}).ID(s); err == nil {
			t.Errorf("%q must not be taken for a folder", s)
		}
		if _, err := (mediafireProvider{}).FileID(s); err != nil {
			t.Errorf("%q should be a file link: %v", s, err)
		}
	}
	for _, s := range []string{
		"", "https://www.mediafire.com/", "https://www.mediafire.com/folder/", "https://www.mediafire.com/folder/short",
		"https://www.mediafire.com/folder/waytoolongtobeakeyatallreally",
		"https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg", "https://example.com/folder/abc123def4567",
		"https://notmediafire.com/folder/abc123def4567",
	} {
		if _, err := parseMediaFireFolderLink(s); err == nil {
			t.Errorf("%q should be rejected as a folder", s)
		}
		if _, err := parseMediaFireFileLink(s); err == nil {
			t.Errorf("%q should be rejected as a file", s)
		}
	}
}

func TestFindMediaFireLinks(t *testing.T) {
	got := findMediaFireLinks(`Pack: https://www.mediafire.com/folder/abc123def4567/Pack, single
	(https://www.mediafire.com/file/qk1234567890abc/Mod.zipmod/file), and https://mediafire.com/folder/abc123def4567.
	Not these: https://www.mediafire.com/folder/waytoolongtobeakeyatallreally or https://www.mediafire.com/pricing`)
	want := []string{
		"https://www.mediafire.com/folder/abc123def4567",
		"https://www.mediafire.com/folder/abc123def4567",
		"https://www.mediafire.com/file/qk1234567890abc",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v want %v", got, want)
	}
	// wrapped in a redirect URL, and de-duplicated by linksIn
	l := forumMediaFire.linksIn([]string{
		"https://kknow.cc/proxy.php?link=https%3A%2F%2Fwww.mediafire.com%2Ffile%2Fqk1234567890abc%2FMod.zipmod%2Ffile&hash=1",
		"https://www.mediafire.com/file/qk1234567890abc/Mod.zipmod/file",
	})
	if len(l) != 1 || l[0] != "https://www.mediafire.com/file/qk1234567890abc" {
		t.Errorf("proxied: %v", l)
	}
	// the dedupe id tells folders and files apart
	a, _ := forumMediaFire.id("https://www.mediafire.com/folder/abc123def4567")
	b, _ := forumMediaFire.id("https://www.mediafire.com/file/abc123def4567")
	if a == b {
		t.Errorf("a folder and a file with the same key must not be duplicates: %q %q", a, b)
	}
}

func TestParseMediaFireDownloadPage(t *testing.T) {
	scrambled := base64.StdEncoding.EncodeToString([]byte("https://download1234.mediafire.com/abc/qk/Mod.zipmod"))
	for name, tc := range map[string]struct{ page, want string }{
		"plain": {`<html><a class="input popsok" id="downloadButton" href="https://download1111.mediafire.com/x/qk/A.zipmod">Download</a></html>`,
			"https://download1111.mediafire.com/x/qk/A.zipmod"},
		"scrambled": {`<a id="downloadButton" href="javascript:void(0)" data-scrambled-url="` + scrambled + `">Download</a>`,
			"https://download1234.mediafire.com/abc/qk/Mod.zipmod"},
		"only in a script": {`<script>var u = "https://download9.mediafire.com/y/qk/B.zipmod";</script>`,
			"https://download9.mediafire.com/y/qk/B.zipmod"},
	} {
		got, err := parseMediaFireDownloadPage([]byte(tc.page))
		if err != nil || got != tc.want {
			t.Errorf("%s: %q, %v (want %q)", name, got, err, tc.want)
		}
	}

	_, err := parseMediaFireDownloadPage([]byte(`<html><div class="error">Invalid or Deleted File.</div></html>`))
	var me *mediafireError
	if !errors.As(err, &me) || !me.permanent() {
		t.Errorf("a deleted file should be a permanent error: %v", err)
	}
	_, err = parseMediaFireDownloadPage([]byte(`<html>please solve this captcha</html>`))
	if err == nil || errors.As(err, &me) {
		t.Errorf("a page without a link is a plain, retried error: %v", err)
	}
}

func TestMediaFireErrors(t *testing.T) {
	for _, e := range []*mediafireError{
		{Status: 404},
		{Code: "110", Message: "Unknown or Invalid QuickKey"},
		{Code: "112", Message: "Unknown or invalid FolderKey"},
		{Code: "999", Message: "Unknown or invalid thing"},
	} {
		if !e.permanent() {
			t.Errorf("%v should be permanent", e)
		}
	}
	for _, e := range []*mediafireError{{Status: 403}, {Status: 429}, {Status: 502}, {Code: "105", Message: "Invalid session"}} {
		if e.permanent() {
			t.Errorf("%v must not purge anything", e)
		}
	}
	if !(&mediafireError{Status: 429}).retryable() || !(&mediafireError{Status: 503}).retryable() || (&mediafireError{Status: 403}).retryable() {
		t.Error("429 and 5xx are retried, 403 is not")
	}
	if !isGoneError(fmt.Errorf("wrapped: %w", &mediafireError{Status: 404})) || isGoneError(&mediafireError{Status: 403}) {
		t.Error("isGoneError should follow permanent()")
	}
}

// ---------- fake MediaFire ----------

type fakeMFFile struct {
	key, name, created string
	content            []byte
	password           bool
	scrambled          bool // the page hides the link in data-scrambled-url
	captcha            bool // the page has no download link
	deleted            bool // the page says the file was deleted
	directFails        int  // this many download requests are refused (403) before they work
	directStatus       int  // every download request is answered with this status
}

type fakeMFFolder struct {
	key, name string
	files     []string // quick keys
	folders   []string // folder keys
}

type fakeMediaFire struct {
	mu         sync.Mutex
	files      map[string]*fakeMFFile
	folders    map[string]*fakeMFFolder
	pageHits   map[string]int
	directHits map[string]int
	chunk      int // files per chunk
	srv        *httptest.Server
	t          *testing.T
}

func newFakeMediaFire(t *testing.T) *fakeMediaFire {
	m := &fakeMediaFire{files: map[string]*fakeMFFile{}, folders: map[string]*fakeMFFolder{}, pageHits: map[string]int{}, directHits: map[string]int{}, chunk: 2, t: t}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	mediafireAPIBase = m.srv.URL + "/api/1.5"
	mediafirePageBase = m.srv.URL
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMediaFire) addFile(f *fakeMFFile) *fakeMFFile {
	if f.created == "" {
		f.created = "2024-02-15 16:30:51"
	}
	m.files[f.key] = f
	return f
}

func (m *fakeMediaFire) addFolder(key, name string, files, folders []string) {
	m.folders[key] = &fakeMFFolder{key: key, name: name, files: files, folders: folders}
}

func mfWrite(w http.ResponseWriter, resp map[string]any) {
	resp["result"] = "Success"
	json.NewEncoder(w).Encode(map[string]any{"response": resp})
}

func mfFail(w http.ResponseWriter, code, msg string) {
	json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": "Error", "error": code, "message": msg}})
}

func (m *fakeMediaFire) fileJSON(f *fakeMFFile) map[string]any {
	pw := "no"
	if f.password {
		pw = "yes"
	}
	return map[string]any{"quickkey": f.key, "filename": f.name, "size": strconv.Itoa(len(f.content)), "created": f.created, "password_protected": pw}
}

func (m *fakeMediaFire) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/api/1.5/folder/get_info.php":
		f := m.folders[q.Get("folder_key")]
		if f == nil {
			mfFail(w, "112", "Unknown or invalid FolderKey")
			return
		}
		mfWrite(w, map[string]any{"folder_info": map[string]any{"folderkey": f.key, "name": f.name}})
	case r.URL.Path == "/api/1.5/folder/get_content.php":
		f := m.folders[q.Get("folder_key")]
		if f == nil {
			mfFail(w, "112", "Unknown or invalid FolderKey")
			return
		}
		if q.Get("response_format") != "json" {
			m.t.Errorf("response_format missing: %s", r.URL)
		}
		content := map[string]any{}
		resp := map[string]any{"folder_content": content, "more_chunks": "no"}
		if q.Get("content_type") == "folders" {
			subs := []any{}
			for _, k := range f.folders {
				subs = append(subs, map[string]any{"folderkey": k, "name": m.folders[k].name})
			}
			content["folders"] = subs
		} else {
			chunk, _ := strconv.Atoi(q.Get("chunk"))
			lo, hi := (chunk-1)*m.chunk, chunk*m.chunk
			files := []any{}
			for i, k := range f.files {
				if i >= lo && i < hi {
					files = append(files, m.fileJSON(m.files[k]))
				}
			}
			content["files"] = files
			if hi < len(f.files) {
				resp["more_chunks"] = "yes"
			}
		}
		mfWrite(w, resp)
	case r.URL.Path == "/api/1.5/file/get_info.php":
		f := m.files[q.Get("quick_key")]
		if f == nil {
			mfFail(w, "110", "Unknown or Invalid QuickKey")
			return
		}
		mfWrite(w, map[string]any{"file_info": m.fileJSON(f)})
	case strings.HasPrefix(r.URL.Path, "/file/"):
		qk := strings.TrimPrefix(r.URL.Path, "/file/")
		f := m.files[qk]
		m.pageHits[qk]++
		switch {
		case f == nil || f.deleted:
			fmt.Fprint(w, `<html><div class="error">Invalid or Deleted File.</div></html>`)
		case f.captcha:
			fmt.Fprint(w, `<html>please solve this captcha</html>`)
		case f.scrambled:
			fmt.Fprintf(w, `<a id="downloadButton" href="javascript:void(0)" data-scrambled-url="%s">Download</a>`,
				base64.StdEncoding.EncodeToString([]byte(m.srv.URL+"/dl/"+qk+"/"+f.name)))
		default:
			fmt.Fprintf(w, `<a class="input popsok" id="downloadButton" href="%s/dl/%s/%s">Download</a>`, m.srv.URL, qk, f.name)
		}
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		qk := strings.Split(strings.TrimPrefix(r.URL.Path, "/dl/"), "/")[0]
		f := m.files[qk]
		m.directHits[qk]++
		if f == nil {
			http.NotFound(w, r)
			return
		}
		if f.directStatus != 0 {
			http.Error(w, "refused", f.directStatus)
			return
		}
		if f.directFails > 0 {
			f.directFails--
			http.Error(w, "link expired", http.StatusForbidden)
			return
		}
		var s, e int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e); err != nil || s > e || e >= len(f.content) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(f.content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(f.content[s : e+1])
	default:
		http.NotFound(w, r)
	}
}

func writeMediaFireConfig(t *testing.T, dir string, groups ...*linkGroup) string {
	t.Helper()
	path := filepath.Join(dir, "config", "mediafire.json")
	if err := (&hostedConfig{Groups: groups}).save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func readMediaFireIndex(t *testing.T, dir, name string) modpackIndexFile {
	t.Helper()
	return readMediaFireIndexKind(t, dir, "mediafire", name)
}

// readMediaFireIndexKind reads data/<kind>/<name>.json (shared by the hosted-source tests).
func readMediaFireIndexKind(t *testing.T, dir, kind, name string) modpackIndexFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "data", kind, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx modpackIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestMediaFireFolderEndToEnd(t *testing.T) {
	m := newFakeMediaFire(t) // two files per chunk: the root folder needs three chunks
	m.addFile(&fakeMFFile{key: "fileAAAAAAAAAAA", name: "A.zipmod", content: makeZipmod(t, "com.test.a", "1.0", false)})
	m.addFile(&fakeMFFile{key: "fileBBBBBBBBBBB", name: "B.ZIPMOD", content: makeZipmod(t, "com.test.big", "2.0", true), scrambled: true})
	m.addFile(&fakeMFFile{key: "fileTxtXXXXXXX", name: "readme.txt", content: []byte("hi")})
	m.addFile(&fakeMFFile{key: "fileBadXXXXXXX", name: "NotAZip.zip", content: []byte("this is not a zip file at all")})
	m.addFile(&fakeMFFile{key: "filePassXXXXXX", name: "Secret.zipmod", content: makeZipmod(t, "com.test.secret", "1.0", false), password: true})
	m.addFile(&fakeMFFile{key: "fileExpired000", name: "Expired.zipmod", content: makeZipmod(t, "com.test.expired", "1.0", false), directFails: 1})
	m.addFile(&fakeMFFile{key: "fileCaptcha000", name: "Captcha.zipmod", content: make([]byte, 100), captcha: true})
	m.addFile(&fakeMFFile{key: "fileGoneXXXXXX", name: "Gone.zipmod", content: make([]byte, 100), deleted: true})
	m.addFile(&fakeMFFile{key: "fileRefused000", name: "Refused.zipmod", content: make([]byte, 100), directStatus: 403})
	m.addFile(&fakeMFFile{key: "fileInnerXXXXX", name: "Inner.zipmod", content: makeZipmod(t, "com.test.inner", "3.0", false)})
	m.addFolder("rootfolder0001", "My mods",
		[]string{"fileAAAAAAAAAAA", "fileBBBBBBBBBBB", "fileTxtXXXXXXX", "fileBadXXXXXXX", "filePassXXXXXX", "fileExpired000", "fileCaptcha000", "fileGoneXXXXXX", "fileRefused000"},
		[]string{"subfolder00001"})
	m.addFolder("subfolder00001", "Sub", []string{"fileInnerXXXXX"}, nil)

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeMediaFireConfig(t, dir, &linkGroup{Name: "manual", Links: []string{"https://www.mediafire.com/folder/rootfolder0001/My_mods"}}),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 3,
	}
	cachePath := filepath.Join(dir, "cache", "mediafire", "My mods.json")

	n, f := buildHostedIndexes(opt, mediafireProvider{})
	if n != 4 || f != 0 {
		t.Fatalf("first run: newGuids=%d failures=%d", n, f)
	}
	idx := readMediaFireIndex(t, dir, "My mods")
	want := map[string]string{
		"com.test.a":       "https://www.mediafire.com/file/fileAAAAAAAAAAA",
		"com.test.big":     "https://www.mediafire.com/file/fileBBBBBBBBBBB", // read through a scrambled link
		"com.test.expired": "https://www.mediafire.com/file/fileExpired000",  // its first download link was refused
		"com.test.inner":   "https://www.mediafire.com/file/fileInnerXXXXX",  // in a sub-folder
	}
	if idx.Count != len(want) || len(idx.Guids) != len(want) {
		t.Errorf("index: %+v", idx)
	}
	for g, u := range want {
		if len(idx.Guids[g]) != 1 || idx.Guids[g][0] != u {
			t.Errorf("%s: %v want %s", g, idx.Guids[g], u)
		}
	}
	if m.pageHits["fileExpired000"] != 2 {
		t.Errorf("an expired download link should be resolved again once: %d page loads", m.pageHits["fileExpired000"])
	}

	cache := loadCache(cachePath)
	for _, key := range []string{"fileBadXXXXXXX", "fileGoneXXXXXX"} {
		if e := cache[mediafireFileURL(key)]; e.Error == "" || e.Guid != "" {
			t.Errorf("%s should be recorded as unindexable: %+v", key, e)
		}
	}
	for _, key := range []string{"fileCaptcha000", "fileRefused000"} {
		if _, ok := cache[mediafireFileURL(key)]; ok {
			t.Errorf("%s: a captcha or refused download must not be cached (it is retried next run)", key)
		}
	}
	for _, key := range []string{"fileTxtXXXXXXX", "filePassXXXXXX"} {
		if _, ok := cache[mediafireFileURL(key)]; ok {
			t.Errorf("%s should have been ignored", key)
		}
	}

	// Second run: only the files that failed for a temporary reason are tried again.
	m.mu.Lock()
	m.pageHits, m.directHits = map[string]int{}, map[string]int{}
	m.mu.Unlock()
	if n, f := buildHostedIndexes(opt, mediafireProvider{}); n != 0 || f != 0 {
		t.Fatalf("second run: %d %d", n, f)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.pageHits {
		if key != "fileCaptcha000" && key != "fileRefused000" {
			t.Errorf("second run loaded the page of %s again", key)
		}
	}
	if m.pageHits["fileCaptcha000"] == 0 || m.pageHits["fileRefused000"] == 0 {
		t.Error("the files that failed temporarily were not retried")
	}
}

func TestMediaFireSingleFilesAndPurge(t *testing.T) {
	m := newFakeMediaFire(t)
	m.addFile(&fakeMFFile{key: "modFileAAAAAAA", name: "Mod.zipmod", content: makeZipmod(t, "com.test.single", "1.0", false)})
	m.addFile(&fakeMFFile{key: "cardFileXXXXXX", name: "card.png", content: []byte("png")})
	m.addFile(&fakeMFFile{key: "packFileXXXXXX", name: "pack.zip", content: []byte("this is not a zip file at all")})
	m.addFolder("emptyfolder001", "Cards only", []string{"cardFileXXXXXX"}, nil)

	dir := t.TempDir()
	scraped := &linkGroup{Name: "kknow.cc", Search: "https://kknow.cc/search/?q=x", Links: []string{
		"https://www.mediafire.com/file/modFileAAAAAAA/Mod.zipmod/file",
		"https://www.mediafire.com/file/cardFileXXXXXX/card.png/file", // not a mod archive: purged
		"https://www.mediafire.com/file/packFileXXXXXX/pack.zip/file", // a zip that is no mod: purged
		"https://www.mediafire.com/file/missingkey0000/x.zipmod/file", // gone (error 110): purged
		"https://www.mediafire.com/folder/emptyfolder001/Cards_only",  // no mods: purged
		"https://www.mediafire.com/folder/missingfolder01/Gone",       // gone (error 112): purged
	}}
	opt := hostedOptions{
		configPath: writeMediaFireConfig(t, dir, scraped),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	n, f := buildHostedIndexes(opt, mediafireProvider{})
	if n != 1 || f != 0 {
		t.Fatalf("newGuids=%d failures=%d", n, f)
	}
	idx := readMediaFireIndex(t, dir, singleFilesName)
	if u := idx.Guids["com.test.single"]; len(u) != 1 || u[0] != "https://www.mediafire.com/file/modFileAAAAAAA" {
		t.Errorf("single file: %v", idx.Guids)
	}
	cfg, err := loadHostedConfig(opt.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if links := cfg.allLinks(); len(links) != 1 || !strings.Contains(links[0], "modFileAAAAAAA") {
		t.Errorf("links left after purging: %v", links)
	}
}

func TestMediaFireHandMaintainedLinksAreNeverPurged(t *testing.T) {
	m := newFakeMediaFire(t)
	_ = m
	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeMediaFireConfig(t, dir, &linkGroup{Name: "manual", Links: []string{"https://www.mediafire.com/folder/missingfolder01/Gone"}}),
		dataDir:    filepath.Join(dir, "data"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	if _, f := buildHostedIndexes(opt, mediafireProvider{}); f != 1 {
		t.Errorf("a dead link in a manual group is a failure, got %d", f)
	}
	cfg, err := loadHostedConfig(opt.configPath)
	if err != nil || len(cfg.allLinks()) != 1 {
		t.Errorf("manual link must stay: %v %v", cfg, err)
	}
}
