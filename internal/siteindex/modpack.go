package siteindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ModpackEntry is one version of one mod in the betterrepack modpack index.
type ModpackEntry struct {
	Version      string   `json:"version"`
	Betterrepack []string `json:"betterrepack"`
}

// ModpackIndex is the published modpack.json: guid -> versions, newest first, Unknown last.
type ModpackIndex map[string][]ModpackEntry

// BuildModpack reads <dataDir>/modpack.json ({"generated", "count", "guids": {guid: {version:
// [link, ...]}}}) and converts it to the same shape as mod.json, with normalized versions.
func BuildModpack(dataDir string) (ModpackIndex, error) {
	path := filepath.Join(dataDir, "modpack.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Guids map[string]map[string][]string `json:"guids"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	out := make(ModpackIndex, len(doc.Guids))
	for guid, versions := range doc.Guids {
		raws := make([]string, 0, len(versions))
		for v := range versions {
			raws = append(raws, v)
		}
		sort.Strings(raws) // several spellings of one version end up in one entry
		byVersion := map[string]*ModpackEntry{}
		var list []ModpackEntry
		for _, rv := range raws {
			nv := NormalizeVersion(rv)
			e := byVersion[nv]
			if e == nil {
				list = append(list, ModpackEntry{Version: nv, Betterrepack: []string{}})
				e = &list[len(list)-1]
				byVersion[nv] = e
			}
			for _, l := range versions[rv] {
				if !contains(e.Betterrepack, l) {
					e.Betterrepack = append(e.Betterrepack, l)
				}
			}
		}
		sort.SliceStable(list, func(i, j int) bool { return CompareVersions(list[i].Version, list[j].Version) > 0 })
		out[guid] = list
	}
	return out, nil
}

// Write encodes the index as compact JSON.
func (idx ModpackIndex) Write(path string) error {
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
