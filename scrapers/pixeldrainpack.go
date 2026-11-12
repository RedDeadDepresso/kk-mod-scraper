package scrapers

// GUID index for the forum's curated "Mod Packs" on Pixeldrain.
//
// The first posts of the forum's "Mod - Part N: Mod Packs ..." threads link to Pixeldrain
// files (https://pixeldrain.com/u/<id>) that are not single mods but big .zip archives holding
// hundreds of .zipmod files (one archive per "GUID-Index-NNN" pack). They can't be indexed
// like the other Pixeldrain files: there is no manifest.xml at the root of such a zip, and
// reading hundreds of manifests through ranged reads would be wasteful anyway. Instead:
//
//   - a /l/ list page embeds its files in window.viewer_data; each file is then handled like a /u/ one;
//   - GET /api/file/<id>/info/zip lists the zip's entries (names and sizes) in one call;
//   - the forum publishes All_Mod_List.txt, one line per mod of every pack:
//     "001: 3, file_name: [TDI]PokeBalls.zipmod, guid: &lt;pokeb.tdi" (HTML-encoded), so the
//     name of an entry is mapped to its GUID through that list;
//   - a single entry can be downloaded at
//     https://pixeldrain.com/api/file/<id>/info/zip/<path inside the zip>, and that is the URL
//     that goes in the index.
//
// A zip contains the "GUID-Index-NNN.txt" file(s) of the pack(s) it holds, which tells which
// pack(s) of the list to look the names up in (the same file name can occur in several packs
// with different GUIDs). A zip without such a file is looked up in the whole list, and a
// name that is ambiguous there is skipped.
//
// Names stored in a legacy code page (GBK...) come back from Pixeldrain mangled, with "\uFFFD"
// for each byte that is not valid UTF-8; see pixeldrainmangle.go for how they are matched.
//
// Input (all optional; if there is no mod list the step does nothing):
//
//	config/forum-packs/All_Mod_List.txt   the mod list
//	config/forum-packs/*.htm              saved first posts of the forum threads (their
//	                                      /u/ links are used), and/or
//	config/forum-packs/links.txt          extra links, one per line (# comments allowed)
//
// Output: data/pixeldrain/Forum Mod Packs.json, merged into mod.json with the other
// Pixeldrain indexes by the Pages workflow. /u/ links (one zip) and /l/ links (a list of zips) are handled.

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	forumPacksIndexName = "Forum Mod Packs"
	forumPacksDir       = "config/forum-packs"
	// Public base of the download URLs in the index (pixeldrainAPIBase may point at a fake in tests).
	pixeldrainPublicAPI = "https://pixeldrain.com/api"
)

// ---------------------------------------------------------------------------
// The mod list
// ---------------------------------------------------------------------------

// modList maps file names to GUIDs: per pack, and over all packs.
type modList struct {
	byPack map[int]map[string]string // pack -> file name -> guid
	all    map[string]map[string]bool
	lower  map[string]map[string]bool // lowercase file name -> guids
	lines  int

	mu      sync.Mutex                         // guards mangled
	mangled map[int]map[string]map[string]bool // pack -> mangled name -> guids (see pixeldrainmangle.go)
}

var modListLineRe = regexp.MustCompile(`^(\d+): (\d+), file_name: (.*), guid: (.*)$`)

// parseModList reads All_Mod_List.txt. The text is HTML-encoded (&amp; &lt; &gt;), one level.
func parseModList(path string) (*modList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ml := &modList{byPack: map[int]map[string]string{}, all: map[string]map[string]bool{}, lower: map[string]map[string]bool{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		m := modListLineRe.FindStringSubmatch(html.UnescapeString(line))
		if m == nil {
			continue
		}
		pack, _ := strconv.Atoi(m[1])
		name, guid := strings.TrimSpace(m[3]), strings.TrimSpace(m[4])
		if name == "" || guid == "" || guid == "<not set>" {
			continue // no GUID to index
		}
		ml.lines++
		if ml.byPack[pack] == nil {
			ml.byPack[pack] = map[string]string{}
		}
		ml.byPack[pack][name] = guid
		for _, k := range []string{name} {
			if ml.all[k] == nil {
				ml.all[k] = map[string]bool{}
			}
			ml.all[k][guid] = true
		}
		lk := strings.ToLower(name)
		if ml.lower[lk] == nil {
			ml.lower[lk] = map[string]bool{}
		}
		ml.lower[lk][guid] = true
	}
	return ml, sc.Err()
}

// lookup finds the GUID of a zip entry's file name: in the given packs first, then (if no
// pack matched) in the whole list, where the name must lead to exactly one GUID.
func (ml *modList) lookup(name string, packs []int) (guid string, ok bool) {
	for _, p := range packs {
		if g, found := ml.byPack[p][name]; found {
			return g, true
		}
	}
	if len(packs) > 0 {
		// The pack is known: tolerate a different letter case only inside those packs.
		for _, p := range packs {
			for n, g := range ml.byPack[p] {
				if strings.EqualFold(n, name) {
					return g, true
				}
			}
		}
	}
	// Names that Pixeldrain returned mangled (legacy code page read as UTF-8, bad bytes -> U+FFFD):
	// recovered through the pack's names, and only when they lead to exactly one GUID.
	if g := ml.lookupMangled(name, packs); g != "" {
		return g, true
	}
	if g := onlyOne(ml.all[name]); g != "" {
		return g, true
	}
	if g := onlyOne(ml.lower[strings.ToLower(name)]); g != "" {
		return g, true
	}
	return "", false
}

func onlyOne(set map[string]bool) string {
	if len(set) != 1 {
		return ""
	}
	for g := range set {
		return g
	}
	return ""
}

// ---------------------------------------------------------------------------
// Zip listing
// ---------------------------------------------------------------------------

// zipEntry is one file inside a zip, with its path relative to the zip's root.
type zipEntry struct {
	Path string
	Size int64
}

type zipChild struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Size     int64           `json:"size"`
	FileSize int64           `json:"file_size"`
	Children json.RawMessage `json:"children"`
}

// parseZipChildren reads the "children" of /api/file/<id>/info/zip: an object name -> node
// ({"size": n}, possibly with nested "children"), or (as in the file-system API) a list of
// nodes with a "name". Directories are walked.
func parseZipChildren(raw json.RawMessage, prefix string, out *[]zipEntry, depth int) error {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" || depth > 12 {
		return nil
	}
	add := func(name string, c zipChild) error {
		full := prefix + name
		hasKids := len(strings.TrimSpace(string(c.Children))) > 2 && string(c.Children) != "null"
		if c.Type == "dir" || hasKids {
			return parseZipChildren(c.Children, full+"/", out, depth+1)
		}
		size := c.Size
		if size == 0 {
			size = c.FileSize
		}
		*out = append(*out, zipEntry{Path: full, Size: size})
		return nil
	}
	switch raw[0] {
	case '{':
		var m map[string]zipChild
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if err := add(n, m[n]); err != nil {
				return err
			}
		}
	case '[':
		var l []zipChild
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		for _, c := range l {
			if err := add(c.Name, c); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected \"children\" in the zip listing: %.40s", raw)
	}
	return nil
}

// zipList returns the entries of the zip behind a /u/ link.
func (c *pixeldrainClient) zipList(id string) ([]zipEntry, error) {
	var doc struct {
		Children json.RawMessage `json:"children"`
	}
	if err := c.getJSON("/file/"+id+"/info/zip", &doc); err != nil {
		return nil, err
	}
	var out []zipEntry
	if err := parseZipChildren(doc.Children, "", &out, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// zipEntryURL is the download URL of one file inside a zip.
func zipEntryURL(id, entryPath string) string {
	segs := strings.Split(entryPath, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return pixeldrainPublicAPI + "/file/" + id + "/info/zip/" + strings.Join(segs, "/")
}

var guidIndexNameRe = regexp.MustCompile(`(?i)^GUID-Index-(\d+)[^/]*\.txt$`)

// packsOf returns the packs a zip holds, from its GUID-Index-NNN.txt files.
func packsOf(entries []zipEntry) []int {
	seen := map[int]bool{}
	var packs []int
	for _, e := range entries {
		if m := guidIndexNameRe.FindStringSubmatch(path.Base(e.Path)); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && !seen[n] {
				seen[n] = true
				packs = append(packs, n)
			}
		}
	}
	sort.Ints(packs)
	return packs
}

// mapZip turns the entries of one zip into guid -> URLs. unmatched lists the mod files whose
// GUID the list doesn't know.
func mapZip(id string, entries []zipEntry, ml *modList) (index map[string][]string, unmatched []string, packs []int) {
	packs = packsOf(entries)
	index = map[string][]string{}
	for _, e := range entries {
		base := path.Base(e.Path)
		if !isZipmodName(base) || e.Size <= 0 {
			continue
		}
		guid, ok := ml.lookup(base, packs)
		if !ok {
			unmatched = append(unmatched, e.Path)
			continue
		}
		index[guid] = append(index[guid], zipEntryURL(id, e.Path))
	}
	return index, unmatched, packs
}

// ---------------------------------------------------------------------------
// Links and lists
// ---------------------------------------------------------------------------

// packLink is a /u/ (one zip) or /l/ (a list of zips) link.
type packLink struct {
	List bool
	ID   string
}

// packFile is one zip to index. Size and DateUpload come from the list page for files of a
// list; for a /u/ link they are read with one /info call.
type packFile struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	DateUpload string `json:"date_upload"`
}

func isPackZip(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".zip") }

// findPackLinks returns the /u/ and /l/ links of an HTML page or text, in order of
// appearance, once each. Directory (/d/) links are not handled here.
func findPackLinks(text string) []packLink {
	var out []packLink
	seen := map[packLink]bool{}
	for _, l := range findPixeldrainLinks(text) {
		var pl packLink
		if id, err := parsePixeldrainFileLink(l); err == nil {
			pl = packLink{ID: id}
		} else if id, err := parsePixeldrainListLink(l); err == nil {
			pl = packLink{List: true, ID: id}
		} else {
			continue
		}
		if !seen[pl] {
			seen[pl] = true
			out = append(out, pl)
		}
	}
	return out
}

func readPackLinks(dir string) ([]packLink, error) {
	var links []packLink
	seen := map[packLink]bool{}
	var files []string
	for _, pat := range []string{"*.htm", "*.html", "links.txt"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		text := string(data)
		if strings.HasSuffix(f, "links.txt") {
			var kept []string
			for _, ln := range strings.Split(text, "\n") {
				if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "#") {
					kept = append(kept, ln)
				}
			}
			text = strings.Join(kept, "\n")
		}
		found := findPackLinks(text)
		slog.Info("Read pack links", "file", filepath.Base(f), "links", len(found))
		for _, l := range found {
			if !seen[l] {
				seen[l] = true
				links = append(links, l)
			}
		}
	}
	return links, nil
}

// parseViewerData reads the files of a list from the list page
// (https://pixeldrain.com/l/<id>): the page embeds `window.viewer_data = {...};` with the
// list in "api_response". A file's id is taken from its "detail_href" ("/file/<id>/info"),
// which is also the API path of the file.
func parseViewerData(page string) (title string, files []packFile, err error) {
	const marker = "window.viewer_data"
	i := strings.Index(page, marker)
	if i < 0 {
		return "", nil, errors.New("no viewer_data in the list page")
	}
	rest := page[i+len(marker):]
	eq := strings.Index(rest, "=")
	if eq < 0 {
		return "", nil, errors.New("malformed viewer_data in the list page")
	}
	var doc struct {
		Type string `json:"type"`
		Resp struct {
			Title string `json:"title"`
			Files []struct {
				ID         string `json:"id"`
				DetailHref string `json:"detail_href"`
				Name       string `json:"name"`
				Size       int64  `json:"size"`
				DateUpload string `json:"date_upload"`
			} `json:"files"`
		} `json:"api_response"`
	}
	if err := json.NewDecoder(strings.NewReader(rest[eq+1:])).Decode(&doc); err != nil {
		return "", nil, fmt.Errorf("could not read viewer_data: %w", err)
	}
	for _, f := range doc.Resp.Files {
		id := f.ID
		if m := detailHrefRe.FindStringSubmatch(f.DetailHref); m != nil {
			id = m[1]
		}
		if id == "" {
			continue
		}
		files = append(files, packFile{ID: id, Name: f.Name, Size: f.Size, DateUpload: f.DateUpload})
	}
	return doc.Resp.Title, files, nil
}

var detailHrefRe = regexp.MustCompile(`^/file/([A-Za-z0-9]+)/info`)

// pixeldrainSiteBase is the website (a variable so tests can point it at a fake).
var pixeldrainSiteBase = "https://pixeldrain.com"

// listFiles returns the files of a /l/ list: from the list page, and if that can't be read,
// from the JSON API.
func (c *pixeldrainClient) listFiles(id string) ([]packFile, error) {
	files, err := pixeldrainRetry(func() ([]packFile, error) {
		res, err := c.do(pixeldrainSiteBase+"/l/"+id, nil)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		if err != nil {
			return nil, err
		}
		_, files, err := parseViewerData(string(body))
		return files, err
	})
	if err == nil {
		return files, nil
	}
	var pe *pixeldrainError
	if errors.As(err, &pe) && pe.permanent() {
		return nil, err // the list is gone
	}
	slog.Warn("List page unusable; trying the API", "source", pixeldrainKind, "list", id, "err", err)
	_, apiFiles, aerr := c.list(id)
	if aerr != nil {
		return nil, err // the page's error says more than the fallback's
	}
	files = nil
	for _, f := range apiFiles {
		files = append(files, packFile{ID: f.ID, Name: f.Name, Size: f.Size, DateUpload: f.DateUpload})
	}
	return files, nil
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

// notAZip reports the API answers that mean "this file is not a zip (any more)": it is
// skipped, not a failure (the first posts also link to a .txt, for instance).
func notAZip(err error) bool {
	var pe *pixeldrainError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// RunPixeldrainPacks indexes the mod pack zips of config/forum-packs. It returns how many
// GUIDs are new and how many links could not be read (their old entries are kept).
func RunPixeldrainPacks(args []string) (newGuids, failures int) {
	fs := flag.NewFlagSet("pixeldrain-packs", flag.ExitOnError)
	dir := fs.String("packs-dir", forumPacksDir, "directory with All_Mod_List.txt and the saved forum pages / links.txt")
	modlist := fs.String("modlist", "", "mod list (default <packs-dir>/All_Mod_List.txt)")
	dataDir := fs.String("data-dir", defaultDataDir, "the index is written to <dir>/pixeldrain/"+forumPacksIndexName+".json")
	cacheDir := fs.String("cache-dir", defaultCacheDir, "the zip listings are cached in <dir>/pixeldrain/"+forumPacksIndexName+".json")
	maxURLs := fs.Int("max-urls", defaultMaxURLs, "most URLs kept per GUID")
	full := fs.Bool("full", false, "ignore the cache and list every zip again")
	_ = fs.Bool("retry-failed", false, "accepted for compatibility with the other sources (zips that failed are always tried again)")
	_ = fs.Parse(args)
	if *modlist == "" {
		*modlist = filepath.Join(*dir, "All_Mod_List.txt")
	}
	return buildPackIndex(*dir, *modlist, filepath.Join(*dataDir, pixeldrainKind, forumPacksIndexName+".json"),
		filepath.Join(*cacheDir, pixeldrainKind, forumPacksIndexName+".json"), *full, max(1, *maxURLs))
}

// packCacheEntry remembers the listing of one zip, valid while its size and upload date
// (the cheap /info call) are unchanged. The mapping to GUIDs is redone every run from the
// cached entries, so a changed mod list is picked up without listing anything again.
type packCacheEntry struct {
	Size       int64      `json:"size"`
	DateUpload string     `json:"date_upload"`
	Entries    []zipEntry `json:"entries"`
}

type packCacheFile struct {
	Format int                       `json:"format"`
	Zips   map[string]packCacheEntry `json:"zips"`
	Lists  map[string][]packFile     `json:"lists,omitempty"` // /l/ id -> its zips, for a day the list page is unreachable
}

func loadPackCache(path string) packCacheFile {
	empty := packCacheFile{Format: 1, Zips: map[string]packCacheEntry{}, Lists: map[string][]packFile{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var f packCacheFile
	if json.Unmarshal(data, &f) != nil {
		return empty
	}
	if f.Zips == nil {
		f.Zips = empty.Zips
	}
	if f.Lists == nil {
		f.Lists = empty.Lists
	}
	return f
}

func savePackCache(path string, cache packCacheFile) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return nil // unchanged: no diff
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func buildPackIndex(dir, modlistPath, out, cachePath string, full bool, maxURLs int) (newGuids, failures int) {
	ml, err := parseModList(modlistPath)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("No mod list; skipping the forum mod packs", "source", pixeldrainKind, "path", modlistPath)
			return 0, 0
		}
		slog.Error("Could not read the mod list", "path", modlistPath, "err", err)
		return 0, 1
	}
	links, err := readPackLinks(dir)
	if err != nil {
		slog.Error("Could not read the pack links", "dir", dir, "err", err)
		return 0, 1
	}
	if len(links) == 0 {
		slog.Info("No pack links; nothing to do", "source", pixeldrainKind, "dir", dir)
		return 0, 0
	}
	slog.Info("Mod list read", "source", pixeldrainKind, "mods", ml.lines, "packs", len(ml.byPack), "links", len(links))

	prev := readIndexGuids(out)
	client := newPixeldrainClient()
	oldCache := packCacheFile{}
	if !full {
		oldCache = loadPackCache(cachePath)
	}
	newCache := packCacheFile{Format: 1, Zips: map[string]packCacheEntry{}, Lists: map[string][]packFile{}}

	// Step 1: the zips to look at. /u/ links give one file (its size and date are read below);
	// /l/ links give their files, with size and date, from the list page.
	type target struct {
		packFile
		known bool // size and date are known from a list
	}
	var targets []target
	seen := map[string]int{} // file id -> index in targets
	add := func(f packFile, known bool) {
		if i, ok := seen[f.ID]; ok {
			if known && !targets[i].known {
				targets[i] = target{f, true}
			}
			return
		}
		seen[f.ID] = len(targets)
		targets = append(targets, target{f, known})
	}
	var failed []string
	for _, l := range links {
		if !l.List {
			add(packFile{ID: l.ID}, false)
			continue
		}
		files, err := client.listFiles(l.ID)
		if err != nil {
			if notAZip(err) {
				slog.Info("List not available; skipped", "source", pixeldrainKind, "list", l.ID, "err", err)
				continue
			}
			slog.Error("Could not read a list", "source", pixeldrainKind, "list", l.ID, "err", err)
			failures++
			// Use the files this list had last time: they are served from the cache.
			files = oldCache.Lists[l.ID]
			if len(files) > 0 {
				newCache.Lists[l.ID] = files
			}
		} else {
			newCache.Lists[l.ID] = files
		}
		n := 0
		for _, f := range files {
			if f.ID != "" && isPackZip(f.Name) && f.Size > 0 {
				add(f, true)
				n++
			}
		}
		slog.Info("List read", "source", pixeldrainKind, "list", l.ID, "files", len(files), "zips", n)
	}

	// Step 2: list each zip (unless the cache has it) and map its files to GUIDs.
	index := map[string][]string{}
	totalFiles, totalMatched, relisted := 0, 0, 0
	for _, t := range targets {
		f, id := t.packFile, t.ID
		c, cached := oldCache.Zips[id]
		if !t.known {
			info, err := client.fileInfo(id) // one light call: name, size, upload date
			if err != nil {
				if notAZip(err) {
					slog.Info("Not available; skipped", "source", pixeldrainKind, "id", id, "err", err)
					continue
				}
				slog.Error("Could not look at a pack link", "source", pixeldrainKind, "id", id, "err", err)
				failures++
				failed = append(failed, id)
				if cached {
					newCache.Zips[id] = c // keep what is known
				}
				continue
			}
			f = packFile{ID: id, Name: info.Name, Size: info.Size, DateUpload: info.DateUpload}
			if !isPackZip(f.Name) || f.Size <= 0 {
				slog.Info("Not a zip; skipped", "source", pixeldrainKind, "id", id, "name", f.Name)
				continue
			}
		}
		var entries []zipEntry
		if cached && c.Size == f.Size && c.DateUpload == f.DateUpload && len(c.Entries) > 0 {
			entries = c.Entries
		} else {
			var err error
			entries, err = pixeldrainRetry(func() ([]zipEntry, error) { return client.zipList(id) })
			if err != nil {
				if notAZip(err) {
					slog.Info("Not a zip; skipped", "source", pixeldrainKind, "id", id, "err", err)
					continue
				}
				slog.Error("Could not list a zip", "source", pixeldrainKind, "id", id, "err", err)
				failures++
				failed = append(failed, id)
				if cached {
					newCache.Zips[id] = c
				}
				continue
			}
			relisted++
		}
		newCache.Zips[id] = packCacheEntry{Size: f.Size, DateUpload: f.DateUpload, Entries: entries}
		part, unmatched, packs := mapZip(id, entries, ml)
		n := 0
		for g, urls := range part {
			index[g] = append(index[g], urls...)
			n += len(urls)
		}
		totalFiles += n + len(unmatched)
		totalMatched += n
		slog.Info("Zip mapped", "source", pixeldrainKind, "id", id, "name", f.Name, "packs", packs, "entries", len(entries), "mapped", n, "unmatched", len(unmatched))
		for i, u := range unmatched {
			if i == 5 {
				slog.Warn("  ...and more unmatched files", "id", id, "more", len(unmatched)-5)
				break
			}
			slog.Warn("  no GUID in the mod list for", "id", id, "file", u)
		}
	}
	if err := savePackCache(cachePath, newCache); err != nil {
		slog.Warn("Could not save the pack cache", "path", cachePath, "err", err)
	}
	slog.Info("Zips listed", "source", pixeldrainKind, "zips", len(targets), "listed_again", relisted, "from_cache", len(newCache.Zips)-relisted)

	// A zip that could not be read keeps its old entries, so one bad moment doesn't drop it.
	for g, urls := range prev {
		for _, u := range urls {
			for _, id := range failed {
				if strings.Contains(u, "/file/"+id+"/info/zip/") {
					index[g] = append(index[g], u)
				}
			}
		}
	}
	for g, urls := range index {
		index[g] = dedupeCap(urls, maxURLs)
	}
	if len(index) == 0 && failures > 0 {
		return 0, failures // nothing new and nothing to keep: leave the file alone
	}
	for g := range index {
		if _, ok := prev[g]; !ok {
			newGuids++
		}
	}
	wrote, err := writeIndex(out, index)
	if err != nil {
		slog.Error("Could not write the index", "path", out, "err", err)
		return 0, failures + 1
	}
	slog.Info("Index written", "source", pixeldrainKind, "name", forumPacksIndexName, "guids", len(index),
		"mod_files", totalFiles, "mapped", totalMatched, "path", out, "changed", wrote)
	return newGuids, failures
}

// dedupeCap removes duplicates (keeping order) and keeps at most max URLs.
func dedupeCap(urls []string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range urls {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}