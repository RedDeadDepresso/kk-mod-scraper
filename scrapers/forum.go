package scrapers

// Collects Mega and Google Drive folder links from the posts of a XenForo forum (kknow.cc)
// and files them in the config of the folder scrapers (config/mega.json, config/drive.json).
//
// Per config, for every group that has a "search" URL (see hostedconfig.go):
//
//  1. a fresh forum search is run with the query and ordering of that URL. The URL itself
//     isn't reused: "/search/<id>/" pages belong to the session that made them and expire;
//  2. the result pages are walked newest first, until the post the previous run started from
//     ("newest_post" in the group) is reached;
//  3. every new post is opened, because search results only hold a truncated snippet (a link
//     can be cut off, or not shown at all). Links inside quotes are ignored: they belong to
//     somebody else's post;
//  4. folder links found there are added to the group, unless the folder is already listed
//     in any group of the file (duplicates), and "newest_post" is moved to the newest result.
//
// Nothing here checks that a folder holds mods. That is done when the folder is indexed
// (hosted.go), which purges the links of these groups that don't lead to any.
//
// Folder links (https://mega.nz/folder/<id>#<key>, https://drive.google.com/drive/folders/<id>)
// and single-file links (https://mega.nz/file/<id>#<key>, https://drive.google.com/file/d/<id>)
// are both collected; whether a file link is a mod is checked at indexing time.
//
// If the forum wants a login for searching, put the Cookie header value of a logged in
// browser session in the KKNOW_COOKIE environment variable (it is only sent to kknow.cc).

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	forumUserAgent    = "kk-mod-scraper-forum/1.0"
	forumCookieEnv    = "KKNOW_COOKIE"
	forumCookieDomain = "kknow.cc"
	forumMaxBody      = 16 << 20
)

// ---------------------------------------------------------------------------
// Link extraction
// ---------------------------------------------------------------------------

// forumKind says how to find the links of one host in a post.
type forumKind struct {
	name string
	// find returns the canonical folder and file links in text, in order of appearance.
	find func(text string) []string
	// id gives a stable folder id of a link (used to detect duplicates).
	id func(link string) (string, error)
}

var (
	// Folder: handle + 22 character key. File: handle + 43 character key.
	megaFindRe      = regexp.MustCompile(`(?i)https?://(?:www\.)?mega\.(?:nz|io|co\.nz)/(folder/|file/|#F!|#!)([A-Za-z0-9_-]{8})[#!]([A-Za-z0-9_-]{22,43})`)
	driveFindRe     = regexp.MustCompile(`(?i)https?://drive\.google\.com/(?:drive/(?:mobile/|u/\d+/)?folders/|folderview\?id=)([A-Za-z0-9_-]{10,})`)
	driveFileFindRe = regexp.MustCompile(`(?i)https?://drive\.google\.com/(?:file/(?:u/\d+/)?d/|uc\?(?:[^\s#"'<>]*&)?id=)([A-Za-z0-9_-]{10,})`)
)

// findMegaLinks returns the Mega folder and file links of a text, canonical, in order.
// The key's length tells them apart (22 for a folder, 43 for a file); anything else is a cut
// or mangled link and is dropped.
func findMegaLinks(text string) []string {
	var out []string
	for _, m := range megaFindRe.FindAllStringSubmatch(text, -1) {
		isFolder := m[1] == "folder/" || m[1] == "#F!"
		key := m[3]
		switch {
		case isFolder && len(key) >= 22:
			out = append(out, fmt.Sprintf("https://mega.nz/folder/%s#%s", m[2], key[:22]))
		case !isFolder && len(key) == 43:
			out = append(out, fmt.Sprintf("https://mega.nz/file/%s#%s", m[2], key))
		}
	}
	return out
}

// findDriveLinks returns the Drive folder and file links of a text, canonical, in order of
// appearance (folders first, then files; the order between kinds doesn't matter).
func findDriveLinks(text string) []string {
	var out []string
	for _, m := range driveFindRe.FindAllStringSubmatch(text, -1) {
		out = append(out, "https://drive.google.com/drive/folders/"+m[1])
	}
	for _, m := range driveFileFindRe.FindAllStringSubmatch(text, -1) {
		out = append(out, driveFileURL(m[1]))
	}
	return out
}

var (
	forumMega  = forumKind{name: megaKind, find: findMegaLinks, id: linkIDFunc(megaProvider{})}
	forumDrive = forumKind{name: driveKind, find: findDriveLinks, id: linkIDFunc(driveProvider{})}
)

// linksIn finds the folder links in the candidate strings of a post (hrefs and text). A
// candidate is also tried percent-decoded, which covers links wrapped in a redirect/proxy
// URL. Result: canonical links, no duplicates, in order of appearance.
func (k forumKind) linksIn(cands []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		for _, l := range k.find(s) {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	for _, c := range cands {
		add(c)
		if strings.Contains(c, "%") {
			dec := c
			for i := 0; i < 2; i++ {
				next, err := url.PathUnescape(dec)
				if err != nil || next == dec {
					break
				}
				dec = next
				add(dec)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Forum pages
// ---------------------------------------------------------------------------

// searchHit is one row of a search result page.
type searchHit struct {
	Key     string // "post-93888", or "thread-3027" when the row points at a thread
	URL     string
	Time    time.Time
	Snippet string
}

var (
	hitPostRe   = regexp.MustCompile(`/post-(\d+)|/posts/(\d+)`)
	hitThreadRe = regexp.MustCompile(`/threads/[^/?#]*?\.?(\d+)(?:[/?#]|$)`)
)

func hitKey(href string) string {
	if m := hitPostRe.FindStringSubmatch(href); m != nil {
		return "post-" + m[1] + m[2]
	}
	if m := hitThreadRe.FindStringSubmatch(href); m != nil {
		return "thread-" + m[1]
	}
	return ""
}

// parseSearchResults reads the rows of a XenForo 2 search result page.
func parseSearchResults(doc *goquery.Document, page *url.URL) []searchHit {
	var hits []searchHit
	doc.Find("li.block-row").Each(func(_ int, row *goquery.Selection) {
		a := row.Find(".contentRow-title a[href]").Last()
		href, ok := a.Attr("href")
		if !ok {
			return
		}
		key := hitKey(href)
		if key == "" {
			slog.Warn("Skipping a search result with an unrecognised link", "href", href)
			return
		}
		ref, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			return
		}
		hit := searchHit{Key: key, URL: page.ResolveReference(ref).String(), Snippet: row.Find(".contentRow-snippet").Text()}
		if t := row.Find("time[data-time]").First(); t.Length() > 0 {
			if secs, err := strconv.ParseInt(t.AttrOr("data-time", ""), 10, 64); err == nil {
				hit.Time = time.Unix(secs, 0).UTC()
			}
		}
		hits = append(hits, hit)
	})
	return hits
}

func nextResultsPage(doc *goquery.Document, page *url.URL) string {
	href, ok := doc.Find(`link[rel="next"]`).First().Attr("href")
	if !ok {
		return ""
	}
	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return ""
	}
	return page.ResolveReference(ref).String()
}

// forumPost is one post of a thread page: the strings in which to look for links.
type forumPost struct {
	Key   string // "post-93807"
	Cands []string
}

// parsePosts reads the posts of a XenForo 2 thread page, in page order.
func parsePosts(doc *goquery.Document) []forumPost {
	var posts []forumPost
	doc.Find(`article[data-content^="post-"]`).Each(func(_ int, art *goquery.Selection) {
		key := art.AttrOr("data-content", "")
		body := art.Find(".message-body .bbWrapper").First()
		if body.Length() == 0 {
			return
		}
		clone := body.Clone()
		clone.Find("blockquote, .bbCodeBlock--quote").Remove() // somebody else's words
		clone.Find("br").ReplaceWithHtml("\n")
		p := forumPost{Key: key}
		clone.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
			p.Cands = append(p.Cands, strings.TrimSpace(a.AttrOr("href", "")))
		})
		p.Cands = append(p.Cands, clone.Text())
		posts = append(posts, p)
	})
	return posts
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

type forumClient struct {
	http   *http.Client
	delay  time.Duration
	cookie string
	last   time.Time
}

func newForumClient(delay time.Duration) *forumClient {
	jar, _ := cookiejar.New(nil)
	return &forumClient{
		http:   &http.Client{Timeout: 45 * time.Second, Jar: jar},
		delay:  delay,
		cookie: strings.TrimSpace(os.Getenv(forumCookieEnv)),
	}
}

func (c *forumClient) cookieFor(u *url.URL) string {
	h := strings.ToLower(u.Hostname())
	if c.cookie != "" && (h == forumCookieDomain || strings.HasSuffix(h, "."+forumCookieDomain)) {
		return c.cookie
	}
	return ""
}

// wait spaces the requests out, so the forum isn't hammered.
func (c *forumClient) wait() {
	if !c.last.IsZero() {
		if d := c.delay - time.Since(c.last); d > 0 {
			time.Sleep(d)
		}
	}
	c.last = time.Now()
}

// do sends a request (GET, or a form POST) with a few retries on rate limits and outages
// and returns the parsed page and its final URL (after redirects).
func (c *forumClient) do(method, rawURL string, form url.Values) (*goquery.Document, *url.URL, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 2 * time.Second)
		}
		c.wait()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequest(method, rawURL, body)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", forumUserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		req.Header.Set("Accept-Language", "en")
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if ck := c.cookieFor(req.URL); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		res, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(res.Body, forumMaxBody))
		res.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d from %s", res.StatusCode, req.URL.Host)
			continue
		}
		if res.StatusCode != http.StatusOK {
			hint := ""
			if res.StatusCode == http.StatusForbidden {
				hint = " (blocked, or a login is needed: see " + forumCookieEnv + ")"
			}
			return nil, nil, fmt.Errorf("HTTP %d for %s%s", res.StatusCode, req.URL.Redacted(), hint)
		}
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(data)))
		if err != nil {
			return nil, nil, err
		}
		return doc, res.Request.URL, nil
	}
	return nil, nil, fmt.Errorf("giving up on %s: %w", rawURL, lastErr)
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// parseSearchURL takes the site, the query and the ordering out of a search URL as the
// forum shows it (".../search/4583029/?q=https%3A%2F%2Fmega.nz&o=date").
func parseSearchURL(raw string) (site *url.URL, query, order string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", "", fmt.Errorf("not a valid search URL: %q", raw)
	}
	query = strings.TrimSpace(u.Query().Get("q"))
	if query == "" {
		return nil, "", "", fmt.Errorf("the search URL has no q= parameter: %q", raw)
	}
	order = u.Query().Get("o")
	if order == "" {
		order = "date"
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, query, order, nil
}

// search runs a new forum search and returns its first result page. This is what the
// forum's own search form does: fetch a CSRF token, POST the keywords, follow the redirect
// to /search/<id>/.
func (c *forumClient) search(site *url.URL, query, order string) (*goquery.Document, *url.URL, error) {
	formDoc, _, err := c.do(http.MethodGet, site.String()+"/search/", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("could not open the search form: %w", err)
	}
	token := strings.TrimSpace(formDoc.Find("html").AttrOr("data-csrf", ""))
	if token == "" {
		token = strings.TrimSpace(formDoc.Find(`input[name="_xfToken"]`).First().AttrOr("value", ""))
	}
	form := url.Values{"keywords": {query}, "order": {order}}
	if token != "" {
		form.Set("_xfToken", token)
	}
	doc, final, err := c.do(http.MethodPost, site.String()+"/search/search", form)
	if err != nil {
		return nil, nil, fmt.Errorf("the search failed: %w", err)
	}
	return doc, final, nil
}

// ---------------------------------------------------------------------------
// Scraping a group
// ---------------------------------------------------------------------------

type forumScraper struct {
	client   *forumClient
	maxPages int
	posts    map[string][]string // post key -> candidate strings, for every post of every page opened
}

func newForumScraper(delay time.Duration, maxPages int) *forumScraper {
	return &forumScraper{client: newForumClient(delay), maxPages: maxPages, posts: map[string][]string{}}
}

// candidates returns the strings of a post in which to look for links.
func (s *forumScraper) candidates(hit searchHit) ([]string, error) {
	if c, ok := s.posts[hit.Key]; ok {
		return c, nil
	}
	doc, _, err := s.client.do(http.MethodGet, hit.URL, nil)
	if err != nil {
		return nil, err
	}
	posts := parsePosts(doc)
	for _, p := range posts { // the page holds more posts; later hits may need them
		s.posts[p.Key] = p.Cands
	}
	if c, ok := s.posts[hit.Key]; ok {
		return c, nil
	}
	if strings.HasPrefix(hit.Key, "thread-") && len(posts) > 0 {
		s.posts[hit.Key] = posts[0].Cands // a thread result stands for its first post
		return posts[0].Cands, nil
	}
	slog.Warn("Post not found on its page; using the search snippet", "post", hit.Key, "url", hit.URL)
	return []string{hit.Snippet}, nil
}

// reachedKnown reports whether hit is the post the previous run started from, or older.
func reachedKnown(m *postMarker, hit searchHit) bool {
	if m == nil {
		return false
	}
	if hit.Key == m.ID {
		return true
	}
	if t, err := time.Parse(time.RFC3339, m.Time); err == nil && !hit.Time.IsZero() && hit.Time.Before(t) {
		return true
	}
	return false
}

// scrapeGroup adds the new folder links of one group's forum search to the group. known is
// the set of folder ids already in the config (it is updated). It returns how many links
// were added. On an error the links found so far are kept, but newest_post doesn't move, so
// the posts are looked at again next run.
func (s *forumScraper) scrapeGroup(g *linkGroup, kind forumKind, known map[string]bool) (added int, err error) {
	site, query, order, err := parseSearchURL(g.Search)
	if err != nil {
		return 0, err
	}
	slog.Info("Searching the forum", "source", kind.name, "group", g.Name, "site", site.Host, "query", query)
	doc, page, err := s.client.search(site, query, order)
	if err != nil {
		return 0, err
	}

	var (
		fresh   []searchHit
		newest  *searchHit
		reached bool
		seen    = map[string]bool{}
	)
	for pageNo := 1; ; pageNo++ {
		hits := parseSearchResults(doc, page)
		if pageNo == 1 && len(hits) == 0 {
			return 0, errors.New("the search returned no results (a login may be needed, or the forum's layout changed)")
		}
		for i := range hits {
			h := hits[i]
			if newest == nil {
				newest = &hits[i]
			}
			if reachedKnown(g.NewestPost, h) {
				reached = true
				break
			}
			if !seen[h.Key] {
				seen[h.Key] = true
				fresh = append(fresh, h)
			}
		}
		if reached {
			break
		}
		next := nextResultsPage(doc, page)
		if next == "" {
			break
		}
		if pageNo >= s.maxPages {
			slog.Warn("Page limit reached; older posts are not looked at", "group", g.Name, "max_pages", s.maxPages)
			break
		}
		if doc, page, err = s.client.do(http.MethodGet, next, nil); err != nil {
			return 0, fmt.Errorf("could not load result page %d: %w", pageNo+1, err)
		}
	}
	slog.Info("Posts to read", "source", kind.name, "group", g.Name, "new_posts", len(fresh), "reached_known_post", reached)

	failed := 0
	for i := len(fresh) - 1; i >= 0; i-- { // oldest first, so the config grows in posting order
		h := fresh[i]
		cands, err := s.candidates(h)
		if err != nil {
			slog.Error("Could not read a post", "post", h.Key, "url", h.URL, "err", err)
			failed++
			continue
		}
		for _, link := range kind.linksIn(cands) {
			id, err := kind.id(link)
			if err != nil || known[id] {
				continue
			}
			known[id] = true
			g.Links = append(g.Links, link)
			added++
			slog.Info("New link", "source", kind.name, "group", g.Name, "link", link, "post", h.URL)
		}
	}

	if failed > 0 {
		return added, fmt.Errorf("%d of %d posts could not be read; they will be tried again next run", failed, len(fresh))
	}
	if newest != nil {
		g.NewestPost = &postMarker{ID: newest.Key, URL: newest.URL, Time: newest.Time.Format(time.RFC3339)}
	}
	return added, nil
}

// runForumConfig scrapes every group with a search URL of one config file and saves the file.
func (s *forumScraper) runForumConfig(path string, kind forumKind) (added, failures int) {
	cfg, err := loadHostedConfig(path)
	if err != nil {
		slog.Error("Config problem", "source", kind.name, "err", err)
		return 0, 1
	}
	scrapedAny := false
	for _, g := range cfg.Groups {
		if g.scraped() {
			scrapedAny = true
		}
	}
	if !scrapedAny {
		slog.Info("No group with a search URL in the config; nothing to scrape", "source", kind.name, "config", path)
		return 0, 0
	}

	known := map[string]bool{}
	for _, l := range cfg.allLinks() {
		if id, err := kind.id(l); err == nil {
			known[id] = true
		}
	}
	for _, g := range cfg.Groups {
		if !g.scraped() {
			continue
		}
		n, err := s.scrapeGroup(g, kind, known)
		added += n
		if err != nil {
			slog.Error("Forum group failed", "source", kind.name, "group", g.Name, "err", err)
			failures++
		}
	}
	if removed := cfg.dedupe(kind.id); removed > 0 {
		slog.Info("Removed duplicate links from the config", "source", kind.name, "removed", removed)
	}
	if err := cfg.save(path); err != nil {
		slog.Error("Could not save the config", "source", kind.name, "err", err)
		failures++
	}
	return added, failures
}

// RunForum collects links from the forum searches configured in config/mega.json and
// config/drive.json. It returns how many links were added and how many groups failed.
// args are command-line style flags (nil = defaults).
func RunForum(args []string) (added, failures int) {
	fs := flag.NewFlagSet("forum", flag.ExitOnError)
	megaCfg := fs.String("mega-config", "config/mega.json", "Mega config (groups with a search URL are scraped)")
	driveCfg := fs.String("drive-config", "config/drive.json", "Google Drive config (groups with a search URL are scraped)")
	maxPages := fs.Int("max-pages", 30, "most result pages read per search")
	delay := fs.Duration("delay", 1500*time.Millisecond, "pause between requests to the forum")
	_ = fs.Parse(args)

	s := newForumScraper(*delay, max(1, *maxPages))
	for _, c := range []struct {
		path string
		kind forumKind
	}{{*megaCfg, forumMega}, {*driveCfg, forumDrive}} {
		n, f := s.runForumConfig(c.path, c.kind)
		added += n
		failures += f
	}
	return added, failures
}
