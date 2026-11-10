package scrapers

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestBepisCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cards.json")
	in := map[int]bepisRecord{
		3: {ID: 3, Bio: "a <b> & \"c\"", Comments: []string{"x", "y\r\nz"}},
		1: {ID: 1, Failed: true},
		2: {ID: 2},
	}
	if err := saveBepisCards(path, in); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 5 { // "[", three cards, "]"
		t.Fatalf("expected one card per line, got %d lines:\n%s", len(lines), raw)
	}
	if !strings.HasPrefix(lines[1], `{"id":1,`) || !strings.HasPrefix(lines[3], `{"id":3,`) {
		t.Errorf("cards not sorted by id:\n%s", raw)
	}
	if !strings.Contains(lines[2], `"comments":[]`) {
		t.Errorf("nil comments must be written as []: %s", lines[2])
	}
	var plain []bepisRecord // still plain JSON, as the standalone tool wrote it
	if err := json.Unmarshal(raw, &plain); err != nil || len(plain) != 3 {
		t.Fatalf("not valid JSON: %v", err)
	}
	out, err := loadBepisCards(path)
	if err != nil {
		t.Fatal(err)
	}
	if out[3].Bio != in[3].Bio || len(out[3].Comments) != 2 || !out[1].Failed {
		t.Errorf("round trip changed the data: %+v", out)
	}
	if m, err := loadBepisCards(filepath.Join(t.TempDir(), "none.json")); err != nil || len(m) != 0 {
		t.Errorf("a missing cache must be empty, got %v %v", m, err)
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

	added, err := fileBepisLinks(path, forumMega, cards, nil, false)
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
	if added, err = fileBepisLinks(path, forumMega, cards, nil, false); err != nil || added != 0 {
		t.Errorf("purged link re-added: added=%d err=%v", added, err)
	}
	// ...unless the card was fetched again this run, or everything is re-read.
	if added, _ = fileBepisLinks(path, forumMega, cards, map[int]bool{2: true}, false); added != 1 {
		t.Errorf("a card fetched this run must be read, added=%d", added)
	}
	cfg, _ = loadHostedConfig(path)
	cfg.Groups[1].Links = nil
	_ = cfg.save(path)
	if added, _ = fileBepisLinks(path, forumMega, cards, nil, true); added != 1 {
		t.Errorf("-full must read every cached card, added=%d", added)
	}

	// New cards above the watermark are read.
	cards[6] = bepisRecord{ID: 6, Bio: "new " + "https://mega.nz/folder/QqQqQqQq#abcdefghijklmnopqrstuv"}
	if added, _ = fileBepisLinks(path, forumMega, cards, nil, false); added != 1 {
		t.Errorf("new card not read, added=%d", added)
	}
}

func TestFileBepisLinksLeavesUnchangedConfigAlone(t *testing.T) {
	path := writeTestConfig(t, `{"groups":[{"name":"x","links":[]}]}`)
	cards := map[int]bepisRecord{1: {ID: 1}}
	if _, err := fileBepisLinks(path, forumGofile, cards, nil, false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	st1, _ := os.Stat(path)
	time.Sleep(20 * time.Millisecond)
	if added, err := fileBepisLinks(path, forumGofile, cards, nil, false); err != nil || added != 0 {
		t.Fatal(added, err)
	}
	after, _ := os.ReadFile(path)
	st2, _ := os.Stat(path)
	if string(before) != string(after) || !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("an unchanged config was rewritten")
	}
}

func TestBepisFetchNew(t *testing.T) {
	const latest = 6
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path+"?"+r.URL.RawQuery)
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			fmt.Fprintf(w, `{"type":"success","data":{"cards":[{"id":%d},{"id":%d}]}}`, latest-1, latest)
		case strings.HasSuffix(r.URL.Path, "/cardPage"):
			id := r.URL.Query().Get("id")
			if id == "5" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"type":"success","data":{"card":{"cardData":{"bio":"bio %s %s"}},"comments":[{"text":"c%s"}]}}`, id, testGofile, id)
		}
	}))
	defer srv.Close()

	c := &bepisClient{http: srv.Client(), lim: &bepisLimiter{interval: time.Millisecond}, base: srv.URL}
	path := filepath.Join(t.TempDir(), "cards.json")
	records := map[int]bepisRecord{3: {ID: 3, Bio: "cached"}} // ids 1 and 2 are "older than the cache"

	fresh, err := c.fetchNew(context.Background(), path, records, bepisFetchOptions{workers: 2, saveEvery: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || records[3].Bio != "cached" {
		t.Fatalf("expected the cached card to be kept and 4..6 fetched, got %d records", len(records))
	}
	if !fresh[4] || fresh[5] || !fresh[6] || !records[5].Failed {
		t.Errorf("fresh=%v failed5=%v", fresh, records[5].Failed)
	}
	if !strings.Contains(records[4].Bio, testGofile) || records[4].Comments[0] != "c4" {
		t.Errorf("card 4 = %+v", records[4])
	}
	for _, h := range hits {
		if strings.Contains(h, "id=1") || strings.Contains(h, "id=3") {
			t.Errorf("fetched a card that is cached or older: %s", h)
		}
	}
	saved, err := loadBepisCards(path)
	if err != nil || len(saved) != 4 {
		t.Fatalf("saved cache: %d records, %v", len(saved), err)
	}

	// Nothing new: no card requests, only the search for the newest id.
	hits = nil
	if fresh, err = c.fetchNew(context.Background(), path, records, bepisFetchOptions{workers: 2, saveEvery: 10}); err != nil || len(fresh) != 0 {
		t.Fatal(fresh, err)
	}
	if len(hits) != 1 {
		t.Errorf("expected only the search request, got %v", hits)
	}

	// -retry-failed: only the failed card, and it still fails (404).
	hits = nil
	if _, err = c.fetchNew(context.Background(), path, records, bepisFetchOptions{retryFailed: true, workers: 1, saveEvery: 10}); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "id=5") {
		t.Errorf("retry should only ask for card 5, got %v", hits)
	}

	// max-cards caps a run.
	records = map[int]bepisRecord{}
	if _, err = c.fetchNew(context.Background(), path, records, bepisFetchOptions{maxCards: 2, workers: 1, saveEvery: 10}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].ID != 1 || records[2].ID != 2 {
		t.Errorf("max-cards: %v", records)
	}
}

func TestRunBepisFilesLinksOfTheCache(t *testing.T) {
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
	err := saveBepisCards(filepath.Join(cards, "cards.json"), map[int]bepisRecord{
		1: {ID: 1, Bio: testMegaFolder + " and " + testGofile},
	})
	if err != nil {
		t.Fatal(err)
	}
	// An unreachable site must not stop the cached cards from being filed: it is one failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) }))
	defer srv.Close()
	old := bepisSearchBase
	bepisSearchBase = srv.URL
	defer func() { bepisSearchBase = old }()

	added, failures := RunBepis([]string{"-bepis-dir", cards, "-config-dir", cfgDir}, []string{"mega", "gofile"})
	if added != 2 || failures != 1 {
		t.Errorf("added=%d failures=%d, want 2 and 1", added, failures)
	}
	// only the enabled kinds are filled
	added, _ = RunBepis([]string{"-bepis-dir", cards, "-config-dir", cfgDir, "-full"}, []string{"gofile"})
	if added != 0 {
		t.Errorf("gofile link was already filed, added=%d", added)
	}
}
