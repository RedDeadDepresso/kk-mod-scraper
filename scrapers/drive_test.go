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

func TestParseDriveFolderLink(t *testing.T) {
	for _, s := range []string{
		"https://drive.google.com/drive/folders/1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu",
		"https://drive.google.com/drive/folders/1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu?usp=sharing",
		"https://drive.google.com/drive/u/0/folders/1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu",
		"https://drive.google.com/drive/mobile/folders/1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu",
		"https://drive.google.com/open?id=1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu",
	} {
		if id, err := parseDriveFolderLink(s); err != nil || id != "1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu" {
			t.Errorf("%q: %q, %v", s, id, err)
		}
	}
	for _, s := range []string{"", "https://drive.google.com/file/d/1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu/view", "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg", "https://drive.google.com/drive/folders/"} {
		if _, err := parseDriveFolderLink(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}

func TestParseDriveError(t *testing.T) {
	body := []byte(`{"error":{"code":403,"message":"quota","errors":[{"reason":"downloadQuotaExceeded"}]}}`)
	if e := parseDriveError(403, body); e.Reason != "downloadQuotaExceeded" || e.permanent() {
		t.Errorf("quota: %+v permanent=%v", e, e.permanent())
	}
	if e := parseDriveError(403, []byte(`{"error":{"message":"no","errors":[{"reason":"cannotDownloadFile"}]}}`)); !e.permanent() {
		t.Error("cannotDownloadFile should be permanent")
	}
	if e := parseDriveError(404, []byte("<html>")); !e.permanent() || e.Message == "" {
		t.Errorf("404: %+v", e)
	}
	if e := parseDriveError(416, nil); e.permanent() || e.retryable() {
		t.Error("416 should neither be permanent nor retried")
	}
	if e := parseDriveError(503, nil); e.permanent() || !e.retryable() {
		t.Error("503 should be retried, not permanent")
	}
}

// ---------- fake Drive ----------

type fakeDriveFile struct {
	id, name, mime, modified string
	content                  []byte
	hasSize                  bool
	status                   int    // non-zero: answer media requests with this error
	reason                   string // error reason for that status
}

type fakeDrive struct {
	mu        sync.Mutex
	folders   map[string][]string // folder id -> child ids (files or folders)
	names     map[string]string   // folder id -> name
	files     map[string]*fakeDriveFile
	mediaGets map[string]int
	pageSize  int
	srv       *httptest.Server
}

func newFakeDrive(t *testing.T) *fakeDrive {
	d := &fakeDrive{folders: map[string][]string{}, names: map[string]string{}, files: map[string]*fakeDriveFile{}, mediaGets: map[string]int{}, pageSize: 2}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	driveAPIBase = d.srv.URL
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDrive) addFile(parent string, f *fakeDriveFile) {
	if f.modified == "" {
		f.modified = "2024-02-15T16:30:51.000Z"
	}
	f.hasSize = f.mime == "" || !strings.HasPrefix(f.mime, "application/vnd.google-apps.")
	d.files[f.id] = f
	d.folders[parent] = append(d.folders[parent], f.id)
}

func (d *fakeDrive) addFolder(parent, id, name string) {
	d.names[id] = name
	d.folders[parent] = append(d.folders[parent], id)
}

func (d *fakeDrive) fail(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": status, "message": "fake " + reason, "errors": []map[string]string{{"reason": reason}},
	}})
}

func (d *fakeDrive) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.Header.Get("X-Goog-Api-Key") != "testkey" {
		d.fail(w, 400, "keyInvalid")
		return
	}
	if strings.Contains(r.URL.RawQuery, "testkey") {
		panic("the API key must not be in the URL")
	}
	q := r.URL.Query()
	if r.URL.Path == "/files" {
		var parent string
		fmt.Sscanf(strings.TrimPrefix(q.Get("q"), "'"), "%s", &parent)
		parent = strings.TrimSuffix(strings.Fields(strings.TrimPrefix(q.Get("q"), "'"))[0], "'")
		ids := d.folders[parent]
		start := 0
		if tok := q.Get("pageToken"); tok != "" {
			fmt.Sscanf(tok, "t%d", &start)
		}
		end := min(start+d.pageSize, len(ids))
		var out []map[string]string
		for _, id := range ids[start:end] {
			if name, isFolder := d.names[id]; isFolder {
				out = append(out, map[string]string{"id": id, "name": name, "mimeType": driveFolderMime})
				continue
			}
			f := d.files[id]
			item := map[string]string{"id": f.id, "name": f.name, "modifiedTime": f.modified}
			if f.mime != "" {
				item["mimeType"] = f.mime
			} else {
				item["mimeType"] = "application/zip"
			}
			if f.hasSize {
				item["size"] = fmt.Sprint(len(f.content))
			}
			out = append(out, item)
		}
		resp := map[string]any{"files": out}
		if end < len(ids) {
			resp["nextPageToken"] = fmt.Sprintf("t%d", end)
		}
		json.NewEncoder(w).Encode(resp)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/files/")
	if q.Get("alt") == "media" {
		f := d.files[id]
		d.mediaGets[id]++
		if f == nil {
			d.fail(w, 404, "notFound")
			return
		}
		if f.status != 0 {
			d.fail(w, f.status, f.reason)
			return
		}
		var s, e int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e); err != nil || s > e || e >= len(f.content) {
			d.fail(w, 416, "badRange")
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(f.content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(f.content[s : e+1])
		return
	}
	if name, ok := d.names[id]; ok {
		json.NewEncoder(w).Encode(map[string]string{"id": id, "name": name, "mimeType": driveFolderMime})
		return
	}
	d.fail(w, 404, "notFound")
}

func TestDriveEndToEnd(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "testkey")
	d := newFakeDrive(t)
	const root = "1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu"
	d.names[root] = "pandaman"
	d.addFile(root, &fakeDriveFile{id: "fileA", name: "A.zipmod", content: makeZipmod(t, "com.test.a", "1.0", false)})
	d.addFile(root, &fakeDriveFile{id: "doc1", name: "notes.zipmod", mime: "application/vnd.google-apps.document"}) // no size: ignored
	d.addFolder(root, "SUBFOLDER1", "sub")
	d.addFile(root, &fakeDriveFile{id: "fileBig", name: "Big.zipmod", content: makeZipmod(t, "com.test.big", "2.0", true)})
	d.addFile(root, &fakeDriveFile{id: "fileGone", name: "Gone.zipmod", content: make([]byte, 100), status: 404, reason: "notFound"})
	d.addFile(root, &fakeDriveFile{id: "fileQuota", name: "Quota.zipmod", content: make([]byte, 100), status: 403, reason: "downloadQuotaExceeded"})
	d.addFile(root, &fakeDriveFile{id: "fileBlocked", name: "Blocked.zipmod", content: make([]byte, 100), status: 403, reason: "cannotDownloadFile"})
	d.addFile("SUBFOLDER1", &fakeDriveFile{id: "fileC", name: "C.ZIPMOD", content: makeZipmod(t, "com.test.c", "1.5", false)})
	d.addFile("SUBFOLDER1", &fakeDriveFile{id: "fileTxt", name: "readme.txt", content: []byte("hi")})
	d.addFile("SUBFOLDER1", &fakeDriveFile{id: "fileBad", name: "NotAZip.zip", content: []byte("this is not a zip file at all")})

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeConfig(t, dir, "https://drive.google.com/drive/folders/"+root+"?usp=sharing"),
		indicesDir: filepath.Join(dir, "indices"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 3,
	}
	indexPath := filepath.Join(dir, "indices", "drive", "pandaman.json")
	cachePath := filepath.Join(dir, "cache", "drive", "pandaman.json")

	newGuids, failures := buildHostedIndexes(opt, driveProvider{})
	if newGuids != 3 || failures != 0 {
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
		"com.test.a":   "https://drive.google.com/file/d/fileA/view",
		"com.test.big": "https://drive.google.com/file/d/fileBig/view",
		"com.test.c":   "https://drive.google.com/file/d/fileC/view", // from the sub-folder
	}
	if idx.Count != 3 {
		t.Errorf("header: %+v", idx)
	}
	for g, u := range want {
		if len(idx.Guids[g]) != 1 || idx.Guids[g][0] != u {
			t.Errorf("%s: %v want %s", g, idx.Guids[g], u)
		}
	}

	cache := loadCache(cachePath)
	for _, id := range []string{"fileGone", "fileBlocked", "fileBad"} {
		if e := cache[driveFileURL(id)]; e.Error == "" || e.Guid != "" {
			t.Errorf("%s should be recorded as unindexable: %+v", id, e)
		}
	}
	if _, ok := cache[driveFileURL("fileQuota")]; ok {
		t.Error("a quota error must not be cached (it should be retried next run)")
	}
	for _, id := range []string{"doc1", "fileTxt"} {
		if _, ok := cache[driveFileURL(id)]; ok {
			t.Errorf("%s should have been ignored", id)
		}
	}

	// Second run: only the quota-limited file is tried again.
	d.mu.Lock()
	d.mediaGets = map[string]int{}
	d.mu.Unlock()
	if n, f := buildHostedIndexes(opt, driveProvider{}); n != 0 || f != 0 {
		t.Fatalf("second run: %d %d", n, f)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, n := range d.mediaGets {
		if id != "fileQuota" {
			t.Errorf("second run downloaded %s again (%d requests)", id, n)
		}
	}
	if d.mediaGets["fileQuota"] == 0 {
		t.Error("the quota-limited file was not retried")
	}
}

func TestDriveKeyAndAccessProblems(t *testing.T) {
	d := newFakeDrive(t)
	const root = "1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu"
	d.names[root] = "pandaman"
	d.addFile(root, &fakeDriveFile{id: "fileA", name: "A.zipmod", content: makeZipmod(t, "com.test.a", "1.0", false)})

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeConfig(t, dir, "https://drive.google.com/drive/folders/"+root),
		indicesDir: filepath.Join(dir, "indices"), cacheDir: filepath.Join(dir, "cache"), maxURLs: 4, workers: 2,
	}
	stale := filepath.Join(dir, "indices", "drive", "keep me.json")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("{}"), 0o644)

	t.Setenv("GOOGLE_API_KEY", "")
	if _, f := buildHostedIndexes(opt, driveProvider{}); f != 1 {
		t.Errorf("a missing key should be a failure, got %d", f)
	}
	t.Setenv("GOOGLE_API_KEY", "wrongkey")
	if _, f := buildHostedIndexes(opt, driveProvider{}); f != 1 {
		t.Errorf("a rejected key should be a failure, got %d", f)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Error("files were cleaned up although the run failed")
	}

	// A folder that doesn't exist (or isn't shared) is a failure with a helpful message.
	t.Setenv("GOOGLE_API_KEY", "testkey")
	opt.configPath = writeConfig(t, dir, "https://drive.google.com/drive/folders/1234567890notshared")
	_, err := driveProvider{}.Load("https://drive.google.com/drive/folders/1234567890notshared")
	if err == nil || !strings.Contains(err.Error(), "Anyone with the link") {
		t.Errorf("unhelpful error: %v", err)
	}
	if _, f := buildHostedIndexes(opt, driveProvider{}); f != 1 {
		t.Errorf("an unshared folder should be a failure, got %d", f)
	}
}
