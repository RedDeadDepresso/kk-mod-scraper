package scrapers

// Shared plumbing for sources that are "folders of zipmods on a file host" (Mega, Google
// Drive). A folderProvider knows how to list one folder and how to read a file's manifest
// with a few ranged reads; everything else is common:
//
//   - config/<kind>.json is a JSON array of folder links;
//   - for every folder, indices/<kind>/<folder name>.json and cache/<kind>/<folder name>.json
//     are written (same formats as the BetterRepack scraper, via refreshCache and writeIndex);
//   - two folders with the same name get "<name> (<id>)";
//   - files of folders that were removed from the config (or renamed on the host) are deleted
//     at the end of a run in which every folder could be read;
//   - one broken folder doesn't stop the others; it is counted as a failure.

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// remoteFolder is a listed folder, ready to be probed.
type remoteFolder struct {
	Name    string
	Listing map[string]fileSig                     // canonical file URL -> change signature
	Labels  map[string]string                      // file URL -> file name, for log lines
	Probe   func(url string) (manifestInfo, error) // reads GUID and version of one file
}

type folderProvider interface {
	// Kind is "mega" or "drive": the sub-directory and game_type of the output.
	Kind() string
	// ID validates a link from the config and returns a stable id for the folder.
	ID(raw string) (string, error)
	// Load lists the folder.
	Load(raw string) (*remoteFolder, error)
}

type hostedOptions struct {
	kind        string
	configPath  string
	indicesDir  string // index files go to <indicesDir>/<kind>/
	cacheDir    string // cache files go to <cacheDir>/<kind>/
	full        bool
	retryFailed bool
	maxURLs     int
	workers     int
}

func (o hostedOptions) indexDir() string { return filepath.Join(o.indicesDir, o.kind) }
func (o hostedOptions) cacheSub() string { return filepath.Join(o.cacheDir, o.kind) }

func isZipmodName(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".zipmod") || strings.HasSuffix(l, ".zip")
}

// ---------------------------------------------------------------------------
// Config and file names
// ---------------------------------------------------------------------------

// loadLinkConfig reads the list of folder links (a JSON array of strings).
func loadLinkConfig(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read the config: %w", err)
	}
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s must be a JSON array of folder links: %w", path, err)
	}
	links := make([]string, 0, len(raw))
	for _, l := range raw {
		if l = strings.TrimSpace(l); l != "" {
			links = append(links, l)
		}
	}
	return links, nil
}

// safeFileName turns a folder name into something usable as a file name on any platform
// (keeps letters of any script, digits, spaces and common punctuation).
func safeFileName(name string) string {
	var sb strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r < 0x20 || r == 0x7f, strings.ContainsRune(`/\:*?"<>|`, r):
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	out := strings.Trim(sb.String(), " .")
	if runes := []rune(out); len(runes) > 100 {
		out = strings.TrimRight(string(runes[:100]), " .")
	}
	return out
}

// uniqueFileBase picks the file name (without ".json") for a folder: its own name, or
// "<name> (<id>)" if another folder of this run already took that name, or just the id if
// the folder has no usable name. used maps a lowercase name to the folder id that owns it
// (file systems may be case-insensitive).
func uniqueFileBase(name, id string, used map[string]string) string {
	base := safeFileName(name)
	if base == "" {
		base = id
	}
	if owner, taken := used[strings.ToLower(base)]; taken && owner != id {
		base = fmt.Sprintf("%s (%s)", base, id)
	}
	used[strings.ToLower(base)] = id
	return base
}

// removeStaleJSON deletes the *.json files of dir whose base name isn't in keep.
func removeStaleJSON(dir string, keep map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || keep[strings.TrimSuffix(name, ".json")] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			slog.Warn("Could not remove a stale file", "path", filepath.Join(dir, name), "err", err)
			continue
		}
		slog.Info("Removed a stale file (folder no longer in the config)", "path", filepath.Join(dir, name))
	}
}

// ---------------------------------------------------------------------------
// Build
// ---------------------------------------------------------------------------

// syncFolder indexes one folder and returns the base name of its two files. unreachable is
// how many files couldn't be probed this time (they are retried next run).
func syncFolder(p folderProvider, raw, id string, opt hostedOptions, used map[string]string) (base string, newGuids, unreachable int, err error) {
	kind := p.Kind()
	slog.Info("Listing the folder", "source", kind, "folder", id)
	folder, err := p.Load(raw)
	if err != nil {
		return "", 0, 0, fmt.Errorf("could not list the %s folder %s: %w", kind, id, err)
	}
	base = uniqueFileBase(folder.Name, id, used)
	slog.Info("Folder listed", "source", kind, "name", base, "zipmods", len(folder.Listing))
	if len(folder.Listing) == 0 {
		return base, 0, 0, fmt.Errorf("the %s folder %q has no .zipmod files (wrong link, or an empty folder?)", kind, base)
	}

	cachePath := filepath.Join(opt.cacheSub(), base+".json")
	cache := map[string]cacheEntry{}
	if opt.full {
		slog.Info("Full rescan requested", "name", base)
	} else {
		cache = loadCache(cachePath)
		slog.Info("Cache loaded", "name", base, "files", len(cache))
	}

	entries, unreachable := refreshCache(folder.Listing, cache, nil, refreshOptions{
		full: opt.full, retryFailed: opt.retryFailed, workers: opt.workers, cachePath: cachePath,
		probe:   folder.Probe,
		label:   func(u string) string { return folder.Labels[u] },
		noMoves: true, // every file has its own id; name/date/size matching would only mislead
	})

	// All files share one "category", so buildGameIndex just orders by newest version, then
	// newest upload, and drops identical copies.
	index := buildGameIndex(entries, []string{""}, "", opt.maxURLs)
	out := filepath.Join(opt.indexDir(), base+".json")
	prev := readIndexGuids(out)
	for guid := range index {
		if _, ok := prev[guid]; !ok {
			newGuids++
		}
	}
	wrote, err := writeIndex(out, kind, index)
	if err != nil {
		return base, 0, unreachable, fmt.Errorf("could not write %s: %w", out, err)
	}
	slog.Info("Index written", "source", kind, "name", base, "guids", len(index), "path", out, "changed", wrote)
	if unreachable > 0 {
		slog.Warn("Some files are missing from the index; the next run will try them again",
			"source", kind, "name", base, "unreachable_files", unreachable)
	}
	return base, newGuids, unreachable, nil
}

// buildHostedIndexes indexes every folder of the config. newGuids counts GUIDs that weren't
// in the previous index of their folder; failures counts folders (or config problems) that
// could not be processed at all - the other folders are still done.
func buildHostedIndexes(opt hostedOptions, p folderProvider) (newGuids, failures int) {
	opt.kind = p.Kind()
	links, err := loadLinkConfig(opt.configPath)
	if err != nil {
		slog.Error("Config problem", "source", opt.kind, "err", err)
		return 0, 1
	}
	if len(links) == 0 {
		slog.Warn("No folders in the config; nothing to do", "source", opt.kind, "config", opt.configPath)
		return 0, 0
	}

	seen := map[string]bool{}
	used := map[string]string{}
	keep := map[string]bool{}
	for _, raw := range links {
		id, err := p.ID(raw)
		if err != nil {
			slog.Error("Skipping an invalid link in the config", "source", opt.kind, "err", err)
			failures++
			continue
		}
		if seen[id] {
			slog.Warn("Folder listed twice in the config; using it once", "source", opt.kind, "folder", id)
			continue
		}
		seen[id] = true

		base, n, _, err := syncFolder(p, raw, id, opt, used)
		if base != "" {
			keep[base] = true // keep its files even if this run couldn't finish them
		}
		if err != nil {
			slog.Error("Folder failed", "source", opt.kind, "folder", id, "err", err)
			failures++
			continue
		}
		newGuids += n
	}

	// Only clean up when every folder was read: a folder that failed to list has an unknown
	// name, and its files must not be mistaken for leftovers.
	if failures == 0 {
		removeStaleJSON(opt.indexDir(), keep)
		removeStaleJSON(opt.cacheSub(), keep)
	}
	return newGuids, failures
}

// runHosted parses command-line style flags (nil = defaults) and builds the indexes.
func runHosted(p folderProvider, args []string) (newGuids, failures int) {
	fs := flag.NewFlagSet(p.Kind(), flag.ExitOnError)
	config := fs.String("config", "config/"+p.Kind()+".json", "JSON array of public folder links")
	indicesDir := fs.String("indices-dir", defaultIndicesDir, "index files are written to <dir>/"+p.Kind()+"/<folder name>.json")
	cacheDir := fs.String("cache-dir", defaultCacheDir, "cache files are written to <dir>/"+p.Kind()+"/<folder name>.json")
	full := fs.Bool("full", false, "ignore the caches and probe every file")
	retryFailed := fs.Bool("retry-failed", false, "probe again the files recorded as not indexable")
	maxURLs := fs.Int("max-urls", defaultMaxURLs, "most URLs kept per GUID")
	workers := fs.Int("workers", 4, "parallel downloads (max 8; the hosts rate-limit aggressively)")
	_ = fs.Parse(args)

	return buildHostedIndexes(hostedOptions{
		configPath:  *config,
		indicesDir:  *indicesDir,
		cacheDir:    *cacheDir,
		full:        *full,
		retryFailed: *retryFailed,
		maxURLs:     max(1, *maxURLs),
		workers:     max(1, min(*workers, 8)),
	}, p)
}
