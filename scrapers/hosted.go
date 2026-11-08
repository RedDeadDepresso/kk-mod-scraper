package scrapers

// Shared plumbing for sources that are "folders of zipmods on a file host" (Mega, Google
// Drive). A folderProvider knows how to list one folder and how to read a file's manifest
// with a few ranged reads; everything else is common:
//
//   - config/<kind>.json holds the folder links in groups (see hostedconfig.go); links of
//     groups filled by the forum scraper that turn out to hold no mods are purged from it,
//     and duplicates are removed;
//   - for every folder, data/<kind>/<folder name>.json and cache/<kind>/<folder name>.json
//     are written (same formats as the BetterRepack scraper, via refreshCache and writeIndex);
//   - two folders with the same name get "<name> (<id>)";
//   - files of folders that were removed from the config (or renamed on the host) are deleted
//     at the end of a run in which every folder could be read;
//   - one broken folder doesn't stop the others; it is counted as a failure.

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
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
	// Kind is "mega" or "drive": the sub-directory of the output.
	Kind() string
	// ID validates a link from the config and returns a stable id for the folder.
	ID(raw string) (string, error)
	// Load lists the folder.
	Load(raw string) (*remoteFolder, error)
}

// fileProvider is implemented by providers that also accept links to single files. All the
// file links of a config are indexed together, as one pseudo folder (singleFilesName).
type fileProvider interface {
	// FileID validates a single-file link and returns a stable id for the file.
	FileID(raw string) (string, error)
	// FileURL is the canonical URL under which the file appears in the index.
	FileURL(raw string) (string, error)
	// LoadFile looks at one file: the result's Listing is empty if it is not a mod archive.
	LoadFile(raw string) (*remoteFolder, error)
}

const singleFilesName = "(single files)"

// linkRef classifies a config link as a folder or a single file.
type linkRef struct {
	id     string // folder id, or "file:<id>"
	isFile bool
}

func classifyLink(p folderProvider, raw string) (linkRef, error) {
	id, ferr := p.ID(raw)
	if ferr == nil {
		return linkRef{id: id}, nil
	}
	if fp, ok := p.(fileProvider); ok {
		if fid, err := fp.FileID(raw); err == nil {
			return linkRef{id: "file:" + fid, isFile: true}, nil
		}
	}
	return linkRef{}, ferr
}

// linkIDFunc gives the dedupe id of any link (folder or file) of a provider.
func linkIDFunc(p folderProvider) func(string) (string, error) {
	return func(raw string) (string, error) {
		r, err := classifyLink(p, raw)
		return r.id, err
	}
}

type hostedOptions struct {
	kind        string
	configPath  string
	dataDir     string // index files go to <dataDir>/<kind>/
	cacheDir    string // cache files go to <cacheDir>/<kind>/
	full        bool
	retryFailed bool
	maxURLs     int
	workers     int
}

func (o hostedOptions) indexDir() string { return filepath.Join(o.dataDir, o.kind) }
func (o hostedOptions) cacheSub() string { return filepath.Join(o.cacheDir, o.kind) }

func isZipmodName(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".zipmod") || strings.HasSuffix(l, ".zip")
}

// ---------------------------------------------------------------------------
// Config and file names
// ---------------------------------------------------------------------------

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
// Purging
// ---------------------------------------------------------------------------

// purgeError says that a link doesn't lead to any mods (and never will): the folder is gone,
// or holds nothing that is a mod. It is only acted upon for links of scraped groups.
type purgeError struct{ reason string }

func (e *purgeError) Error() string { return e.reason }

// isGoneError reports whether the host definitively said the folder doesn't exist (any more):
// as opposed to rate limits, outages or a missing API key, which must never purge anything.
func isGoneError(err error) bool {
	var me megaError
	if errors.As(err, &me) {
		return me == -9 || me == -16 || me == -8 // not found, taken down, expired
	}
	var de *driveError
	if errors.As(err, &de) {
		return de.Status == http.StatusNotFound
	}
	return false
}

// ---------------------------------------------------------------------------
// Build
// ---------------------------------------------------------------------------

// syncFolder indexes one folder and returns the base name of its two files. unreachable is
// how many files couldn't be probed this time (they are retried next run). With purgeable
// set (the link comes from a scraped group), a folder that is gone or holds no mods is
// reported as a *purgeError instead of being indexed (as empty) or counted as a failure.
func syncFolder(p folderProvider, raw, id string, opt hostedOptions, used map[string]string, purgeable bool) (base string, newGuids, unreachable int, err error) {
	kind := p.Kind()
	slog.Info("Listing the folder", "source", kind, "folder", id)
	folder, err := p.Load(raw)
	if err != nil {
		if purgeable && isGoneError(err) {
			return "", 0, 0, &purgeError{fmt.Sprintf("the %s folder %s no longer exists (%v)", kind, id, err)}
		}
		return "", 0, 0, fmt.Errorf("could not list the %s folder %s: %w", kind, id, err)
	}
	base = uniqueFileBase(folder.Name, id, used)
	slog.Info("Folder listed", "source", kind, "name", base, "zipmods", len(folder.Listing))
	if len(folder.Listing) == 0 {
		if purgeable {
			return "", 0, 0, &purgeError{fmt.Sprintf("the %s folder %q has no .zipmod or .zip files", kind, base)}
		}
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
	if purgeable && len(index) == 0 && unreachable == 0 {
		// Every file was read and none is a mod (character cards zipped, packs of many mods...).
		return "", 0, 0, &purgeError{fmt.Sprintf("none of the %d files of the %s folder %q is a mod", len(folder.Listing), kind, base)}
	}
	out := filepath.Join(opt.indexDir(), base+".json")
	prev := readIndexGuids(out)
	for guid := range index {
		if _, ok := prev[guid]; !ok {
			newGuids++
		}
	}
	wrote, err := writeIndex(out, index)
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

// fileItem is a single-file link of the config, with the group that holds it.
type fileItem struct {
	raw string
	g   *linkGroup
}

// syncFiles indexes all the single-file links together. Links of scraped groups that turn
// out not to be mods (or are gone) are returned in purge, with the reason. failures counts
// links that could not be looked at (they are retried next run, and keep their cached data).
func syncFiles(p folderProvider, fp fileProvider, items []fileItem, opt hostedOptions, used map[string]string) (base string, newGuids, failures int, purge map[string]string) {
	kind := p.Kind()
	purge = map[string]string{}
	base = uniqueFileBase(singleFilesName, "single-files", used)
	cachePath := filepath.Join(opt.cacheSub(), base+".json")
	cache := map[string]cacheEntry{}
	if !opt.full {
		cache = loadCache(cachePath)
	}

	merged := &remoteFolder{Listing: map[string]fileSig{}, Labels: map[string]string{}}
	probes := map[string]func(string) (manifestInfo, error){}
	rawOf := map[string]string{}
	slog.Info("Looking at single files", "source", kind, "files", len(items))
	for _, it := range items {
		f, err := fp.LoadFile(it.raw)
		var pe *purgeError
		switch {
		case errors.As(err, &pe):
			if it.g.scraped() {
				purge[it.raw] = pe.reason
				continue
			}
			slog.Error("Unusable file link", "source", kind, "group", it.g.Name, "link", it.raw, "err", err)
			failures++
			continue
		case err != nil && it.g.scraped() && isGoneError(err):
			purge[it.raw] = fmt.Sprintf("the %s file no longer exists (%v)", kind, err)
			continue
		case err != nil:
			slog.Error("Could not look at a file link", "source", kind, "group", it.g.Name, "link", it.raw, "err", err)
			failures++
			// Keep what the cache knows, so one bad moment doesn't drop the file from the index.
			if u, uerr := fp.FileURL(it.raw); uerr == nil {
				if e, ok := cache[u]; ok {
					merged.Listing[u] = fileSig{Mtime: e.Mtime, Size: e.Size}
					merged.Labels[u] = u
					rawOf[u] = it.raw
				}
			}
			continue
		}
		if len(f.Listing) == 0 {
			if it.g.scraped() {
				purge[it.raw] = fmt.Sprintf("the %s file %q is not a .zipmod or .zip", kind, f.Name)
				continue
			}
			slog.Error("File link is not a .zipmod or .zip", "source", kind, "group", it.g.Name, "link", it.raw, "name", f.Name)
			failures++
			continue
		}
		for u, sig := range f.Listing {
			merged.Listing[u] = sig
			merged.Labels[u] = f.Labels[u]
			probes[u] = f.Probe
			rawOf[u] = it.raw
		}
	}

	groupOf := map[string]*linkGroup{}
	for _, it := range items {
		groupOf[it.raw] = it.g
	}
	entries, unreachable := refreshCache(merged.Listing, cache, nil, refreshOptions{
		full: opt.full, retryFailed: opt.retryFailed, workers: opt.workers, cachePath: cachePath,
		probe: func(u string) (manifestInfo, error) {
			if pr, ok := probes[u]; ok {
				return pr(u)
			}
			return manifestInfo{}, errors.New("the file could not be looked at this run")
		},
		label:   func(u string) string { return merged.Labels[u] },
		noMoves: true,
	})
	for u, e := range entries {
		// A recorded error is permanent: the file was read and is no mod.
		if raw := rawOf[u]; e.Guid == "" && e.Error != "" && groupOf[raw] != nil && groupOf[raw].scraped() {
			purge[raw] = fmt.Sprintf("the %s file is not a usable mod (%s)", kind, e.Error)
		}
	}
	if unreachable > 0 {
		slog.Warn("Some files are missing from the index; the next run will try them again", "source", kind, "unreachable_files", unreachable)
	}

	if len(merged.Listing) == 0 {
		return "", 0, failures, purge // nothing to index: no file, and an old one gets cleaned up
	}
	index := buildGameIndex(entries, []string{""}, "", opt.maxURLs)
	out := filepath.Join(opt.indexDir(), base+".json")
	prev := readIndexGuids(out)
	for guid := range index {
		if _, ok := prev[guid]; !ok {
			newGuids++
		}
	}
	wrote, err := writeIndex(out, index)
	if err != nil {
		slog.Error("Could not write the index", "path", out, "err", err)
		return base, 0, failures + 1, purge
	}
	slog.Info("Index written", "source", kind, "name", base, "guids", len(index), "path", out, "changed", wrote)
	return base, newGuids, failures, purge
}

// buildHostedIndexes indexes every folder of the config. newGuids counts GUIDs that weren't
// in the previous index of their folder; failures counts folders (or config problems) that
// could not be processed at all - the other folders are still done.
//
// Afterwards the config is tidied: duplicate links are removed, and links of scraped groups
// that turned out to hold no mods are purged.
func buildHostedIndexes(opt hostedOptions, p folderProvider) (newGuids, failures int) {
	opt.kind = p.Kind()
	cfg, err := loadHostedConfig(opt.configPath)
	if err != nil {
		slog.Error("Config problem", "source", opt.kind, "err", err)
		return 0, 1
	}
	changed := false
	if n := cfg.dedupe(linkIDFunc(p)); n > 0 {
		slog.Info("Removed duplicate links from the config", "source", opt.kind, "removed", n)
		changed = true
	}
	save := func() bool {
		if err := cfg.save(opt.configPath); err != nil {
			slog.Error("Could not save the config", "source", opt.kind, "err", err)
			return false
		}
		return true
	}
	if len(cfg.allLinks()) == 0 {
		slog.Warn("No folders in the config; nothing to do", "source", opt.kind, "config", opt.configPath)
		if changed && !save() {
			return 0, 1
		}
		return 0, 0
	}

	fp, _ := p.(fileProvider)
	used := map[string]string{}
	keep := map[string]bool{}
	purged := map[string]bool{} // links to remove from the scraped groups
	var files []fileItem
	for _, g := range cfg.Groups {
		for _, raw := range g.Links {
			ref, err := classifyLink(p, raw)
			if err != nil {
				slog.Error("Skipping an invalid link in the config", "source", opt.kind, "group", g.Name, "err", err)
				failures++
				continue
			}
			if ref.isFile {
				files = append(files, fileItem{raw, g})
				continue
			}

			base, n, _, err := syncFolder(p, raw, ref.id, opt, used, g.scraped())
			if base != "" {
				keep[base] = true // keep its files even if this run couldn't finish them
			}
			var pe *purgeError
			if errors.As(err, &pe) {
				slog.Info("Purging a link without mods", "source", opt.kind, "group", g.Name, "link", raw, "why", pe.reason)
				purged[raw] = true
				continue
			}
			if err != nil {
				slog.Error("Folder failed", "source", opt.kind, "group", g.Name, "folder", ref.id, "err", err)
				failures++
				continue
			}
			newGuids += n
		}
	}

	if len(files) > 0 && fp != nil {
		base, n, f, purge := syncFiles(p, fp, files, opt, used)
		if base != "" {
			keep[base] = true
		}
		newGuids += n
		failures += f
		for raw, why := range purge {
			slog.Info("Purging a link without mods", "source", opt.kind, "link", raw, "why", why)
			purged[raw] = true
		}
	}

	if len(purged) > 0 {
		for _, g := range cfg.Groups {
			if g.scraped() { // links of hand-maintained groups are never purged
				g.remove(purged)
			}
		}
		changed = true
	}
	if changed {
		if !save() {
			failures++
		} else if len(purged) > 0 {
			slog.Info("Config updated", "source", opt.kind, "purged", len(purged))
		}
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
	config := fs.String("config", "config/"+p.Kind()+".json", "config of public folder links, in groups (a plain JSON array also works)")
	dataDir := fs.String("data-dir", defaultDataDir, "index files are written to <dir>/"+p.Kind()+"/<folder name>.json")
	cacheDir := fs.String("cache-dir", defaultCacheDir, "cache files are written to <dir>/"+p.Kind()+"/<folder name>.json")
	full := fs.Bool("full", false, "ignore the caches and probe every file")
	retryFailed := fs.Bool("retry-failed", false, "probe again the files recorded as not indexable")
	maxURLs := fs.Int("max-urls", defaultMaxURLs, "most URLs kept per GUID")
	workers := fs.Int("workers", 4, "parallel downloads (max 8; the hosts rate-limit aggressively)")
	_ = fs.Parse(args)

	return buildHostedIndexes(hostedOptions{
		configPath:  *config,
		dataDir:     *dataDir,
		cacheDir:    *cacheDir,
		full:        *full,
		retryFailed: *retryFailed,
		maxURLs:     max(1, *maxURLs),
		workers:     max(1, min(*workers, 8)),
	}, p)
}
