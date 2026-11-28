package siteindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Sources are the file hosts of an entry, in output order. The folder sources are the
// directories data/<name>/*.json; telegram is data/telegram.json.
var folderSources = []string{"mega", "drive", "pixeldrain", "dropbox", "mediafire", "catbox", "gofile"}

// Entry is one version of one mod, with the links to it on every file host (empty list = none).
type Entry struct {
	Version    string   `json:"version"`
	Mega       []string `json:"mega"`
	Drive      []string `json:"drive"`
	Pixeldrain []string `json:"pixeldrain"`
	Dropbox    []string `json:"dropbox"`
	Mediafire  []string `json:"mediafire"`
	Catbox     []string `json:"catbox"`
	Gofile     []string `json:"gofile"`
	Telegram   []string `json:"telegram"`
}

// Index is the published mod.json: guid -> versions, newest first, Unknown last.
type Index map[string][]Entry

func (e *Entry) links(source string) *[]string {
	switch source {
	case "mega":
		return &e.Mega
	case "drive":
		return &e.Drive
	case "pixeldrain":
		return &e.Pixeldrain
	case "dropbox":
		return &e.Dropbox
	case "mediafire":
		return &e.Mediafire
	case "catbox":
		return &e.Catbox
	case "gofile":
		return &e.Gofile
	}
	return &e.Telegram
}

// builder collects links as guid -> normalized version -> source -> links.
type builder map[string]map[string]*Entry

func (b builder) add(guid, rawVersion, source string, links ...string) {
	if guid == "" {
		return
	}
	v := NormalizeVersion(rawVersion)
	if b[guid] == nil {
		b[guid] = map[string]*Entry{}
	}
	e := b[guid][v]
	if e == nil {
		e = &Entry{Version: v}
		b[guid][v] = e
	}
	dst := e.links(source)
	for _, l := range links {
		if !contains(*dst, l) {
			*dst = append(*dst, l)
		}
	}
}

func contains(s []string, x string) bool {
	for _, y := range s {
		if y == x {
			return true
		}
	}
	return false
}

func (b builder) index() Index {
	out := make(Index, len(b))
	for guid, versions := range b {
		list := make([]Entry, 0, len(versions))
		for _, e := range versions {
			for _, s := range append([]string{"telegram"}, folderSources...) {
				if p := e.links(s); *p == nil {
					*p = []string{}
				}
			}
			list = append(list, *e)
		}
		sort.Slice(list, func(i, j int) bool { return CompareVersions(list[i].Version, list[j].Version) > 0 })
		out[guid] = list
	}
	return out
}

// Build reads the index files under dataDir and merges them.
//   - data/<source>/*.json hold {"guids": {"<guid>": {"<version>": [link, ...]}}}, read in
//     file-name order (the best link of each folder is first in its list).
//   - data/telegram.json is a newest-first list of {"Guid", "Version", "Link"}.
func Build(dataDir string) (Index, error) {
	b := builder{}

	for _, source := range folderSources {
		files, err := filepath.Glob(filepath.Join(dataDir, source, "*.json"))
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			var doc struct {
				Guids map[string]map[string][]string `json:"guids"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			guids := make([]string, 0, len(doc.Guids))
			for g := range doc.Guids {
				guids = append(guids, g)
			}
			sort.Strings(guids)
			for _, g := range guids {
				versions := make([]string, 0, len(doc.Guids[g]))
				for v := range doc.Guids[g] {
					versions = append(versions, v)
				}
				sort.Strings(versions) // several spellings of one version end up in one entry
				for _, v := range versions {
					b.add(g, v, source, doc.Guids[g][v]...)
				}
			}
		}
	}

	raw, err := os.ReadFile(filepath.Join(dataDir, "telegram.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		var mods []struct{ Guid, Version, Link string }
		if err := json.Unmarshal(raw, &mods); err != nil {
			return nil, fmt.Errorf("telegram.json: %w", err)
		}
		for _, m := range mods {
			if m.Link != "" {
				b.add(m.Guid, m.Version, "telegram", m.Link)
			}
		}
	}
	return b.index(), nil
}

// Write encodes the index as compact JSON.
func (idx Index) Write(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(idx); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
