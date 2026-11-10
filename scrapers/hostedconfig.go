package scrapers

// config/mega.json and config/drive.json hold the folder links, divided in groups, one group
// per place the links came from:
//
//	{
//	  "groups": [
//	    { "name": "manual",   "links": ["https://mega.nz/folder/<id>#<key>"] },
//	    { "name": "kknow.cc", "search": "https://kknow.cc/search/<n>/?q=https%3A%2F%2Fmega.nz&o=date",
//	      "newest_post": { "id": "post-93888", "url": "https://kknow.cc/threads/...", "time": "2026-11-06T16:37:43Z" },
//	      "links": ["https://mega.nz/folder/<id>#<key>"] }
//	  ]
//	}
//
// A group with a "search" URL is filled by the forum scraper (forum.go) and tidied up by the
// indexer: links without mods are purged from it. A group without one is hand-maintained and
// never loses links automatically. "newest_post" is where the forum scraper stops next time.
//
// A group with a "source" (db.bepis.moe) is filled by the Bepis scraper (bepis.go) and is
// tidied up the same way. "newest_card" is the newest cached card whose bio and comments were
// read for it.
//
// The old format, a plain JSON array of links, is still read (as one group named "manual").

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// postMarker identifies the newest forum post a group has been scraped up to.
type postMarker struct {
	ID   string `json:"id"`   // "post-93888", or "thread-3027" for a result that is a thread's first post
	URL  string `json:"url"`  // for humans
	Time string `json:"time"` // RFC 3339, UTC
}

type linkGroup struct {
	Name       string      `json:"name"`
	Search     string      `json:"search,omitempty"`
	NewestPost *postMarker `json:"newest_post,omitempty"`
	Source     string      `json:"source,omitempty"`      // filled by another scraper than the forum one: "db.bepis.moe"
	NewestCard int         `json:"newest_card,omitempty"` // Bepis groups: how far the cached cards were read
	Links      []string    `json:"links"`
}

// scraped reports whether the group is maintained automatically (filled by a scraper, and
// purged of links without mods by the indexer).
func (g *linkGroup) scraped() bool { return g.Search != "" || g.Source != "" }

// searched reports whether the group is filled by a forum search.
func (g *linkGroup) searched() bool { return g.Search != "" }

type hostedConfig struct {
	Groups []*linkGroup `json:"groups"`
}

// loadHostedConfig reads the grouped format, or the legacy array of links.
func loadHostedConfig(path string) (*hostedConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read the config: %w", err)
	}
	trimmed := bytes.TrimSpace(data)
	if string(trimmed) == "null" {
		return &hostedConfig{}, nil // what an empty list marshals to
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var legacy []string
		if err := json.Unmarshal(trimmed, &legacy); err != nil {
			return nil, fmt.Errorf("%s must be a JSON array of folder links or an object with \"groups\": %w", path, err)
		}
		cfg := &hostedConfig{Groups: []*linkGroup{{Name: "manual", Links: legacy}}}
		cfg.tidy()
		return cfg, nil
	}
	var raw struct {
		Groups *[]*linkGroup `json:"groups"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.Groups == nil {
		if err == nil {
			err = errors.New(`missing "groups"`)
		}
		return nil, fmt.Errorf("%s must be a JSON array of folder links or an object with \"groups\": %w", path, err)
	}
	cfg := &hostedConfig{Groups: *raw.Groups}
	for i, g := range cfg.Groups {
		if g == nil {
			return nil, fmt.Errorf("%s: group %d is null", path, i+1)
		}
		if strings.TrimSpace(g.Name) == "" {
			g.Name = fmt.Sprintf("group %d", i+1)
		}
	}
	cfg.tidy()
	return cfg, nil
}

// tidy trims the links and drops empty ones.
func (c *hostedConfig) tidy() {
	for _, g := range c.Groups {
		clean := make([]string, 0, len(g.Links))
		for _, l := range g.Links {
			if l = strings.TrimSpace(l); l != "" {
				clean = append(clean, l)
			}
		}
		g.Links = clean
	}
}

// allLinks returns every link of every group, in order.
func (c *hostedConfig) allLinks() []string {
	var out []string
	for _, g := range c.Groups {
		out = append(out, g.Links...)
	}
	return out
}

// dedupe removes links that point at a folder already listed (earlier in the same group, or
// in an earlier group). id maps a link to a stable folder id; links it can't parse are kept
// (the indexer reports them). Returns how many links were removed.
func (c *hostedConfig) dedupe(id func(string) (string, error)) int {
	seen := map[string]bool{}
	removed := 0
	for _, g := range c.Groups {
		kept := g.Links[:0]
		for _, l := range g.Links {
			fid, err := id(l)
			if err == nil {
				if seen[fid] {
					removed++
					continue
				}
				seen[fid] = true
			}
			kept = append(kept, l)
		}
		g.Links = kept
	}
	return removed
}

// remove deletes the given links from a group.
func (g *linkGroup) remove(links map[string]bool) {
	kept := g.Links[:0]
	for _, l := range g.Links {
		if !links[l] {
			kept = append(kept, l)
		}
	}
	g.Links = kept
}

// save writes the config atomically (so an interrupted run can't leave half a file).
func (c *hostedConfig) save(path string) error {
	for _, g := range c.Groups {
		if g.Links == nil {
			g.Links = []string{} // "links": [] rather than null
		}
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// loadLinkConfig returns every link of the config (all groups), trimmed, empty ones dropped.
func loadLinkConfig(path string) ([]string, error) {
	cfg, err := loadHostedConfig(path)
	if err != nil {
		return nil, err
	}
	return cfg.allLinks(), nil
}
