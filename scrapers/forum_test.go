package scrapers

import (
	"fmt"
	"io"
	"net/url"

	"github.com/PuerkitoBio/goquery"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFindFolders(t *testing.T) {
	text := `a https://mega.nz/folder/gvx2iLTD#eNPVAKMuM9WxM5lLjdJQPw. b https://mega.nz/folder/gvx2iLTD#eNPVAKMuM9WxM5lLjdJQPw/folder/SUB12345
	c https://mega.nz/file/ScQXxKZb#USXN4KQ7E-_tI2vZ8RSq3l-vs00TkLNgUUA-wrjhDv8 d https://mega.nz/folder/5VEHASwB#C5H1m
	e https://mega.co.nz/#F!AbCdEfGh!eNPVAKMuM9WxM5lLjdJQPw`
	got := forumMega.linksIn([]string{text})
	want := []string{
		"https://mega.nz/folder/gvx2iLTD#eNPVAKMuM9WxM5lLjdJQPw",
		"https://mega.nz/file/ScQXxKZb#USXN4KQ7E-_tI2vZ8RSq3l-vs00TkLNgUUA-wrjhDv8", // a single file; the cut folder link (key "C5H1m") is dropped
		"https://mega.nz/folder/AbCdEfGh#eNPVAKMuM9WxM5lLjdJQPw",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("mega: got %v want %v", got, want)
	}
	// wrapped in a proxy url
	got = forumMega.linksIn([]string{"https://kknow.cc/proxy.php?link=https%3A%2F%2Fmega.nz%2Ffolder%2Flug3XBDZ%23pRO7SGMg2_Xf6WROBKNEYg&hash=1"})
	if len(got) != 1 || got[0] != "https://mega.nz/folder/lug3XBDZ#pRO7SGMg2_Xf6WROBKNEYg" {
		t.Errorf("proxied: %v", got)
	}
	d := forumDrive.linksIn([]string{
		"https://drive.google.com/drive/folders/1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu?usp=sharing, https://drive.google.com/drive/u/0/folders/1E8TRD5lSUBrbeb_M4BSHPgMPqHYo8dLl",
		"https://drive.google.com/file/d/1L9c4qG7mZZP3N19pZyM52wTqx9oMdp2Z/view",
	})
	if len(d) != 3 || d[0] != "https://drive.google.com/drive/folders/1-3YtRJFeAeNHybNNQhbnLvKX-NRdxmxu" ||
		d[2] != "https://drive.google.com/file/d/1L9c4qG7mZZP3N19pZyM52wTqx9oMdp2Z/view" {
		t.Errorf("drive: %v", d)
	}
}

func TestParseSearchResultsRealPage(t *testing.T) {
	f, err := os.Open("testdata/kknow_search_page1.htm")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc := mustDoc(t, f)
	page := mustURL(t, "https://kknow.cc/search/4583029/?q=x")
	hits := parseSearchResults(doc, page)
	if len(hits) != 40 {
		t.Fatalf("got %d hits, want 40", len(hits))
	}
	if hits[0].Key != "post-93888" || hits[0].Time.Unix() != 1793983063 ||
		hits[0].URL != "https://kknow.cc/threads/illusion-koikatu-card-sharing-request-thread-2-0-restart.6/post-93888" {
		t.Errorf("first hit: %+v", hits[0])
	}
	var threads int
	for _, h := range hits {
		if strings.HasPrefix(h.Key, "thread-") {
			threads++
		}
	}
	if threads != 2 {
		t.Errorf("thread-type results: %d, want 2", threads)
	}
	if next := nextResultsPage(doc, page); next != "https://kknow.cc/search/1/?page=2&q=https%3A%2F%2Fmega.nz&o=date" {
		t.Errorf("next page: %q", next)
	}
}

func TestParsePostsIgnoresQuotes(t *testing.T) {
	doc := mustDoc(t, strings.NewReader(postPage(
		post(1, `Hi <a href="https://mega.nz/folder/aaaaaaaa#bbbbbbbbbbbbbbbbbbbbbb" class="link link--external">https://mega.nz/folder/aaaa...</a><br>and https://drive.google.com/drive/folders/1234567890abcdef`, `<blockquote class="bbCodeBlock bbCodeBlock--expandable bbCodeBlock--quote"><div class="bbCodeBlock-content">https://mega.nz/folder/qqqqqqqq#wwwwwwwwwwwwwwwwwwwwww</div></blockquote>`),
	)))
	ps := parsePosts(doc)
	if len(ps) != 1 || ps[0].Key != "post-1" {
		t.Fatalf("%+v", ps)
	}
	m := forumMega.linksIn(ps[0].Cands)
	if len(m) != 1 || m[0] != "https://mega.nz/folder/aaaaaaaa#bbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("mega links: %v (quoted link must be ignored)", m)
	}
	if d := forumDrive.linksIn(ps[0].Cands); len(d) != 1 {
		t.Errorf("drive links: %v", d)
	}
}

// ---------------------------------------------------------------------------
// Fake XenForo
// ---------------------------------------------------------------------------

type fakeForum struct {
	*httptest.Server
	mu       sync.Mutex
	posts    map[string]string // post id -> bbWrapper html
	postHits map[string]int
	searches int
}

func post(id int, body, quote string) string {
	return fmt.Sprintf(`<article class="message message--post js-post" data-author="x" data-content="post-%d" id="js-post-%d"><div class="message-inner"><div class="message-cell message-cell--main"><div class="message-body"><div class="bbWrapper">%s%s</div></div></div></div></article>`, id, id, quote, body)
}

func postPage(articles ...string) string {
	return `<html><body>` + strings.Join(articles, "\n") + `</body></html>`
}

var postHrefRe = regexp.MustCompile(`post-(\d+)$`)

func newFakeForum(t *testing.T) *fakeForum {
	t.Helper()
	page1, err := os.ReadFile("testdata/kknow_search_page1.htm")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeForum{posts: map[string]string{}, postHits: map[string]int{}}
	f.posts["93534"] = `Gotcha <a href="https://mega.nz/folder/gvx2iLTD#eNPVAKMuM9WxM5lLjdJQPw">link</a>`
	f.posts["93229"] = `https://mega.nz/folder/lug3XBDZ#pRO7SGMg2_Xf6WROBKNEYg`
	f.posts["91192"] = `beside this <a href="https://mega.nz/folder/5VEHASwB#C5H1mAAAAAAAAAAAAAAAAA">https://mega.nz/folder/5VEHASwB#C5H1m...</a>`
	f.posts["93807"] = `here https://mega.nz/file/ScQXxKZb#USXN4KQ7E-_tI2vZ8RSq3l-vs00TkLNgUUA-wrjhDv8`
	f.posts["92165"] = `https://mega.nz/folder/d8MxVI7Y#3A6ttLQVYc4kilelaL9OkQ and https://drive.google.com/drive/folders/1E8TRD5lSUBrbeb_M4BSHPgMPqHYo8dLl`

	mux := http.NewServeMux()
	mux.HandleFunc("/search/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/":
			fmt.Fprint(w, `<html data-csrf="1,tok"><body>form</body></html>`)
		case r.URL.Path == "/search/search" && r.Method == http.MethodPost:
			r.ParseForm()
			if r.Form.Get("_xfToken") != "1,tok" || r.Form.Get("keywords") == "" {
				http.Error(w, "bad form", 400)
				return
			}
			f.mu.Lock()
			f.searches++
			f.mu.Unlock()
			http.Redirect(w, r, "/search/1/?q="+r.Form.Get("keywords")+"&o=date", http.StatusSeeOther)
		case r.URL.Path == "/search/1/" && r.URL.Query().Get("page") == "2":
			fmt.Fprint(w, `<html><body><ol class="block-body"><li class="block-row block-row--separated"><div class="contentRow"><div class="contentRow-main"><h3 class="contentRow-title"><a href="/threads/t.6/post-100">t</a></h3><div class="contentRow-snippet">old</div><time class="u-dt" data-time="1700000000"></time></div></div></li></ol></body></html>`)
		case r.URL.Path == "/search/1/":
			w.Write(page1)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/threads/", func(w http.ResponseWriter, r *http.Request) {
		m := postHrefRe.FindStringSubmatch(r.URL.Path)
		if m == nil { // a thread URL: page 1, starting with the first post
			fmt.Fprint(w, postPage(post(1, "first post", "")))
			return
		}
		f.mu.Lock()
		f.postHits[m[1]]++
		f.mu.Unlock()
		body, ok := f.posts[m[1]]
		if !ok {
			body = "thanks"
		}
		var id int
		fmt.Sscan(m[1], &id)
		fmt.Fprint(w, postPage(post(id, body, "")))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func writeGrouped(t *testing.T, path, json string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForumScrapeAndIncrementalStop(t *testing.T) {
	f := newFakeForum(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mega.json")
	// The manual group already has one of the folders: it must not be added again.
	writeGrouped(t, path, fmt.Sprintf(`{"groups":[
	 {"name":"manual","links":["https://mega.nz/folder/lug3XBDZ#pRO7SGMg2_Xf6WROBKNEYg"]},
	 {"name":"kknow.cc","search":%q,"links":[]}]}`, f.URL+"/search/4583029/?q=https%3A%2F%2Fmega.nz&o=date"))

	s := newForumScraper(0, 10)
	added, failures := s.runForumConfig(path, forumMega)
	if failures != 0 || added != 4 {
		t.Fatalf("added=%d failures=%d, want 4 and 0", added, failures)
	}
	cfg, err := loadHostedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Groups[1].Links
	want := []string{ // oldest post first
		"https://mega.nz/folder/5VEHASwB#C5H1mAAAAAAAAAAAAAAAAA", // full link from the post's href, not the cut snippet
		"https://mega.nz/folder/d8MxVI7Y#3A6ttLQVYc4kilelaL9OkQ",
		"https://mega.nz/folder/gvx2iLTD#eNPVAKMuM9WxM5lLjdJQPw",
		"https://mega.nz/file/ScQXxKZb#USXN4KQ7E-_tI2vZ8RSq3l-vs00TkLNgUUA-wrjhDv8", // post 93807, a single file
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("links:\n got %v\nwant %v", got, want)
	}
	if len(cfg.Groups[0].Links) != 1 {
		t.Errorf("manual group changed: %v", cfg.Groups[0].Links)
	}
	m := cfg.Groups[1].NewestPost
	if m == nil || m.ID != "post-93888" || m.Time != time.Unix(1793983063, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("marker: %+v", m)
	}
	if f.searches != 1 {
		t.Errorf("searches: %d", f.searches)
	}

	// Second run: nothing new, so only the first result is seen and no post is opened again.
	f.mu.Lock()
	f.postHits = map[string]int{}
	f.mu.Unlock()
	s2 := newForumScraper(0, 10)
	added, failures = s2.runForumConfig(path, forumMega)
	if added != 0 || failures != 0 {
		t.Fatalf("second run added=%d failures=%d", added, failures)
	}
	if len(f.postHits) != 0 {
		t.Errorf("posts were opened again: %v", f.postHits)
	}
	cfg2, _ := loadHostedConfig(path)
	if strings.Join(cfg2.Groups[1].Links, "|") != strings.Join(got, "|") {
		t.Errorf("config changed on a no-op run")
	}
}

func TestForumFailureKeepsMarker(t *testing.T) {
	f := newFakeForum(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mega.json")
	writeGrouped(t, path, fmt.Sprintf(`{"groups":[{"name":"kknow.cc","search":%q,"links":[]}]}`, f.URL+"/search/1/?q=https%3A%2F%2Fmega.nz&o=date"))
	f.Close() // the forum goes away
	added, failures := newForumScraper(0, 10).runForumConfig(path, forumMega)
	if added != 0 || failures != 1 {
		t.Fatalf("added=%d failures=%d", added, failures)
	}
	cfg, _ := loadHostedConfig(path)
	if cfg.Groups[0].NewestPost != nil {
		t.Errorf("marker set after a failed run")
	}
}

// ---------------------------------------------------------------------------
// Config and purge
// ---------------------------------------------------------------------------

func TestHostedConfigFormats(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.json")
	writeGrouped(t, legacy, `["https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg"," "]`)
	cfg, err := loadHostedConfig(legacy)
	if err != nil || len(cfg.Groups) != 1 || cfg.Groups[0].Name != "manual" || len(cfg.Groups[0].Links) != 1 {
		t.Fatalf("legacy: %+v %v", cfg, err)
	}
	g := filepath.Join(dir, "g.json")
	writeGrouped(t, g, `{"groups":[{"name":"a","links":["https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg"]},{"name":"b","links":["https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg/file/X","https://mega.nz/folder/AAAAAAAA#Gu2wMS8oer6bISpDzVdBWg"]}]}`)
	cfg, err = loadHostedConfig(g)
	if err != nil {
		t.Fatal(err)
	}
	if n := cfg.dedupe(megaProvider{}.ID); n != 1 || len(cfg.Groups[0].Links) != 1 || len(cfg.Groups[1].Links) != 1 {
		t.Errorf("dedupe removed %d: %+v %+v", n, cfg.Groups[0], cfg.Groups[1])
	}
	if err := cfg.save(g); err != nil {
		t.Fatal(err)
	}
	if cfg2, err := loadHostedConfig(g); err != nil || len(cfg2.allLinks()) != 2 {
		t.Errorf("round trip: %v %v", cfg2, err)
	}
}

func TestPurgeOnlyScrapedGroups(t *testing.T) {
	// A Mega folder that exists but holds no zipmod (junk only), once in a scraped group
	// and once in a hand-maintained one.
	linkStr := "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg"
	link, _ := parseMegaFolderLink(linkStr)
	junk := newFakeFile(t, "DDDDDDDD", "card.png", 1700000200, []byte("not a mod"))
	newFakeMega(t, link, junk)

	for _, scraped := range []bool{true, false} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config", "mega.json")
		search := ""
		if scraped {
			search = `"search":"https://kknow.cc/search/1/?q=x",`
		}
		writeGrouped(t, path, `{"groups":[{"name":"g",`+search+`"links":["`+linkStr+`"]}]}`)
		_, failures := buildMegaIndexes(hostedOptions{configPath: path, dataDir: filepath.Join(dir, "i"), cacheDir: filepath.Join(dir, "c"), maxURLs: 4, workers: 2})
		cfg, _ := loadHostedConfig(path)
		if scraped && (failures != 0 || len(cfg.Groups[0].Links) != 0) {
			t.Errorf("scraped group: failures=%d links=%v (link should have been purged silently)", failures, cfg.Groups[0].Links)
		}
		if !scraped && (failures != 1 || len(cfg.Groups[0].Links) != 1) {
			t.Errorf("manual group: failures=%d links=%v (must stay and fail as before)", failures, cfg.Groups[0].Links)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func mustDoc(t *testing.T, r io.Reader) *goquery.Document {
	t.Helper()
	d, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
