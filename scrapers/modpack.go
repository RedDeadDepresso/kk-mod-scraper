package scrapers

// Sideloader Modpack GUID indexes, built straight from the BetterRepack download server.
//
// How it works:
//  1. Crawl the server's directory listings (Apache + Apaxy theme). Every row gives the
//     file's properly encoded URL, its "last modified" date and its size.
//  2. For every .zipmod that is new or changed since the last run, read its GUID and version
//     from the server with HTTP Range requests (the zip's tail, then only manifest.xml - a
//     few KB per mod, never the whole file).
//  3. Write one index, {guid: [url, ...]}, best URL first, covering the categories of both
//     games (kkCategories and kksCategories). The URLs say which folder, and so which game,
//     a file is in.
//
// Incremental updates: the listing's date and size strings are the change signature. Files
// whose signature is unchanged keep their cached GUID and are not probed; a file that only
// moved or was renamed (same filename, date and size) keeps it too. The cache lives in the
// repo (cache/modpack.json).
//
// Not indexed: "Sideloader Modpack - Bleeding Edge" (not part of the BetterRepack game
// release) and the EmotionCreators categories. Edit kkCategories / kksCategories below to
// change what the index contains.
//
// Output (data/modpack.json):
//
//	{
//	  "generated": "2025-01-01T12:00:00",
//	  "count":     5800,
//	  "guids":     {"com.example.mod": ["https://.../Sideloader%20Modpack/a/mod.zipmod", ...], ...}
//	}
//
// The URLs are percent-encoded exactly as the server lists them.
//
// URLs of one GUID are ordered best first: main "Sideloader Modpack" folder before the
// specialised categories (studio last), then newest manifest version, then newest file date.
// Identical copies (same filename, date and size) in several folders are listed once, and at
// most -max-urls URLs are kept per GUID.

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

const (
	defaultModpackBaseURL = "https://sideload.betterrepack.com/download/KKEC/"
	modpackUserAgent      = "kk-mod-scraper-modpack-index-builder/1.0"

	mainCategory   = "Sideloader Modpack"
	studioCategory = "Sideloader Modpack - Studio"

	defaultMaxURLs = 4
	cacheFormat    = 1

	defaultModpackCache = "cache/modpack.json"
	defaultDataDir      = "data"
)

// Server folders (first level under the base URL) of each game, in ranking order: the main
// folder always first. Anything else on the server (Bleeding Edge, EC_*) is neither crawled
// nor indexed.
var kkCategories = []string{
	mainCategory,
	"Sideloader Modpack - Exclusive KK KKS",
	"Sideloader Modpack - Exclusive KK",
	"Sideloader Modpack - Animations",
	"Sideloader Modpack - Fixes",
	"Sideloader Modpack - KK_MaterialEditor",
	"Sideloader Modpack - KK_UncensorSelector",
	"Sideloader Modpack - Maps",
	studioCategory,
}

var kksCategories = []string{
	mainCategory,
	"Sideloader Modpack - Exclusive KK KKS",
	"Sideloader Modpack - Exclusive KKS",
	"Sideloader Modpack - Animations",
	"Sideloader Modpack - KK_MaterialEditor",
	"Sideloader Modpack - KK_UncensorSelector",
	"Sideloader Modpack - KKS_MaterialEditor",
	"Sideloader Modpack - KKS_UncensorSelector",
	"Sideloader Modpack - KKS_Maps",
	studioCategory,
}

// modpackCategories is every category of both games, once, in ranking order: the main folder
// first, then the others in the order they appear in kkCategories and then kksCategories,
// with the studio folder last.
var modpackCategories = mergeCategories(kkCategories, kksCategories)

func mergeCategories(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range lists {
		for _, c := range list {
			if !seen[c] && c != studioCategory {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return append(out, studioCategory)
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

var modpackHTTPClient = &http.Client{Timeout: 30 * time.Second}

// canonical is the percent-encoded form used for requests, the cache and the index.
// Idempotent: existing %XX escapes are kept as they are.
func canonical(s string) string {
	const safe = ":/%?=&"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '-', c == '~', strings.IndexByte(safe, c) >= 0:
			sb.WriteByte(c)
		default:
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

// unquote percent-decodes s; if it isn't valid percent-encoding it is returned unchanged.
func unquote(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// urlPath is the still-encoded path of a URL ("" if it can't be parsed).
func urlPath(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.EscapedPath()
}

func urlHost(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// get issues a GET with the given extra headers and returns the response if its status is
// 2xx, otherwise an error (like Python's urlopen raising HTTPError).
func get(rawURL string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, canonical(rawURL), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", modpackUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := modpackHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		res.Body.Close()
		return nil, fmt.Errorf("HTTP Error %d: %s", res.StatusCode, http.StatusText(res.StatusCode))
	}
	return res, nil
}

// withRetries runs fn, retrying errors with a short pause.
func withRetries[T any](fn func() (T, error)) (T, error) {
	const attempts = 3
	var (
		v   T
		err error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		v, err = fn()
		if err == nil {
			return v, nil
		}
		if attempt < attempts-1 {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
		}
	}
	return v, err
}

func rangeGet(rawURL string, start, end int) ([]byte, error) {
	return withRetries(func() ([]byte, error) {
		res, err := get(rawURL, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end)})
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		switch {
		case res.StatusCode == http.StatusPartialContent:
			return io.ReadAll(res.Body)
		case res.StatusCode == http.StatusOK && start == 0:
			return io.ReadAll(io.LimitReader(res.Body, int64(end)+1))
		}
		return nil, errors.New("server ignored the Range request; stopped to avoid downloading a whole file")
	})
}

var contentRangeRe = regexp.MustCompile(`^bytes\s+(\d+)-(\d+)/(\d+)$`)

type suffixResult struct {
	data  []byte
	total int
	start int // offset the bytes start at
}

// suffixGet returns the last `length` bytes, the total file size and the offset the bytes
// start at, from one Range request.
func suffixGet(rawURL string, length int) (suffixResult, error) {
	return withRetries(func() (suffixResult, error) {
		res, err := get(rawURL, map[string]string{"Range": fmt.Sprintf("bytes=-%d", length)})
		if err != nil {
			return suffixResult{}, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusPartialContent {
			return suffixResult{}, errors.New("server ignored the tail Range request; stopped to avoid downloading a whole file")
		}
		m := contentRangeRe.FindStringSubmatch(strings.TrimSpace(res.Header.Get("Content-Range")))
		if m == nil {
			return suffixResult{}, errors.New("server returned an invalid Content-Range")
		}
		start, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[3])
		data, err := io.ReadAll(res.Body)
		if err != nil {
			return suffixResult{}, err
		}
		return suffixResult{data: data, total: total, start: start}, nil
	})
}

// ---------------------------------------------------------------------------
// Remote zipmod manifest
// ---------------------------------------------------------------------------

// permanentProbeError: the file itself can't be indexed (not a usable zipmod). Retrying
// won't help. Any other error from readRemoteManifest is a network problem.
type permanentProbeError struct{ msg string }

func (e *permanentProbeError) Error() string { return e.msg }

func permanent(format string, args ...any) error {
	return &permanentProbeError{fmt.Sprintf(format, args...)}
}

type manifestInfo struct{ Guid, Version string }

const maxManifestSize = 8 << 20

// rangeReader gives random access to a remote file. HTTP servers implement it with Range
// requests (httpRanger); Mega implements it with ranged downloads that it decrypts.
type rangeReader interface {
	// Suffix returns the last `length` bytes, the total size and the offset they start at.
	Suffix(length int) (suffixResult, error)
	// Range returns bytes start..end inclusive.
	Range(start, end int) ([]byte, error)
}

type httpRanger struct{ url string }

func (h httpRanger) Suffix(length int) (suffixResult, error) { return suffixGet(h.url, length) }
func (h httpRanger) Range(start, end int) ([]byte, error)    { return rangeGet(h.url, start, end) }

// readRemoteManifest reads the GUID and version of a zipmod on a plain HTTP server.
func readRemoteManifest(rawURL string) (manifestInfo, error) {
	return readManifest(httpRanger{rawURL})
}

// readManifest reads the GUID and version of a remote zipmod via ranged reads (tail, then
// manifest.xml). Files that can't be read (ZIP64, encrypted, no root manifest.xml, no GUID,
// ...) give a *permanentProbeError; anything else is network trouble worth retrying later.
func readManifest(r rangeReader) (manifestInfo, error) {
	tail, err := r.Suffix(256 * 1024)
	if err != nil {
		return manifestInfo{}, err
	}
	size := tail.total

	eocdAt := bytes.LastIndex(tail.data, []byte("PK\x05\x06"))
	if eocdAt < 0 || eocdAt+22 > len(tail.data) {
		return manifestInfo{}, permanent("ZIP end-of-central-directory not found")
	}
	le := binary.LittleEndian
	centralSize := le.Uint32(tail.data[eocdAt+12:])
	centralOffset := le.Uint32(tail.data[eocdAt+16:])
	if centralSize == 0xFFFFFFFF || centralOffset == 0xFFFFFFFF {
		return manifestInfo{}, permanent("ZIP64 is not supported")
	}
	cSize, cOffset := int(centralSize), int(centralOffset)

	var central []byte
	if cOffset >= tail.start && cOffset+cSize <= size {
		begin := cOffset - tail.start
		end := min(begin+cSize, len(tail.data))
		central = tail.data[begin:end]
	} else {
		central, err = r.Range(cOffset, cOffset+cSize-1)
		if err != nil {
			return manifestInfo{}, err
		}
	}

	var (
		found                       bool
		flags, method               uint16
		compressedSize, localOffset int
		position                    = 0
	)
	for position+46 <= len(central) && bytes.Equal(central[position:position+4], []byte("PK\x01\x02")) {
		h := central[position:]
		f := le.Uint16(h[8:])
		m := le.Uint16(h[10:])
		cs := le.Uint32(h[20:])
		nameLen := int(le.Uint16(h[28:]))
		extraLen := int(le.Uint16(h[30:]))
		commentLen := int(le.Uint16(h[32:]))
		lo := le.Uint32(h[42:])
		if position+46+nameLen > len(central) {
			break
		}
		name := strings.ReplaceAll(string(central[position+46:position+46+nameLen]), `\`, "/")
		if strings.EqualFold(name, "manifest.xml") {
			found = true
			flags, method, compressedSize, localOffset = f, m, int(cs), int(lo)
			break
		}
		position += 46 + nameLen + extraLen + commentLen
	}
	if !found {
		return manifestInfo{}, permanent("no manifest.xml in the zip root")
	}
	if flags&1 != 0 {
		return manifestInfo{}, permanent("encrypted zipmod")
	}

	var prefix, localHeader []byte
	if localOffset < 64*1024 {
		prefix, err = r.Range(0, min(size-1, 64*1024-1))
		if err != nil {
			return manifestInfo{}, err
		}
		if localOffset > len(prefix) {
			return manifestInfo{}, permanent("invalid zip local file header")
		}
		localHeader = prefix[localOffset:]
	} else {
		localHeader, err = r.Range(localOffset, min(size-1, localOffset+4095))
		if err != nil {
			return manifestInfo{}, err
		}
	}
	if len(localHeader) < 30 || !bytes.Equal(localHeader[:4], []byte("PK\x03\x04")) {
		return manifestInfo{}, permanent("invalid zip local file header")
	}
	dataOffset := localOffset + 30 + int(le.Uint16(localHeader[26:])) + int(le.Uint16(localHeader[28:]))

	var compressed []byte
	if prefix != nil && dataOffset+compressedSize <= len(prefix) {
		compressed = prefix[dataOffset : dataOffset+compressedSize]
	} else {
		compressed, err = r.Range(dataOffset, dataOffset+compressedSize-1)
		if err != nil {
			return manifestInfo{}, err
		}
	}

	var raw []byte
	switch method {
	case 0:
		raw = compressed
	case 8:
		raw, err = io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(compressed)), maxManifestSize))
		if err != nil {
			return manifestInfo{}, permanent("unreadable zip or manifest (%v)", err)
		}
	default:
		return manifestInfo{}, permanent("manifest.xml uses unsupported compression method %d", method)
	}

	info, err := parseManifest(raw)
	if err != nil {
		return manifestInfo{}, permanent("unreadable zip or manifest (%v)", err)
	}
	if info.Guid == "" {
		return manifestInfo{}, permanent("manifest.xml has no GUID")
	}
	return info, nil
}

// parseManifest returns the text of the first <guid> and <version> elements anywhere in the
// document (tag names compared case-insensitively, namespaces ignored). The whole document
// must be well-formed XML.
func parseManifest(raw []byte) (manifestInfo, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	text := strings.ToValidUTF8(string(raw), "\uFFFD")
	dec := xml.NewDecoder(strings.NewReader(text))
	// The text is already decoded; whatever encoding the declaration names, read it as is.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	var guid, version string
	var haveGuid, haveVersion bool
	var current *string // where text of the element being read goes
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifestInfo{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			current = nil
			switch {
			case !haveGuid && strings.EqualFold(t.Name.Local, "guid"):
				haveGuid, current = true, &guid
			case !haveVersion && strings.EqualFold(t.Name.Local, "version"):
				haveVersion, current = true, &version
			}
		case xml.CharData:
			if current != nil {
				*current += string(t)
			}
		case xml.EndElement:
			current = nil
		}
	}
	return manifestInfo{Guid: strings.TrimSpace(guid), Version: strings.TrimSpace(version)}, nil
}

// ---------------------------------------------------------------------------
// Directory listing crawl (Apache mod_autoindex + Apaxy)
// ---------------------------------------------------------------------------

type fileSig struct{ Mtime, Size string }

type listingRow struct {
	parent            bool
	href, mtime, size string
}

// parseListingRows reads the rows of an Apaxy listing:
// <tr><td class="indexcolname"><a href>, indexcollastmod, indexcolsize.
func parseListingRows(body string) []listingRow {
	var (
		rows []listingRow
		row  *listingRow
		cell string
	)
	z := html.NewTokenizer(strings.NewReader(body))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return rows
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			attrs := map[string]string{}
			for _, a := range tok.Attr {
				attrs[a.Key] = a.Val
			}
			classes := strings.Fields(attrs["class"])
			hasClass := func(c string) bool {
				for _, x := range classes {
					if x == c {
						return true
					}
				}
				return false
			}
			switch {
			case tok.Data == "tr":
				row = &listingRow{parent: hasClass("parent")}
				cell = ""
			case tok.Data == "td" && row != nil:
				cell = ""
				for _, c := range [][2]string{{"name", "indexcolname"}, {"mtime", "indexcollastmod"}, {"size", "indexcolsize"}} {
					if hasClass(c[1]) {
						cell = c[0]
						break
					}
				}
			case tok.Data == "a" && row != nil && cell == "name" && row.href == "":
				row.href = attrs["href"]
			}
		case html.TextToken:
			if row != nil {
				text := string(z.Text())
				switch cell {
				case "mtime":
					row.mtime += text
				case "size":
					row.size += text
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "td":
				cell = ""
			case "tr":
				if row != nil {
					rows = append(rows, *row)
					row = nil
				}
			}
		}
	}
}

// parseListing returns (subdirectory URLs, {file URL: signature}) of one listing page.
// Only links that stay under baseURL are kept, and only .zipmod files.
func parseListing(body, currentURL, baseURL string) ([]string, map[string]fileSig) {
	baseHost := urlHost(baseURL)
	basePath := unquote(urlPath(baseURL))
	cur, err := url.Parse(currentURL)
	if err != nil {
		return nil, nil
	}
	var dirs []string
	files := map[string]fileSig{}
	for _, row := range parseListingRows(body) {
		href := row.href
		if href == "" || row.parent || strings.HasPrefix(href, "?") || strings.HasPrefix(href, "#") {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		absolute := canonical(cur.ResolveReference(ref).String())
		path := urlPath(absolute)
		if urlHost(absolute) != baseHost || !strings.HasPrefix(unquote(path), basePath) {
			continue
		}
		if strings.HasSuffix(path, "/") {
			dirs = append(dirs, absolute)
		} else if strings.HasSuffix(strings.ToLower(path), ".zipmod") {
			files[absolute] = fileSig{
				Mtime: strings.Join(strings.Fields(row.mtime), " "),
				Size:  strings.TrimSpace(row.size),
			}
		}
	}
	return dirs, files
}

type crawlResult struct {
	files  map[string]fileSig
	failed []string // directory URLs that couldn't be read
}

// crawl walks the listed categories. Only the root listing failing is an error.
func crawl(baseURL string, categories map[string]bool, workers int) (crawlResult, error) {
	type listing struct {
		dirs  []string
		files map[string]fileSig
	}
	fetch := func(directory string) (listing, error) {
		body, err := withRetries(func() (string, error) {
			res, err := get(directory, nil)
			if err != nil {
				return "", err
			}
			defer res.Body.Close()
			b, err := io.ReadAll(res.Body)
			return string(b), err
		})
		if err != nil {
			return listing{}, err
		}
		d, f := parseListing(body, directory, baseURL)
		return listing{d, f}, nil
	}

	root := canonical(baseURL)
	rootListing, err := fetch(root)
	if err != nil {
		return crawlResult{}, fmt.Errorf("could not read %s: %w", root, err)
	}
	wanted := map[string]string{}
	for _, d := range rootListing.dirs {
		p := strings.TrimRight(unquote(urlPath(d)), "/")
		wanted[p[strings.LastIndex(p, "/")+1:]] = d
	}
	var missing []string
	for c := range categories {
		if _, ok := wanted[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		slog.Warn("Categories not found on the server", "categories", strings.Join(missing, ", "))
	}
	var names []string
	for c := range categories {
		if _, ok := wanted[c]; ok {
			names = append(names, c)
		}
	}
	sort.Strings(names)

	type result struct {
		dir string
		l   listing
		err error
	}
	var (
		files    = map[string]fileSig{}
		failed   []string
		seen     = map[string]bool{}
		results  = make(chan result)
		sem      = make(chan struct{}, workers)
		inflight int
		read     int
		wg       sync.WaitGroup
	)
	launch := func(dir string) {
		seen[dir] = true
		inflight++
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			l, err := fetch(dir)
			results <- result{dir, l, err}
		}()
	}
	for _, c := range names {
		if !seen[wanted[c]] {
			launch(wanted[c])
		}
	}
	for inflight > 0 {
		r := <-results
		inflight--
		read++
		if r.err != nil {
			failed = append(failed, r.dir)
			slog.Warn("Could not read directory", "path", unquote(urlPath(r.dir)), "err", r.err)
			continue
		}
		for u, sig := range r.l.files {
			files[u] = sig
		}
		for _, sub := range r.l.dirs {
			if !seen[sub] {
				launch(sub)
			}
		}
		if read%200 == 0 {
			slog.Info("Crawling", "directories", read, "zipmods", len(files))
		}
	}
	wg.Wait()
	slog.Info("Crawled", "directories", read, "zipmods", len(files))
	sort.Strings(failed)
	return crawlResult{files: files, failed: failed}, nil
}

// ---------------------------------------------------------------------------
// Cache ({url: {mtime, size, guid, version, error}}); the Mega and Drive scrapers use the same
// format for their own cache files
// ---------------------------------------------------------------------------

type cacheEntry struct {
	Mtime   string `json:"mtime"`
	Size    string `json:"size"`
	Guid    string `json:"guid,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

func loadCache(path string) map[string]cacheEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]cacheEntry{}
	}
	var doc struct {
		Format int                   `json:"format"`
		Files  map[string]cacheEntry `json:"files"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.Format != cacheFormat || doc.Files == nil {
		return map[string]cacheEntry{}
	}
	return doc.Files
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveCache writes the cache with one entry per line (valid JSON, but git diffs stay small).
func saveCache(path string, entries map[string]cacheEntry) error {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "{\"format\":%d,\"files\":{", cacheFormat)
	for i, k := range keys {
		key, err := marshalNoEscape(k)
		if err != nil {
			return err
		}
		val, err := marshalNoEscape(entries[k])
		if err != nil {
			return err
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteString("\n}}\n")
	return writeFileAtomic(path, buf.Bytes())
}

func filenameOf(u string) string {
	p := urlPath(u)
	return strings.ToLower(unquote(p[strings.LastIndex(p, "/")+1:]))
}

func categoryOf(u, baseURL string) string {
	rel := strings.TrimPrefix(unquote(urlPath(u)), unquote(urlPath(baseURL)))
	if i := strings.Index(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return rel
}

type refreshOptions struct {
	full, retryFailed bool
	workers           int
	cachePath         string

	// Optional hooks; the defaults suit the BetterRepack server.
	probe   func(url string) (manifestInfo, error) // reads GUID/version (default readRemoteManifest)
	label   func(url string) string                // human-readable name for log lines
	noMoves bool                                   // don't match new files to vanished ones by name/date/size
}

// refreshCache brings the cache in line with the listing, probing only new/changed files.
// Returns the new cache and the number of files that couldn't be probed this time.
func refreshCache(listing map[string]fileSig, cache map[string]cacheEntry, failedDirs []string,
	opt refreshOptions) (map[string]cacheEntry, int) {
	if opt.probe == nil {
		opt.probe = readRemoteManifest
	}
	if opt.label == nil {
		opt.label = func(u string) string { return unquote(urlPath(u)) }
	}
	old := cache
	if opt.full {
		old = map[string]cacheEntry{}
	}

	// Entries below a directory that couldn't be read are kept as they are rather than dropped.
	result := map[string]cacheEntry{}
	for u, e := range old {
		if _, ok := listing[u]; ok {
			continue
		}
		for _, d := range failedDirs {
			if strings.HasPrefix(u, d) {
				result[u] = e
				break
			}
		}
	}

	// Files that vanished from the listing, by (filename, mtime, size), to recognise moves/renames.
	type twinKey struct{ name, mtime, size string }
	gone := map[twinKey][]cacheEntry{}
	for u, e := range old {
		if _, inListing := listing[u]; inListing {
			continue
		}
		if _, isKept := result[u]; isKept || e.Guid == "" {
			continue
		}
		k := twinKey{filenameOf(u), e.Mtime, e.Size}
		gone[k] = append(gone[k], e)
	}

	urls := make([]string, 0, len(listing))
	for u := range listing {
		urls = append(urls, u)
	}
	sort.Strings(urls)

	var toProbe []string
	reused, moved, changed := 0, 0, 0
	for _, u := range urls {
		sig := listing[u]
		e, known := old[u]
		if known && e.Mtime == sig.Mtime && e.Size == sig.Size && (e.Guid != "" || (e.Error != "" && !opt.retryFailed)) {
			result[u] = e
			reused++
			continue
		}
		if known {
			changed++
		}
		twin := gone[twinKey{filenameOf(u), sig.Mtime, sig.Size}]
		if !known && !opt.noMoves && len(twin) == 1 {
			t := twin[0]
			t.Mtime, t.Size = sig.Mtime, sig.Size
			result[u] = t
			moved++
			continue
		}
		toProbe = append(toProbe, u)
	}
	slog.Info("Cache refreshed", "reused", reused, "moved_or_renamed", moved, "to_probe", len(toProbe),
		"changed", changed, "new", len(toProbe)-changed)
	if len(old) > 0 && len(toProbe) > max(50, len(listing)/2) {
		slog.Warn("Most files look changed - the server may have reset its dates. Probing them all.")
	}

	type probeResult struct {
		url       string
		info      manifestInfo
		err       error
		permanent bool
	}
	jobs := make(chan string)
	results := make(chan probeResult)
	var wg sync.WaitGroup
	for i := 0; i < opt.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				info, err := opt.probe(u)
				var pe *permanentProbeError
				results <- probeResult{u, info, err, errors.As(err, &pe)}
			}
		}()
	}
	go func() {
		for _, u := range toProbe {
			jobs <- u
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	unreachable, permanentCount, done := 0, 0, 0
	save := func() {
		if err := saveCache(opt.cachePath, result); err != nil {
			slog.Error("Could not save the cache", "path", opt.cachePath, "err", err)
		}
	}
	for r := range results {
		sig := listing[r.url]
		done++
		switch {
		case r.err == nil:
			result[r.url] = cacheEntry{Mtime: sig.Mtime, Size: sig.Size, Guid: r.info.Guid, Version: r.info.Version}
		case r.permanent:
			permanentCount++
			result[r.url] = cacheEntry{Mtime: sig.Mtime, Size: sig.Size, Error: r.err.Error()}
			slog.Info("Skipping unindexable zipmod", "path", opt.label(r.url), "reason", r.err)
		default:
			unreachable++
			slog.Warn("Could not probe zipmod", "path", opt.label(r.url), "err", r.err)
		}
		if done%200 == 0 || done == len(toProbe) {
			slog.Info("Probing", "done", done, "total", len(toProbe))
		}
		if done%500 == 0 {
			save() // a long first run isn't lost if it is cut short
		}
	}
	save()
	if permanentCount > 0 {
		slog.Info("Some zipmods can't be indexed (recorded; use -retry-failed to try again)", "count", permanentCount)
	}
	return result, unreachable
}

// ---------------------------------------------------------------------------
// Index
// ---------------------------------------------------------------------------

var digitsRe = regexp.MustCompile(`\d+`)

// versionParts are the numbers in a version string, compared like Python tuples of ints.
func versionParts(v string) []string {
	parts := digitsRe.FindAllString(v, -1)
	for i, p := range parts {
		if t := strings.TrimLeft(p, "0"); t == "" {
			parts[i] = "0"
		} else {
			parts[i] = t
		}
	}
	return parts
}

func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if len(pa[i]) != len(pb[i]) {
			if len(pa[i]) < len(pb[i]) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(pa) < len(pb):
		return -1
	case len(pa) > len(pb):
		return 1
	}
	return 0
}

// buildGameIndex returns {guid: [url, ...]} for the given categories, best URL first.
func buildGameIndex(entries map[string]cacheEntry, categories []string, baseURL string, maxURLs int) map[string][]string {
	rank := map[string]int{}
	for i, c := range categories {
		rank[c] = i
	}
	type item struct {
		url string
		e   cacheEntry
	}
	byGuid := map[string][]item{}
	for u, e := range entries {
		if e.Guid == "" {
			continue
		}
		if _, ok := rank[categoryOf(u, baseURL)]; ok {
			byGuid[e.Guid] = append(byGuid[e.Guid], item{u, e})
		}
	}

	index := make(map[string][]string, len(byGuid))
	for guid, items := range byGuid {
		// Chained stable sorts, least significant key first.
		sort.SliceStable(items, func(i, j int) bool { return items[i].url < items[j].url })
		sort.SliceStable(items, func(i, j int) bool { return items[i].e.Mtime > items[j].e.Mtime })
		sort.SliceStable(items, func(i, j int) bool { return compareVersions(items[i].e.Version, items[j].e.Version) > 0 })
		sort.SliceStable(items, func(i, j int) bool {
			return rank[categoryOf(items[i].url, baseURL)] < rank[categoryOf(items[j].url, baseURL)]
		})
		type identity struct{ name, mtime, size string }
		seen := map[identity]bool{}
		urls := []string{}
		for _, it := range items {
			id := identity{filenameOf(it.url), it.e.Mtime, it.e.Size}
			if seen[id] {
				continue // an identical copy in another folder
			}
			seen[id] = true
			urls = append(urls, it.url)
		}
		if len(urls) > maxURLs {
			urls = urls[:maxURLs]
		}
		index[guid] = urls
	}
	return index
}

type modpackIndexFile struct {
	Generated string              `json:"generated"`
	Count     int                 `json:"count"`
	Guids     map[string][]string `json:"guids"`
}

// readIndexGuids returns the GUIDs of an existing index file (nil if it can't be read).
func readIndexGuids(path string) map[string][]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var prev modpackIndexFile
	if json.Unmarshal(data, &prev) != nil {
		return nil
	}
	return prev.Guids
}

// writeIndex writes an index unless the file already holds exactly these GUIDs (so a day
// without changes on the server leaves no diff). A file that still has the old "game_type"
// field is rewritten once, to drop it. Reports whether it wrote.
func writeIndex(path string, index map[string][]string) (bool, error) {
	if data, err := os.ReadFile(path); err == nil {
		var prev struct {
			modpackIndexFile
			LegacyGameType json.RawMessage `json:"game_type"`
		}
		if json.Unmarshal(data, &prev) == nil && prev.LegacyGameType == nil && reflect.DeepEqual(prev.Guids, index) {
			return false, nil
		}
	}
	doc := modpackIndexFile{
		Generated: time.Now().UTC().Format("2006-01-02T15:04:05"),
		Count:     len(index),
		Guids:     index,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(doc); err != nil {
		return false, err
	}
	return true, writeFileAtomic(path, buf.Bytes())
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

type modpackOptions struct {
	baseURL     string
	dataDir     string
	cachePath   string
	full        bool
	retryFailed bool
	maxURLs     int
	workers     int
}

const modpackIndexName = "modpack.json"

// buildModpackIndex crawls the server and writes <dataDir>/modpack.json. problems is how
// many listings/files could not be read (those mods are missing from the index until a later
// run picks them up); newGuids is how many GUIDs were not in the previous index file.
func buildModpackIndex(opt modpackOptions) (problems, newGuids int, err error) {
	baseURL := opt.baseURL
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	baseURL = canonical(baseURL)

	categories := map[string]bool{}
	for _, c := range modpackCategories {
		categories[c] = true
	}

	slog.Info("Crawling the server listings", "server", baseURL, "categories", len(categories))
	crawled, err := crawl(baseURL, categories, opt.workers)
	if err != nil {
		return 0, 0, err
	}

	cache := map[string]cacheEntry{}
	if opt.full {
		slog.Info("Full rescan requested")
	} else {
		cache = loadCache(opt.cachePath)
		slog.Info("Cache loaded", "files", len(cache))
	}
	entries, unreachable := refreshCache(crawled.files, cache, crawled.failed, refreshOptions{
		full: opt.full, retryFailed: opt.retryFailed, workers: opt.workers, cachePath: opt.cachePath,
	})

	index := buildGameIndex(entries, modpackCategories, baseURL, opt.maxURLs)
	out := filepath.Join(opt.dataDir, modpackIndexName)
	prev := readIndexGuids(out)
	for guid := range index {
		if _, ok := prev[guid]; !ok {
			newGuids++
		}
	}
	wrote, err := writeIndex(out, index)
	if err != nil {
		return 0, 0, fmt.Errorf("could not write %s: %w", out, err)
	}
	urls := 0
	for _, v := range index {
		urls += len(v)
	}
	slog.Info("Modpack index", "guids", len(index), "urls", urls, "path", out, "changed", wrote)

	problems = len(crawled.failed) + unreachable
	if problems > 0 {
		slog.Warn("Some mods are missing from the index; the next run will try them again",
			"unreadable_listings", len(crawled.failed), "unreachable_files", unreachable)
	}
	return problems, newGuids, nil
}

// RunModpack builds the modpack index and returns how many GUIDs are new. args are
// command-line style flags (nil = defaults).
func RunModpack(args []string) int {
	fs := flag.NewFlagSet("modpack", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "where modpack.json is written")
	cachePath := fs.String("cache", defaultModpackCache, "incremental-update cache")
	baseURL := fs.String("base-url", defaultModpackBaseURL, "BetterRepack download server")
	full := fs.Bool("full", false, "ignore the cache and probe every file")
	retryFailed := fs.Bool("retry-failed", false, "probe again the files recorded as not indexable")
	maxURLs := fs.Int("max-urls", defaultMaxURLs, "most URLs kept per GUID")
	workers := fs.Int("workers", 8, "parallel requests (max 16)")
	_ = fs.Parse(args)

	_, newGuids, err := buildModpackIndex(modpackOptions{
		baseURL:     *baseURL,
		dataDir:     *dataDir,
		cachePath:   *cachePath,
		full:        *full,
		retryFailed: *retryFailed,
		maxURLs:     max(1, *maxURLs),
		workers:     max(1, min(*workers, 16)),
	})
	if err != nil {
		fatal("Modpack index build failed", "err", err)
	}
	return newGuids
}
