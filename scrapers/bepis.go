package scrapers

// Collects Mega, Google Drive, Pixeldrain, Dropbox, MediaFire, Catbox and Gofile links from
// the bios and comments of the cards on db.bepis.moe and files them in the config of the
// folder scrapers (config/mega.json, config/drive.json, ...), next to the forum links.
//
//  1. every card that isn't cached yet is fetched (ids go up from the last cached one to the
//     newest card, which the site's "most recent" search reports) and stored in
//     cache/bepis/cards.json, so a card is only ever fetched once. The cache is a JSON array
//     of {id, bio, comments, failed}, one card per line (valid JSON, but a daily commit only
//     adds lines instead of rewriting a 30+ MB line). Cards the site refused are kept with
//     "failed": true and only tried again with -retry-failed;
//  2. per config, a group with "source": "db.bepis.moe" holds the links of these cards. It is
//     created when missing. Like the forum groups it is maintained automatically: links
//     that lead to no mods are purged by the indexer (hosted.go);
//  3. "newest_card" in that group is how far the cached cards were read. Only cards above it
//     (and cards fetched in this run, which covers retried ones) are read, so a link that was
//     purged is not added and probed again every run. -full reads every cached card again.
//
// Links are found with the same finders as the forum scraper (forum.go), so they come out
// canonical and a folder that is already in the config, in any group, is not added twice.
// Caveat inherited from the original tool: a card is fetched once, so comments posted on it
// later are not seen.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// bepisSearchBase is a variable so that tests can point it at a fake server.
var bepisSearchBase = "https://db.bepis.moe/api/frontend"

const (
	bepisCardType   = "KK"
	bepisSource     = "db.bepis.moe" // name and "source" of the config group
	bepisUserAgent  = "kk-mod-scraper-bepis/1.0"
	defaultBepisDir = "cache/bepis"
)

// bepisRecord is one cached card.
type bepisRecord struct {
	ID       int      `json:"id"`
	Bio      string   `json:"bio"`
	Comments []string `json:"comments"`
	Failed   bool     `json:"failed"`
}

// ---------------------------------------------------------------------------
// API response shapes
// ---------------------------------------------------------------------------

type bepisSearchResp struct {
	Type string `json:"type"`
	Data struct {
		Cards []struct {
			ID int `json:"id"`
		} `json:"cards"`
	} `json:"data"`
}

type bepisCardResp struct {
	Type string `json:"type"`
	Data struct {
		Card struct {
			CardData struct {
				Bio string `json:"bio"`
			} `json:"cardData"`
		} `json:"card"`
		Comments []struct {
			Text string `json:"text"`
		} `json:"comments"`
	} `json:"data"`
}

// ---------------------------------------------------------------------------
// HTTP: one rate limiter shared by all workers, retries with backoff
// ---------------------------------------------------------------------------

type bepisLimiter struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func (l *bepisLimiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	slot := l.next
	l.next = slot.Add(l.interval)
	l.mu.Unlock()

	select {
	case <-time.After(time.Until(slot)):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// penalize pushes every worker back (used on 429 / server errors).
func (l *bepisLimiter) penalize(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t := time.Now().Add(d); t.After(l.next) {
		l.next = t
	}
}

type bepisClient struct {
	http *http.Client
	lim  *bepisLimiter
	base string
}

var errBepisPermanent = errors.New("permanent error")

func (c *bepisClient) getJSON(ctx context.Context, url string, out any) error {
	const maxAttempts = 6
	backoff := 2 * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.lim.wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("%w: %v", errBepisPermanent, err)
		}
		req.Header.Set("User-Agent", bepisUserAgent)

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			c.lim.penalize(backoff)
			backoff *= 2
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			wait := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					wait = time.Duration(secs) * time.Second
				} else if t, err := http.ParseTime(ra); err == nil {
					wait = time.Until(t)
				}
			}
			if wait < time.Second {
				wait = time.Second
			}
			slog.Warn("Bepis is pushing back, pausing all workers", "status", resp.StatusCode,
				"wait", wait.Round(time.Second), "attempt", attempt, "of", maxAttempts)
			c.lim.penalize(wait)
			backoff *= 2
			lastErr = fmt.Errorf("http %d", resp.StatusCode)
			continue
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("%w: http %d", errBepisPermanent, resp.StatusCode)
		case readErr != nil:
			lastErr = readErr
			continue
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%w: bad json: %v", errBepisPermanent, err)
		}
		return nil
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, lastErr)
}

// ---------------------------------------------------------------------------
// Card cache
// ---------------------------------------------------------------------------

// loadBepisCards reads the cache (a missing file is an empty cache).
func loadBepisCards(path string) (map[int]bepisRecord, error) {
	m := map[int]bepisRecord{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var list []bepisRecord
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, r := range list {
		m[r.ID] = r
	}
	return m, nil
}

// saveBepisCards writes the cache atomically (temp file + rename), sorted by id, one card per
// line so that git diffs only show the cards that were added or changed.
func saveBepisCards(path string, m map[int]bepisRecord) error {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, id := range ids {
		r := m[id]
		if r.Comments == nil {
			r.Comments = []string{}
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(b)
		if i < len(ids)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("]\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Fetching cards
// ---------------------------------------------------------------------------

func (c *bepisClient) latestID(ctx context.Context) (int, error) {
	var s bepisSearchResp
	url := fmt.Sprintf("%s/search?cardType=%s&orderType=mostRecent", c.base, bepisCardType)
	if err := c.getJSON(ctx, url, &s); err != nil {
		return 0, err
	}
	if s.Type != "success" || len(s.Data.Cards) == 0 {
		return 0, errors.New("the search returned no cards")
	}
	latest := 0
	for _, card := range s.Data.Cards {
		latest = max(latest, card.ID)
	}
	return latest, nil
}

func (c *bepisClient) fetchCard(ctx context.Context, id int) bepisRecord {
	rec := bepisRecord{ID: id, Comments: []string{}}
	var cr bepisCardResp
	url := fmt.Sprintf("%s/cardPage?cardType=%s&id=%d", c.base, bepisCardType, id)
	if err := c.getJSON(ctx, url, &cr); err != nil || cr.Type != "success" {
		if err != nil && ctx.Err() == nil {
			slog.Debug("Bepis card failed", "id", id, "err", err)
		}
		rec.Failed = true
		return rec
	}
	rec.Bio = cr.Data.Card.CardData.Bio
	for _, cm := range cr.Data.Comments {
		rec.Comments = append(rec.Comments, cm.Text)
	}
	return rec
}

type bepisFetchOptions struct {
	retryFailed bool
	maxCards    int // 0 = no limit
	workers     int
	saveEvery   int
}

// fetchNew fetches the cards that aren't cached yet (or, with retryFailed, the failed ones)
// and stores them in records and on disk. It returns the ids of the cards that were fetched
// successfully this run. Interrupting (Ctrl+C) saves what was fetched so far.
func (c *bepisClient) fetchNew(ctx context.Context, cachePath string, records map[int]bepisRecord, opt bepisFetchOptions) (fetched map[int]bool, err error) {
	fetched = map[int]bool{}

	var ids []int
	if opt.retryFailed {
		for id, r := range records {
			if r.Failed {
				ids = append(ids, id)
			}
		}
		sort.Ints(ids)
		slog.Info("Retrying failed Bepis cards", "count", len(ids))
	} else {
		latest, lerr := c.latestID(ctx)
		if lerr != nil {
			return fetched, fmt.Errorf("could not get the newest Bepis card id: %w", lerr)
		}
		start := 1
		for id := range records {
			start = max(start, id+1)
		}
		slog.Info("Bepis cards", "cached_up_to", start-1, "newest", latest)
		for id := start; id <= latest; id++ {
			ids = append(ids, id)
		}
	}
	if opt.maxCards > 0 && len(ids) > opt.maxCards {
		slog.Warn("Fetching only part of the new Bepis cards this run; the rest follows next run",
			"new", len(ids), "max_cards", opt.maxCards)
		ids = ids[:opt.maxCards]
	}
	if len(ids) == 0 {
		slog.Info("No new Bepis cards")
		return fetched, nil
	}
	slog.Info("Fetching Bepis cards", "count", len(ids), "first", ids[0], "last", ids[len(ids)-1])

	jobs := make(chan int)
	results := make(chan bepisRecord)
	var wg sync.WaitGroup
	for i := 0; i < opt.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				rec := c.fetchCard(ctx, id)
				if ctx.Err() != nil {
					return // interrupted: don't record a bogus failure
				}
				results <- rec
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	done, failed, sinceSave := 0, 0, 0
	for rec := range results {
		records[rec.ID] = rec
		done++
		sinceSave++
		if rec.Failed {
			failed++
		} else {
			fetched[rec.ID] = true
		}
		if sinceSave >= opt.saveEvery {
			if serr := saveBepisCards(cachePath, records); serr != nil {
				slog.Error("Could not save the Bepis card cache", "err", serr)
			}
			sinceSave = 0
			slog.Info("Bepis progress", "done", done, "of", len(ids), "failed", failed, "last_id", rec.ID)
		}
	}
	if serr := saveBepisCards(cachePath, records); serr != nil {
		return fetched, fmt.Errorf("could not save the Bepis card cache: %w", serr)
	}
	slog.Info("Bepis cards fetched", "fetched", done, "failed", failed, "cached", len(records))
	return fetched, nil
}

// ---------------------------------------------------------------------------
// Filing the links
// ---------------------------------------------------------------------------

// bepisKinds maps the name of a source to how its links are found in a text.
var bepisKinds = map[string]forumKind{
	megaKind:       forumMega,
	driveKind:      forumDrive,
	pixeldrainKind: forumPixeldrain,
	dropboxKind:    forumDropbox,
	mediafireKind:  forumMediaFire,
	catboxKind:     forumCatbox,
	gofileKind:     forumGofile,
}

// bepisGroup returns the group of the config that holds the Bepis links, adding it if needed.
func (c *hostedConfig) bepisGroup() (g *linkGroup, created bool) {
	for _, g := range c.Groups {
		if g.Source == bepisSource {
			return g, false
		}
	}
	g = &linkGroup{Name: bepisSource, Source: bepisSource, Links: []string{}}
	c.Groups = append(c.Groups, g)
	return g, true
}

// fileBepisLinks adds the links found in the bios and comments of the cards to the Bepis group
// of one config. fresh are the cards fetched this run; every card above the group's
// newest_card is read as well (all cards if full). Returns how many links were added.
func fileBepisLinks(path string, kind forumKind, records map[int]bepisRecord, fresh map[int]bool, full bool) (added int, err error) {
	cfg, err := loadHostedConfig(path)
	if err != nil {
		return 0, err
	}
	g, created := cfg.bepisGroup()

	known := map[string]bool{}
	for _, l := range cfg.allLinks() {
		if id, err := kind.id(l); err == nil {
			known[id] = true
		}
	}

	from := g.NewestCard
	if full {
		from = 0
	}
	ids := make([]int, 0, len(records))
	newest := g.NewestCard
	for id, r := range records {
		newest = max(newest, id)
		if !r.Failed && (id > from || fresh[id]) {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids) // the config grows in card order

	for _, id := range ids {
		r := records[id]
		if r.Bio == "" && len(r.Comments) == 0 {
			continue
		}
		cands := make([]string, 0, 1+len(r.Comments))
		cands = append(cands, r.Bio)
		cands = append(cands, r.Comments...)
		for _, link := range kind.linksIn(cands) {
			lid, err := kind.id(link)
			if err != nil || known[lid] {
				continue
			}
			known[lid] = true
			g.Links = append(g.Links, link)
			added++
			slog.Debug("New link", "source", kind.name, "group", g.Name, "link", link, "card", id)
		}
	}

	if !created && added == 0 && newest == g.NewestCard {
		return 0, nil // nothing changed; leave the file alone
	}
	g.NewestCard = newest
	if err := cfg.save(path); err != nil {
		return 0, fmt.Errorf("could not save the config: %w", err)
	}
	return added, nil
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

// RunBepis fetches the new cards of db.bepis.moe and files the links of their bios and
// comments in the configs of the given kinds (names of sources: "mega", "drive", ...; nil = all).
// It returns how many links were added and how many things failed (the site being
// unreachable, a config that can't be read or written). A single card that the site refuses is
// not a failure: it is recorded and skipped. args are command-line style flags (nil = defaults).
func RunBepis(args []string, kinds []string) (added, failures int) {
	fs := flag.NewFlagSet("bepis", flag.ExitOnError)
	cacheDir := fs.String("bepis-dir", defaultBepisDir, "where cards.json, the cache of fetched cards, is kept")
	configDir := fs.String("config-dir", "config", "folder with the <source>.json configs the links are added to")
	retryFailed := fs.Bool("retry-failed", false, "fetch again the cards the site refused earlier")
	full := fs.Bool("full", false, "read the links of every cached card again, not only of the new ones")
	rps := fs.Float64("rps", 5, "most requests per second to Bepis (shared by all workers)")
	workers := fs.Int("workers", 4, "parallel requests")
	saveEvery := fs.Int("save-every", 500, "write the card cache to disk every N cards")
	maxCards := fs.Int("max-cards", 30000, "most cards fetched per run (0 = no limit); the rest follows next run")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cachePath := filepath.Join(*cacheDir, "cards.json")
	records, err := loadBepisCards(cachePath)
	if err != nil {
		slog.Error("Could not read the Bepis card cache", "path", cachePath, "err", err)
		return 0, 1
	}

	c := &bepisClient{
		http: &http.Client{Timeout: 30 * time.Second},
		lim:  &bepisLimiter{interval: time.Duration(float64(time.Second) / max(*rps, 0.1))},
		base: bepisSearchBase,
	}
	fresh, ferr := c.fetchNew(ctx, cachePath, records, bepisFetchOptions{
		retryFailed: *retryFailed,
		maxCards:    *maxCards,
		workers:     max(1, *workers),
		saveEvery:   max(1, *saveEvery),
	})
	if ferr != nil {
		// Still file the links of the cards we have: they may not have been read yet.
		slog.Error("Bepis could not be fetched completely", "err", ferr)
		failures++
	}
	if len(records) == 0 {
		return 0, failures
	}

	want := map[string]bool{}
	if len(kinds) == 0 {
		for name := range bepisKinds {
			want[name] = true
		}
	}
	for _, k := range kinds {
		want[strings.TrimSpace(k)] = true
	}
	names := make([]string, 0, len(bepisKinds))
	for name := range bepisKinds {
		if want[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(*configDir, name+".json")
		n, err := fileBepisLinks(path, bepisKinds[name], records, fresh, *full)
		if err != nil {
			slog.Error("Could not file the Bepis links", "source", name, "config", path, "err", err)
			failures++
			continue
		}
		slog.Info("Bepis links filed", "source", name, "added", n)
		added += n
	}
	return added, failures
}
