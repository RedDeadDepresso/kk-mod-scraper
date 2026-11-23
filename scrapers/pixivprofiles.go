package scrapers

// Collects Mega, Google Drive, Pixeldrain, Dropbox, MediaFire, Catbox and Gofile links from
// the pixiv profiles (description, homepage, social links) of the authors and files them in the
// config of the folder scrapers (config/mega.json, ...), like the Bepis scraper (bepis.go).
//
//  1. the profiles are NOT read from pixiv here: kk-author-scraper does that and keeps them in
//     cache/pixiv/profiles.json of its repository (an object {"<pixiv user id>": {available,
//     name, comment, links: [{url, source}], checked_at, ...}}). That file is downloaded at the
//     start of every run to cache/pixiv/profiles.json here (not committed, see .gitignore). If
//     the download fails, the copy of an earlier run is used when there is one;
//  2. per config, a group with "source": "pixiv" holds the links of these profiles. It is
//     created when missing and maintained automatically: links that lead to no mods are purged
//     by the indexer (hosted.go);
//  3. "newest_profile" in that group is the newest "checked_at" that was read. Only profiles
//     checked after it are read, so a link that was purged is not added and probed again every
//     run. A profile that kk-author-scraper reads again (it does so every 30 days by default)
//     is read again here. -full reads every profile again.
//
// Links are found with the same finders as the forum scraper (forum.go) in the description and
// in every link of the profile, so they come out canonical and a folder that is already in the
// config, in any group, is not added twice.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// pixivProfilesURL is where kk-author-scraper publishes the profiles. A variable so that tests
// can point it at a fake server.
var pixivProfilesURL = "https://raw.githubusercontent.com/RedDeadDepresso/kk-author-scraper/refs/heads/main/cache/pixiv/profiles.json"

const (
	pixivSource     = "pixiv" // name and "source" of the config group
	defaultPixivDir = "cache/pixiv"
)

// pixivProfile is one profile of profiles.json (only what is needed here).
type pixivProfile struct {
	Comment   string `json:"comment"`
	CheckedAt string `json:"checked_at"`
	Links     []struct {
		URL string `json:"url"`
	} `json:"links"`
}

// loadPixivProfiles reads the profiles (a missing file is an empty set).
func loadPixivProfiles(path string) (map[string]pixivProfile, error) {
	m := map[string]pixivProfile{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// filePixivLinks adds the links found in the profiles to the pixiv group of one config: every
// profile checked after the group's newest_profile is read (all profiles if full). Returns how
// many links were added.
func filePixivLinks(path string, kind forumKind, profiles map[string]pixivProfile, full bool) (added int, err error) {
	cfg, err := loadHostedConfig(path)
	if err != nil {
		return 0, err
	}
	g, created := cfg.sourceGroup(pixivSource)

	known := map[string]bool{}
	for _, l := range cfg.allLinks() {
		if id, err := kind.id(l); err == nil {
			known[id] = true
		}
	}

	from := g.NewestProfile
	if full {
		from = ""
	}
	newest := g.NewestProfile
	ids := make([]string, 0, len(profiles))
	for id, p := range profiles {
		newest = max(newest, p.CheckedAt)
		if full || p.CheckedAt > from {
			ids = append(ids, id)
		}
	}
	// the config grows in the order the profiles were read
	sort.Slice(ids, func(i, j int) bool {
		a, b := profiles[ids[i]], profiles[ids[j]]
		if a.CheckedAt != b.CheckedAt {
			return a.CheckedAt < b.CheckedAt
		}
		return ids[i] < ids[j]
	})

	for _, id := range ids {
		p := profiles[id]
		cands := make([]string, 0, 1+len(p.Links))
		if p.Comment != "" {
			cands = append(cands, p.Comment)
		}
		for _, l := range p.Links {
			cands = append(cands, l.URL)
		}
		for _, link := range kind.linksIn(cands) {
			lid, err := kind.id(link)
			if err != nil || known[lid] {
				continue
			}
			known[lid] = true
			g.Links = append(g.Links, link)
			added++
			slog.Debug("New link", "source", kind.name, "group", g.Name, "link", link, "pixiv_user", id)
		}
	}

	if !created && added == 0 && newest == g.NewestProfile {
		return 0, nil // nothing changed; leave the file alone
	}
	g.NewestProfile = newest
	if err := cfg.save(path); err != nil {
		return 0, fmt.Errorf("could not save the config: %w", err)
	}
	return added, nil
}

// RunPixiv downloads the pixiv profiles collected by kk-author-scraper and files the links in
// them in the configs of the given kinds (names of sources: "mega", "drive", ...; nil = all).
// It returns how many links were added and how many things failed (the download, a config that
// can't be read or written). args are command-line style flags (nil = defaults).
func RunPixiv(args []string, kinds []string) (added, failures int) {
	fs := flag.NewFlagSet("pixiv", flag.ExitOnError)
	cacheDir := fs.String("pixiv-dir", defaultPixivDir, "where profiles.json, the downloaded profiles, is kept")
	profilesURL := fs.String("pixiv-url", pixivProfilesURL, "where to download profiles.json from (kk-author-scraper's cache)")
	configDir := fs.String("config-dir", "config", "folder with the <source>.json configs the links are added to")
	full := fs.Bool("full", false, "read the links of every profile again, not only of the new ones")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cachePath := filepath.Join(*cacheDir, "profiles.json")
	err := downloadFile(ctx, *profilesURL, cachePath, func(p string) error {
		_, err := loadPixivProfiles(p)
		return err
	})
	if err != nil {
		// Still file the links of the profiles we have, if an earlier run left a copy.
		slog.Error("Could not download the pixiv profiles", "url", *profilesURL, "err", err)
		failures++
	} else {
		slog.Info("Pixiv profiles downloaded", "path", cachePath)
	}
	profiles, err := loadPixivProfiles(cachePath)
	if err != nil {
		slog.Error("Could not read the pixiv profiles", "path", cachePath, "err", err)
		return 0, failures + 1
	}
	if len(profiles) == 0 {
		return 0, failures
	}
	slog.Info("Pixiv profiles", "count", len(profiles))

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
		n, err := filePixivLinks(path, bepisKinds[name], profiles, *full)
		if err != nil {
			slog.Error("Could not file the pixiv links", "source", name, "config", path, "err", err)
			failures++
			continue
		}
		slog.Info("Pixiv links filed", "source", name, "added", n)
		added += n
	}
	return added, failures
}
