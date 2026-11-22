package scrapers

// Collects Mega, Google Drive, Pixeldrain, Dropbox, MediaFire, Catbox and Gofile links from
// the bios and comments of the cards on db.bepis.moe and files them in the config of the
// folder scrapers (config/mega.json, config/drive.json, ...), next to the forum links.
//
//  1. the cards are NOT fetched from db.bepis.moe here: kk-author-scraper does that and keeps
//     them in cache/bepis/cards.json of its repository (a JSON array of
//     {id, bio, authors, comments, failed}, one card per line). That file is downloaded at the
//     start of every run to cache/bepis/cards.json here (not committed, see .gitignore). If the
//     download fails, the copy of an earlier run is used when there is one;
//  2. per config, a group with "source": "db.bepis.moe" holds the links of these cards. It is
//     created when missing. Like the forum groups it is maintained automatically: links
//     that lead to no mods are purged by the indexer (hosted.go);
//  3. "newest_card" in that group is how far the cards were read. Only cards above it are
//     read, so a link that was purged is not added and probed again every run. -full reads
//     every card again (also needed for a card the author scraper had refused and fetched
//     again later, which is older than the mark).
//
// Links are found with the same finders as the forum scraper (forum.go), so they come out
// canonical and a folder that is already in the config, in any group, is not added twice.
// Caveat inherited from the author scraper: a card is fetched once, so comments posted on it
// later are not seen.

import (
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
	"strings"
	"syscall"
	"time"
)

// bepisCardsURL is where kk-author-scraper publishes the cards. A variable so that tests can
// point it at a fake server.
var bepisCardsURL = "https://github.com/RedDeadDepresso/kk-author-scraper/raw/refs/heads/main/cache/bepis/cards.json"

const (
	bepisSource     = "db.bepis.moe" // name and "source" of the config group
	bepisUserAgent  = "kk-mod-scraper-bepis/1.0"
	defaultBepisDir = "cache/bepis"
)

// bepisRecord is one card of cards.json. The file also has "authors" (used by
// kk-author-scraper), which is not needed here.
type bepisRecord struct {
	ID       int      `json:"id"`
	Bio      string   `json:"bio"`
	Comments []string `json:"comments"`
	Failed   bool     `json:"failed"`
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

// downloadBepisCards downloads cards.json to path (atomically, only replacing the old copy
// when the new one is a valid JSON array of cards).
func downloadBepisCards(ctx context.Context, url, path string) error {
	const attempts = 3
	var lastErr error
	for i := 1; i <= attempts; i++ {
		if i > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i) * 2 * time.Second):
			}
		}
		lastErr = downloadOnce(ctx, url, path)
		if lastErr == nil || ctx.Err() != nil {
			break
		}
		slog.Warn("Downloading the Bepis cards failed", "attempt", i, "of", attempts, "err", lastErr)
	}
	return lastErr
}

func downloadOnce(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", bepisUserAgent)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d from %s", resp.StatusCode, url)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		_, err = loadBepisCards(tmp) // a truncated or wrong file must not replace a good copy
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not download the cards: %w", err)
	}
	return os.Rename(tmp, path)
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
// of one config: every card above the group's newest_card is read (all cards if full).
// Returns how many links were added.
func fileBepisLinks(path string, kind forumKind, records map[int]bepisRecord, full bool) (added int, err error) {
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
		if !r.Failed && id > from {
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

// RunBepis downloads the cards of db.bepis.moe (as collected by kk-author-scraper) and files
// the links of their bios and comments in the configs of the given kinds (names of sources:
// "mega", "drive", ...; nil = all). It returns how many links were added and how many things
// failed (the download, a config that can't be read or written). args are command-line style
// flags (nil = defaults).
func RunBepis(args []string, kinds []string) (added, failures int) {
	fs := flag.NewFlagSet("bepis", flag.ExitOnError)
	cacheDir := fs.String("bepis-dir", defaultBepisDir, "where cards.json, the downloaded cards, is kept")
	cardsURL := fs.String("bepis-url", bepisCardsURL, "where to download cards.json from (kk-author-scraper's cache)")
	configDir := fs.String("config-dir", "config", "folder with the <source>.json configs the links are added to")
	full := fs.Bool("full", false, "read the links of every card again, not only of the new ones")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cachePath := filepath.Join(*cacheDir, "cards.json")
	if err := downloadBepisCards(ctx, *cardsURL, cachePath); err != nil {
		// Still file the links of the cards we have, if an earlier run left a copy.
		slog.Error("Could not download the Bepis cards", "url", *cardsURL, "err", err)
		failures++
	} else {
		slog.Info("Bepis cards downloaded", "path", cachePath)
	}
	records, err := loadBepisCards(cachePath)
	if err != nil {
		slog.Error("Could not read the Bepis cards", "path", cachePath, "err", err)
		return 0, failures + 1
	}
	if len(records) == 0 {
		return 0, failures
	}
	slog.Info("Bepis cards", "count", len(records))

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
		n, err := fileBepisLinks(path, bepisKinds[name], records, *full)
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
