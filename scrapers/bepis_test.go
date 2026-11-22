package scrapers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testMegaFolder  = "https://mega.nz/folder/AbCdEfGh#0123456789abcdefghijkl"
	testMegaFolder2 = "https://mega.nz/folder/ZyXwVuTs#abcdefghijklmnopqrstuv"
	testGofile      = "https://gofile.io/d/UMLcracT"
)

// writeTestCards writes cards.json the way kk-author-scraper does: a JSON array, one card per
// line, with the "authors" list this scraper ignores.
func writeTestCards(t *testing.T, path string, cards map[int]bepisRecord) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("[\n")
	first := true
	for id := 1; id <= 100; id++ {
		r, ok := cards[id]
		if !ok {
			continue
		}
		if !first {
			sb.WriteString(",\n")
		}
		first = false
		if r.Comments == nil {
			r.Comments = []string{}
		}
		b, err := json.Marshal(struct {
			bepisRecord
			Authors []string `json:"authors"`
		}{r, []string{"someone"}})
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
	}
	sb.WriteString("\n]\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBepisCards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cards.json")
	in := map[int]bepisRecord{
		3: {ID: 3, Bio: "a <b> & \"c\"", Comments: []string{"x", "y\r\nz"}},
		1: {ID: 1, Failed: true},
		2: {ID: 2},
	}
	writeTestCards(t, path, in)
	out, err := loadBepisCards(path)
	if err != nil || len(out) != 3 {
		t.Fatalf("%v %v", out, err)
	}
	if out[3].Bio != in[3].Bio || len(out[3].Comments) != 2 || !out[1].Failed {
		t.Errorf("load changed the data: %+v", out)
	}
	if m, err := loadBepisCards(filepath.Join(t.TempDir(), "none.json")); err != nil || len(m) != 0 {
		t.Errorf("a missing cache must be empty, got %v %v", m, err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"a":1}`), 0o644)
	if _, err := loadBepisCards(bad); err == nil {
		t.Error("a file that is not an array must be an error")
	}
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mega.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileBepisLinks(t *testing.T) {
	path := writeTestConfig(t, `{"groups":[{"name":"manual","links":["`+testMegaFolder+`"]}]}`)
	cards := map[int]bepisRecord{
		1: {ID: 1, Bio: "my mod: " + testMegaFolder},                                        // already in the manual group
		2: {ID: 2, Comments: []string{"nice", "here: " + testMegaFolder2 + ")"}},            // new, from a comment
		3: {ID: 3, Bio: "gofile only " + testGofile},                                        // other host: not for mega
		4: {ID: 4, Bio: "broken " + testMegaFolder2, Failed: true},                          // failed cards are skipped
		5: {ID: 5, Bio: "dup " + strings.Replace(testMegaFolder2, "mega.nz", "mega.nz", 1)}, // duplicate of 2
	}

	added, err := fileBepisLinks(path, forumMega, cards, false)
	if err != nil || added != 1 {
		t.Fatalf("added=%d err=%v, want 1 new link", added, err)
	}
	cfg, _ := loadHostedConfig(path)
	if len(cfg.Groups) != 2 || cfg.Groups[1].Source != bepisSource || cfg.Groups[1].Name != bepisSource {
		t.Fatalf("bepis group missing: %+v", cfg.Groups)
	}
	g := cfg.Groups[1]
	if len(g.Links) != 1 || g.Links[0] != testMegaFolder2 || g.NewestCard != 5 {
		t.Errorf("group = %+v", g)
	}
	if !g.scraped() || g.searched() {
		t.Error("a bepis group is maintained automatically, but never searched on the forum")
	}

	// A link the indexer purged must not come back while its card is below the watermark.
	g.Links = nil
	if err := cfg.save(path); err != nil {
		t.Fatal(err)
	}
	if added, err = fileBepisLinks(path, forumMega, cards, false); err != nil || added != 0 {
		t.Errorf("purged link re-added: added=%d err=%v", added, err)
	}
	// ...unless everything is re-read.
	if added, _ = fileBepisLinks(path, forumMega, cards, true); added != 1 {
		t.Errorf("-full must read every cached card, added=%d", added)
	}

	// New cards above the watermark are read.
	cards[6] = bepisRecord{ID: 6, Bio: "new " + "https://mega.nz/folder/QqQqQqQq#abcdefghijklmnopqrstuv"}
	if added, _ = fileBepisLinks(path, forumMega, cards, false); added != 1 {
		t.Errorf("new card not read, added=%d", added)
	}
}

func TestFileBepisLinksLeavesUnchangedConfigAlone(t *testing.T) {
	path := writeTestConfig(t, `{"groups":[{"name":"x","links":[]}]}`)
	cards := map[int]bepisRecord{1: {ID: 1}}
	if _, err := fileBepisLinks(path, forumGofile, cards, false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	st1, _ := os.Stat(path)
	time.Sleep(20 * time.Millisecond)
	if added, err := fileBepisLinks(path, forumGofile, cards, false); err != nil || added != 0 {
		t.Fatal(added, err)
	}
	after, _ := os.ReadFile(path)
	st2, _ := os.Stat(path)
	if string(before) != string(after) || !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("an unchanged config was rewritten")
	}
}

func newCardsServer(t *testing.T, path string, status *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *status != http.StatusOK {
			http.Error(w, "no", *status)
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
		}
		w.Write(b)
	}))
}

func TestRunBepisDownloadsAndFilesLinks(t *testing.T) {
	dir := t.TempDir()
	cards := filepath.Join(dir, "bepis")
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mega", "gofile"} {
		if err := os.WriteFile(filepath.Join(cfgDir, k+".json"), []byte(`{"groups":[{"name":"manual","links":[]}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// what the server (kk-author-scraper's repository) serves
	served := filepath.Join(dir, "served.json")
	writeTestCards(t, served, map[int]bepisRecord{
		1: {ID: 1, Bio: testMegaFolder + " and " + testGofile},
	})
	status := http.StatusOK
	srv := newCardsServer(t, served, &status)
	defer srv.Close()
	args := []string{"-bepis-dir", cards, "-config-dir", cfgDir, "-bepis-url", srv.URL}

	added, failures := RunBepis(args, []string{"mega", "gofile"})
	if added != 2 || failures != 0 {
		t.Fatalf("added=%d failures=%d, want 2 and 0", added, failures)
	}
	if _, err := os.Stat(filepath.Join(cards, "cards.json")); err != nil {
		t.Errorf("cards.json was not downloaded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cards, "cards.json.tmp")); err == nil {
		t.Error("temp file left behind")
	}

	// only the enabled kinds are filled, and nothing is added twice
	added, _ = RunBepis(append(args, "-full"), []string{"gofile"})
	if added != 0 {
		t.Errorf("gofile link was already filed, added=%d", added)
	}
}

func TestRunBepisKeepsOldCopyWhenDownloadFails(t *testing.T) {
	dir := t.TempDir()
	cards := filepath.Join(dir, "bepis")
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "mega.json"), []byte(`{"groups":[{"name":"manual","links":[]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestCards(t, filepath.Join(cards, "cards.json"), map[int]bepisRecord{1: {ID: 1, Bio: testMegaFolder}})

	status := http.StatusForbidden
	srv := newCardsServer(t, "", &status)
	defer srv.Close()

	// The download retries with a pause; the short deadline keeps the test fast.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := downloadBepisCards(ctx, srv.URL, filepath.Join(cards, "cards.json")); err == nil {
		t.Fatal("expected an error")
	}
	records, err := loadBepisCards(filepath.Join(cards, "cards.json"))
	if err != nil || len(records) != 1 || records[1].Bio != testMegaFolder {
		t.Fatalf("the old copy was damaged: %v %v", records, err)
	}
	if _, err := os.Stat(filepath.Join(cards, "cards.json.tmp")); err == nil {
		t.Error("temp file left behind")
	}
}

func TestDownloadBepisCardsRejectsGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>not json</html>")) }))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "cards.json")
	writeTestCards(t, path, map[int]bepisRecord{1: {ID: 1, Bio: "good"}})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := downloadBepisCards(ctx, srv.URL, path); err == nil {
		t.Fatal("garbage must not be accepted")
	}
	if m, err := loadBepisCards(path); err != nil || m[1].Bio != "good" {
		t.Errorf("good copy replaced: %v %v", m, err)
	}
}
