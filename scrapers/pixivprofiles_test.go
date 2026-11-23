package scrapers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testPixivProfiles = `{
  "1": {"available": true, "comment": "scenes: ` + testMegaFolder2 + ` (thanks)", "checked_at": "2026-11-01T00:00:00Z",
        "links": [{"url": "https://www.patreon.com/x", "source": "comment"}]},
  "2": {"available": true, "comment": "", "checked_at": "2026-11-02T00:00:00Z",
        "links": [{"url": "` + testGofile + `", "source": "webpage"}, {"url": "` + testMegaFolder + `", "source": "webpage"}]},
  "3": {"available": false, "error": "not found", "checked_at": "2026-11-03T00:00:00Z", "links": []}
}`

func writeTestProfiles(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFilePixivLinks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "profiles.json")
	writeTestProfiles(t, src, testPixivProfiles)
	profiles, err := loadPixivProfiles(src)
	if err != nil || len(profiles) != 3 {
		t.Fatalf("%v %v", profiles, err)
	}

	path := writeTestConfig(t, `{"groups":[{"name":"manual","links":["`+testMegaFolder+`"]}]}`)
	added, err := filePixivLinks(path, forumMega, profiles, false)
	if err != nil || added != 1 { // folder 1 is in the manual group already
		t.Fatalf("added=%d err=%v, want 1", added, err)
	}
	cfg, _ := loadHostedConfig(path)
	g := cfg.Groups[1]
	if g.Source != pixivSource || g.Name != pixivSource || len(g.Links) != 1 || g.Links[0] != testMegaFolder2 {
		t.Fatalf("group = %+v", g)
	}
	if g.NewestProfile != "2026-11-03T00:00:00Z" || !g.scraped() || g.searched() {
		t.Errorf("group = %+v", g)
	}

	// A purged link must not come back while its profile is below the watermark...
	g.Links = nil
	if err := cfg.save(path); err != nil {
		t.Fatal(err)
	}
	if added, err = filePixivLinks(path, forumMega, profiles, false); err != nil || added != 0 {
		t.Errorf("purged link re-added: added=%d err=%v", added, err)
	}
	// ...unless everything is re-read.
	if added, _ = filePixivLinks(path, forumMega, profiles, true); added != 1 {
		t.Errorf("-full must read every profile, added=%d", added)
	}
	// A profile read again by the author scraper (newer checked_at) is read again.
	cfg, _ = loadHostedConfig(path)
	cfg.Groups[1].Links = nil
	_ = cfg.save(path)
	p := profiles["1"]
	p.CheckedAt = "2026-11-10T00:00:00Z"
	profiles["1"] = p
	if added, _ = filePixivLinks(path, forumMega, profiles, false); added != 1 {
		t.Errorf("a profile checked again must be read, added=%d", added)
	}
	// The other hosts only get their own links.
	gpath := writeTestConfig(t, `{"groups":[{"name":"manual","links":[]}]}`)
	if added, _ = filePixivLinks(gpath, forumGofile, profiles, false); added != 1 {
		t.Errorf("gofile added=%d, want 1", added)
	}
}

func TestFilePixivLinksLeavesUnchangedConfigAlone(t *testing.T) {
	path := writeTestConfig(t, `{"groups":[{"name":"x","links":[]}]}`)
	profiles := map[string]pixivProfile{"1": {CheckedAt: "2026-11-01T00:00:00Z"}}
	if _, err := filePixivLinks(path, forumGofile, profiles, false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	st1, _ := os.Stat(path)
	time.Sleep(20 * time.Millisecond)
	if added, err := filePixivLinks(path, forumGofile, profiles, false); err != nil || added != 0 {
		t.Fatal(added, err)
	}
	after, _ := os.ReadFile(path)
	st2, _ := os.Stat(path)
	if string(before) != string(after) || !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("an unchanged config was rewritten")
	}
}

func TestRunPixivDownloadsAndFilesLinks(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "pixiv")
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mega", "gofile"} {
		writeTestProfiles(t, filepath.Join(cfgDir, k+".json"), `{"groups":[{"name":"manual","links":[]}]}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testPixivProfiles))
	}))
	defer srv.Close()
	args := []string{"-pixiv-dir", cacheDir, "-config-dir", cfgDir, "-pixiv-url", srv.URL}

	added, failures := RunPixiv(args, []string{"mega", "gofile"})
	if added != 3 || failures != 0 { // two mega folders and one gofile
		t.Fatalf("added=%d failures=%d, want 3 and 0", added, failures)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "profiles.json")); err != nil {
		t.Errorf("profiles.json was not downloaded: %v", err)
	}
	if added, _ = RunPixiv(append(args, "-full"), []string{"gofile"}); added != 0 {
		t.Errorf("already filed, added=%d", added)
	}
}

func TestDownloadPixivProfilesRejectsGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>no</html>")) }))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "profiles.json")
	writeTestProfiles(t, path, testPixivProfiles)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := downloadFile(ctx, srv.URL, path, func(p string) error { _, err := loadPixivProfiles(p); return err })
	if err == nil {
		t.Fatal("garbage must not be accepted")
	}
	if m, err := loadPixivProfiles(path); err != nil || len(m) != 3 {
		t.Errorf("good copy replaced: %v %v", m, err)
	}
}
