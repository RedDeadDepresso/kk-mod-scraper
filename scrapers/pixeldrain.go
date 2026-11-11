package scrapers

// GUID index for zipmods hosted on Pixeldrain.
//
// Pixeldrain serves files unencrypted and honours Range requests, so this is the same trick
// as for Google Drive, over Pixeldrain's public API:
//   - GET /api/list/<id> gives the files of a list (https://pixeldrain.com/l/<id>): id, name,
//     size and upload date of each;
//   - GET /api/file/<id>/info gives the same for a single file (https://pixeldrain.com/u/<id>);
//   - GET /api/file/<id> with a Range header returns just those bytes, so per new or changed
//     file only the zip's tail and manifest.xml are read (a few KB), never the whole file.
//
// No account is needed. Pixeldrain does throttle anonymous downloads now and then (a 403 or
// 429 asking for a captcha); those files are simply tried again on the next run. If that
// happens a lot, an API key (https://pixeldrain.com/user/api_keys) in the PIXELDRAIN_API_KEY
// environment variable (a GitHub Actions secret) raises the limits. It is sent in a request
// header and never appears in URLs, logs or indices.
//
// The newer file-system shares (https://pixeldrain.com/d/<share id>[/<path>]) work the same
// way over GET /api/filesystem/<path>?stat (the node and, for a directory, its children) and
// GET /api/filesystem/<path> (the bytes, with Range). A /d/ link is a folder: it is walked
// recursively, so a link to a share root or to any sub-directory indexes every .zipmod/.zip
// below it. A /d/ link that turns out to point at one file indexes just that file. The URLs
// in the index are https://pixeldrain.com/d/<share id>/<path to the file>.
//
// config/pixeldrain.json is the usual grouped config (see hostedconfig.go) of list links
// (https://pixeldrain.com/l/<id>), directory shares (https://pixeldrain.com/d/<id>/...) and
// file links (https://pixeldrain.com/u/<id>). Output per list or directory, named after it:
// data/pixeldrain/<title>.json and cache/pixeldrain/<title>.json (see hosted.go); all file
// links are indexed together in "(single files)". The URLs in the index of a list are
// https://pixeldrain.com/u/<file id>.
//
// Only .zipmod and .zip files are looked at. A .zip with a manifest.xml at its root is one mod.
// A .zip without one is treated as a pack of mods: Pixeldrain's zip viewer exposes the
// entries of a zip (GET /api/file/<id>/info/zip lists them, GET /api/file/<id>/info/zip/<path>
// serves one), so every .zipmod inside is indexed on its own, with that direct entry URL
// (https://pixeldrain.com/api/file/<id>/info/zip/<path>) as its link. The entry URLs ignore
// Range requests, so the manifests are NOT read from them: the pack itself (/api/file/<id>)
// does honour Range, so its central directory is read once, which says where each entry's
// bytes lie inside the pack, and the entry's own zip tail and manifest.xml are read from
// there (a stored entry with ranged reads; a deflated one is fetched and inflated in memory,
// up to pixeldrainMaxInflate). This works for /u/ and /l/ links; /d/ file-system shares do
// not have the zip viewer API and are still read as plain files.

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pixeldrainKind      = "pixeldrain"
	pixeldrainUserAgent = "kk-mod-scraper-pixeldrain-index-builder/1.0"
	pixeldrainKeyEnv    = "PIXELDRAIN_API_KEY"
)

var (
	pixeldrainAPIBase    = "https://pixeldrain.com/api" // a variable so tests can point it at a fake
	pixeldrainHTTPClient = &http.Client{Timeout: 60 * time.Second}
)

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

// pixeldrainHost matches the domains Pixeldrain is reachable under.
const pixeldrainHost = `(?:www\.)?(?:pixeldrain\.(?:com|net|dev)|pixeldra\.in)`

var (
	pixeldrainListRe = regexp.MustCompile(
		`(?i)^https?://` + pixeldrainHost + `/(?:l/|api/list/)([A-Za-z0-9]{6,12})(?:[/?&#].*)?$`)
	pixeldrainFileRe = regexp.MustCompile(
		`(?i)^https?://` + pixeldrainHost + `/(?:u/|api/file/)([A-Za-z0-9]{6,12})(?:[/?&#].*)?$`)

	// A file-system share: /d/<share id>, optionally followed by a path inside it.
	pixeldrainDirRe = regexp.MustCompile(
		`(?i)^https?://` + pixeldrainHost + `/(?:d/|api/filesystem/)([A-Za-z0-9_-]{4,64})((?:/[^?#]*)?)(?:[?#].*)?$`)

	// Used to pull links out of free text (a post): the \b stops at the end of the id, so a
	// trailing comma or bracket is not part of it, and an over-long token is not a link.
	pixeldrainFindRe = regexp.MustCompile(
		`(?i)https?://` + pixeldrainHost + `/(u/|l/|api/file/|api/list/)([A-Za-z0-9]{6,12})\b`)
	// Directory shares: the path ends at whitespace, quotes or brackets, and trailing
	// punctuation is not part of it.
	pixeldrainFindDirRe = regexp.MustCompile(
		`(?i)https?://` + pixeldrainHost + `/(?:d/|api/filesystem/)([A-Za-z0-9_-]{4,64})((?:/[^\s"'<>()\[\]?#]*)?)`)
)

// parsePixeldrainDirLink returns the share id and the path inside the share (decoded,
// "" for the share root, no trailing slash) of a /d/ link.
func parsePixeldrainDirLink(s string) (share, path string, err error) {
	m := pixeldrainDirRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", "", fmt.Errorf("not a Pixeldrain directory link: %q (expected https://pixeldrain.com/d/<id>)", s)
	}
	return m[1], cleanPixeldrainSubpath(m[2]), nil
}

// cleanPixeldrainSubpath percent-decodes a path and drops empty segments.
func cleanPixeldrainSubpath(raw string) string {
	var segs []string
	for _, seg := range strings.Split(raw, "/") {
		if d, err := url.PathUnescape(seg); err == nil {
			seg = d
		}
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return ""
	}
	return "/" + strings.Join(segs, "/")
}

// pixeldrainEscapePath percent-encodes each segment of a "/a/b/c" path.
func pixeldrainEscapePath(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(segs, "/")
}

// pixeldrainDirURL is the canonical link of a path ("/<share>/dir/file") in the file system.
func pixeldrainDirURL(fsPath string) string {
	return "https://pixeldrain.com/d" + pixeldrainEscapePath(fsPath)
}

func parsePixeldrainListLink(s string) (string, error) {
	m := pixeldrainListRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("not a Pixeldrain list link: %q (expected https://pixeldrain.com/l/<id>)", s)
	}
	return m[1], nil
}

func parsePixeldrainFileLink(s string) (string, error) {
	m := pixeldrainFileRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("not a Pixeldrain file link: %q (expected https://pixeldrain.com/u/<id>)", s)
	}
	return m[1], nil
}

func pixeldrainListURL(id string) string { return "https://pixeldrain.com/l/" + id }
func pixeldrainFileURL(id string) string { return "https://pixeldrain.com/u/" + id }

// findPixeldrainLinks returns the Pixeldrain list and file links of a text, canonical, in
// order of appearance.
func findPixeldrainLinks(text string) []string {
	var out []string
	for _, m := range pixeldrainFindDirRe.FindAllStringSubmatch(text, -1) {
		out = append(out, pixeldrainDirURL("/"+m[1]+strings.TrimRight(cleanPixeldrainSubpath(m[2]), ".,;:!")))
	}
	for _, m := range pixeldrainFindRe.FindAllStringSubmatch(text, -1) {
		switch strings.ToLower(m[1]) {
		case "l/", "api/list/":
			out = append(out, pixeldrainListURL(m[2]))
		default:
			out = append(out, pixeldrainFileURL(m[2]))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

// pixeldrainError is an error answer from the Pixeldrain API.
type pixeldrainError struct {
	Status  int
	Value   string // machine readable, like "file_not_found"
	Message string
}

func (e *pixeldrainError) Error() string {
	if e.Value != "" {
		return fmt.Sprintf("Pixeldrain API error %d (%s): %s", e.Status, e.Value, e.Message)
	}
	return fmt.Sprintf("Pixeldrain API error %d: %s", e.Status, e.Message)
}

// permanent reports whether the file or list is gone (404). Everything else, 403 captcha
// and rate limit answers included, is retried on the next run and never purges a link.
func (e *pixeldrainError) permanent() bool { return e.Status == http.StatusNotFound }

func (e *pixeldrainError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func parsePixeldrainError(status int, body []byte) *pixeldrainError {
	e := &pixeldrainError{Status: status, Message: http.StatusText(status)}
	var doc struct {
		Value   string `json:"value"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &doc) == nil {
		e.Value = doc.Value
		if doc.Message != "" {
			e.Message = doc.Message
		}
	}
	return e
}

type pixeldrainClient struct{ key string }

// newPixeldrainClient never fails: the API key is optional.
func newPixeldrainClient() *pixeldrainClient {
	return &pixeldrainClient{key: strings.TrimSpace(os.Getenv(pixeldrainKeyEnv))}
}

// do sends one request. 2xx responses are returned (the caller closes the body); anything
// else becomes a *pixeldrainError.
func (c *pixeldrainClient) do(rawURL string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", pixeldrainUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	if c.key != "" {
		req.SetBasicAuth("", c.key) // Pixeldrain: empty user name, the API key as password
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := pixeldrainHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		return nil, parsePixeldrainError(res.StatusCode, body)
	}
	return res, nil
}

// pixeldrainRetry runs fn up to three times, retrying network errors, 429 and 5xx.
func pixeldrainRetry[T any](fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 0; attempt < 3; attempt++ {
		if v, err = fn(); err == nil {
			return v, nil
		}
		var pe *pixeldrainError
		if errors.As(err, &pe) && !pe.retryable() {
			return v, err
		}
		if attempt < 2 {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
		}
	}
	return v, err
}

func (c *pixeldrainClient) getJSON(path string, out any) error {
	_, err := pixeldrainRetry(func() (struct{}, error) {
		res, err := c.do(pixeldrainAPIBase+path, nil)
		if err != nil {
			return struct{}{}, err
		}
		defer res.Body.Close()
		return struct{}{}, json.NewDecoder(res.Body).Decode(out)
	})
	return err
}

type pixeldrainFile struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	DateUpload string `json:"date_upload"`
}

func (c *pixeldrainClient) fileInfo(id string) (pixeldrainFile, error) {
	var f pixeldrainFile
	err := c.getJSON("/file/"+id+"/info", &f)
	if err == nil && f.ID == "" {
		f.ID = id
	}
	return f, err
}

func (c *pixeldrainClient) list(id string) (title string, files []pixeldrainFile, err error) {
	var l struct {
		Title string           `json:"title"`
		Files []pixeldrainFile `json:"files"`
	}
	if err = c.getJSON("/list/"+id, &l); err != nil {
		return "", nil, err
	}
	return l.Title, l.Files, nil
}

// pixeldrainNode is one file or directory of the file system API.
type pixeldrainNode struct {
	Type     string `json:"type"` // "file" or "dir"
	Path     string `json:"path"` // "/<share id>/dir/file"
	Name     string `json:"name"`
	Modified string `json:"modified"`
	Created  string `json:"created"`
	FileSize int64  `json:"file_size"`
	Size     int64  `json:"size"`
}

func (n pixeldrainNode) isDir() bool { return n.Type == "dir" }
func (n pixeldrainNode) size() int64 {
	if n.FileSize > 0 {
		return n.FileSize
	}
	return n.Size
}
func (n pixeldrainNode) mtime() string {
	if n.Modified != "" {
		return pixeldrainMtime(n.Modified)
	}
	return pixeldrainMtime(n.Created)
}

// stat returns the node at fsPath ("/<share id>/..."), and its children if it is a directory.
func (c *pixeldrainClient) stat(fsPath string) (pixeldrainNode, []pixeldrainNode, error) {
	var doc struct {
		Path      []pixeldrainNode `json:"path"`
		BaseIndex int              `json:"base_index"`
		Children  []pixeldrainNode `json:"children"`
	}
	if err := c.getJSON("/filesystem"+pixeldrainEscapePath(fsPath)+"?stat", &doc); err != nil {
		return pixeldrainNode{}, nil, err
	}
	if len(doc.Path) == 0 {
		return pixeldrainNode{}, nil, fmt.Errorf("Pixeldrain returned no node for %s", fsPath)
	}
	idx := doc.BaseIndex
	if idx < 0 || idx >= len(doc.Path) {
		idx = len(doc.Path) - 1
	}
	node := doc.Path[idx]
	if node.Path == "" {
		node.Path = fsPath
	}
	for i := range doc.Children {
		if doc.Children[i].Path == "" {
			doc.Children[i].Path = strings.TrimRight(node.Path, "/") + "/" + doc.Children[i].Name
		}
	}
	return node, doc.Children, nil
}

const (
	pixeldrainMaxDepth = 12
	pixeldrainMaxNodes = 20000
)

// walk lists all zip files below a directory node, recursively. Names are relative to the
// starting directory.
func (c *pixeldrainClient) walk(dir pixeldrainNode, children []pixeldrainNode) (files []pixeldrainNode, names map[string]string, err error) {
	names = map[string]string{}
	visited := 0
	var rec func(d pixeldrainNode, kids []pixeldrainNode, rel string, depth int) error
	rec = func(d pixeldrainNode, kids []pixeldrainNode, rel string, depth int) error {
		for _, k := range kids {
			if visited++; visited > pixeldrainMaxNodes {
				return fmt.Errorf("more than %d entries in the directory; stopped", pixeldrainMaxNodes)
			}
			if k.isDir() {
				if depth >= pixeldrainMaxDepth {
					continue
				}
				_, sub, err := c.stat(k.Path)
				if err != nil {
					return fmt.Errorf("listing %s: %w", k.Path, err)
				}
				if err := rec(k, sub, rel+k.Name+"/", depth+1); err != nil {
					return err
				}
				continue
			}
			if k.size() <= 0 || !isZipmodName(k.Name) {
				continue
			}
			files = append(files, k)
			names[k.Path] = rel + k.Name
		}
		return nil
	}
	err = rec(dir, children, "", 0)
	return files, names, err
}

// pixeldrainMtime turns the upload date of the API (RFC 3339) into the index's format.
func pixeldrainMtime(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02 15:04")
	}
	return s
}

// ---------------------------------------------------------------------------
// Zip viewer (the entries of a zip)
// ---------------------------------------------------------------------------

const (
	pixeldrainMaxZipEntries = 2000
	pixeldrainMaxZipDepth   = 6
)

// pixeldrainZipNode is a node of GET /api/file/<id>/info/zip: the zip itself (with
// Properties) or one of its entries. A directory has a Children map; a file has none.
type pixeldrainZipNode struct {
	Size       int64                        `json:"size"`
	Children   map[string]pixeldrainZipNode `json:"children"`
	Properties []string                     `json:"properties"`
}

// zipListing returns the entries of a file that Pixeldrain can browse as a zip. It returns
// nil (and no error) when the file can't be browsed (the API answers 400 or 404 for it), so
// the caller reads it as a plain file. Any other error (rate limits, outages, captchas) is
// returned and the file is tried again on the next run.
func (c *pixeldrainClient) zipListing(id string) (*pixeldrainZipNode, error) {
	var n pixeldrainZipNode
	if err := c.getJSON("/file/"+id+"/info/zip", &n); err != nil {
		var pe *pixeldrainError
		if errors.As(err, &pe) && (pe.Status == http.StatusBadRequest || pe.Status == http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}
	readable := false
	for _, p := range n.Properties {
		if p == "read_individual_files" {
			readable = true
		}
	}
	if !readable || n.Children == nil {
		return nil, nil
	}
	return &n, nil
}

func (n *pixeldrainZipNode) hasRootManifest() bool {
	for name := range n.Children {
		if strings.EqualFold(name, "manifest.xml") {
			return true
		}
	}
	return false
}

type pixeldrainZipEntry struct {
	Path string // inside the zip, "/" separated
	Size int64
}

// entries returns the .zipmod/.zip files inside the zip, sorted by path. Names that
// Pixeldrain could not decode (they contain U+FFFD) can't be requested again, so they are
// skipped.
func (n *pixeldrainZipNode) entries() []pixeldrainZipEntry {
	var out []pixeldrainZipEntry
	var rec func(nodes map[string]pixeldrainZipNode, prefix string, depth int)
	rec = func(nodes map[string]pixeldrainZipNode, prefix string, depth int) {
		names := make([]string, 0, len(nodes))
		for name := range nodes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if len(out) >= pixeldrainMaxZipEntries {
				return
			}
			node := nodes[name]
			full := prefix + name
			if strings.ContainsRune(full, '\uFFFD') {
				continue
			}
			if node.Children != nil { // a directory
				if depth < pixeldrainMaxZipDepth {
					rec(node.Children, full+"/", depth+1)
				}
				continue
			}
			if node.Size > 0 && isZipmodName(name) {
				out = append(out, pixeldrainZipEntry{Path: full, Size: node.Size})
			}
		}
	}
	rec(n.Children, "", 0)
	return out
}

// pixeldrainZipEntryEndpoint is the API path of the bytes of one entry of a zip.
func pixeldrainZipEntryEndpoint(id, entry string) string {
	return "/file/" + id + "/info/zip" + pixeldrainEscapePath(entry)
}

// pixeldrainZipEntryURL is the direct download link of one entry of a zip.
func pixeldrainZipEntryURL(id, entry string) string {
	return "https://pixeldrain.com/api" + pixeldrainZipEntryEndpoint(id, entry)
}

// addPixeldrainFile adds one file to a folder listing. A .zip that is a pack (it has no
// manifest.xml at its root but holds .zipmod files) is added as one item per mod inside;
// anything else is added as the file itself. probes gets, per listed URL, how to read its
// GUID.
func addPixeldrainFile(c *pixeldrainClient, folder *remoteFolder, probes map[string]func() (manifestInfo, error), f pixeldrainFile) error {
	mtime := pixeldrainMtime(f.DateUpload)
	if strings.HasSuffix(strings.ToLower(f.Name), ".zip") {
		zl, err := c.zipListing(f.ID)
		if err != nil {
			return fmt.Errorf("listing the zip %q: %w", f.Name, err)
		}
		if zl != nil && !zl.hasRootManifest() {
			if entries := zl.entries(); len(entries) > 0 {
				pack := &pixeldrainPack{outer: pixeldrainFileRanger(c, f.ID, int(f.Size))}
				for _, e := range entries {
					entryPath := e.Path
					u := pixeldrainZipEntryURL(f.ID, entryPath)
					folder.Labels[u] = f.Name + " > " + entryPath
					folder.Listing[u] = fileSig{Mtime: mtime, Size: strconv.FormatInt(e.Size, 10)}
					probes[u] = func() (manifestInfo, error) { return pack.probe(entryPath) }
				}
				return nil
			}
		}
	}
	u := pixeldrainFileURL(f.ID)
	folder.Labels[u] = f.Name
	folder.Listing[u] = fileSig{Mtime: mtime, Size: strconv.FormatInt(f.Size, 10)}
	probes[u] = pixeldrainFileRanger(c, f.ID, int(f.Size)).probe
	return nil
}

// pixeldrainProbeFrom returns a Probe that reads the GUID of whichever listed item is asked for.
func pixeldrainProbeFrom(probes map[string]func() (manifestInfo, error), what string) func(string) (manifestInfo, error) {
	return func(u string) (manifestInfo, error) {
		pr, ok := probes[u]
		if !ok {
			return manifestInfo{}, permanent("file is no longer in the Pixeldrain %s", what)
		}
		return pr()
	}
}

// ---------------------------------------------------------------------------
// Reading the entries of a pack from the pack itself
// ---------------------------------------------------------------------------

// pixeldrainMaxInflate is the largest deflated entry that is fetched and inflated in memory
// to read its manifest. Stored entries need no such limit: only a few KB of them are read.
const (
	pixeldrainMaxInflate    = 128 << 20
	pixeldrainMaxCentralDir = 32 << 20
)

// zipDirEntry is one entry of a zip's central directory.
type zipDirEntry struct {
	Flags       uint16
	Method      uint16
	CompSize    int64
	UncompSize  int64
	LocalOffset int64
}

// readZipDirectory reads the central directory of a remote zip: the tail first, then the
// directory itself if it isn't inside the tail. ZIP64 is understood. Names are keyed with
// "/" separators.
func readZipDirectory(r rangeReader) (map[string]zipDirEntry, error) {
	tail, err := r.Suffix(256 * 1024)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	eocdAt := bytes.LastIndex(tail.data, []byte("PK\x05\x06"))
	if eocdAt < 0 || eocdAt+22 > len(tail.data) {
		return nil, permanent("ZIP end-of-central-directory not found")
	}
	cSize := int64(le.Uint32(tail.data[eocdAt+12:]))
	cOff := int64(le.Uint32(tail.data[eocdAt+16:]))
	if cSize == 0xFFFFFFFF || cOff == 0xFFFFFFFF || le.Uint16(tail.data[eocdAt+10:]) == 0xFFFF {
		// ZIP64: a locator right before the end record points at the ZIP64 end record.
		if eocdAt < 20 || !bytes.Equal(tail.data[eocdAt-20:eocdAt-16], []byte("PK\x06\x07")) {
			return nil, permanent("ZIP64 end-of-central-directory locator not found")
		}
		recAt := int64(le.Uint64(tail.data[eocdAt-20+8:]))
		var rec []byte
		if recAt >= int64(tail.start) && recAt+56 <= int64(tail.start+len(tail.data)) {
			rec = tail.data[recAt-int64(tail.start):]
		} else if recAt >= 0 && recAt+56 <= int64(tail.total) {
			if rec, err = r.Range(int(recAt), int(recAt)+55); err != nil {
				return nil, err
			}
		}
		if len(rec) < 56 || !bytes.Equal(rec[:4], []byte("PK\x06\x06")) {
			return nil, permanent("ZIP64 end-of-central-directory record not found")
		}
		cSize = int64(le.Uint64(rec[40:]))
		cOff = int64(le.Uint64(rec[48:]))
	}
	if cSize <= 0 || cSize > pixeldrainMaxCentralDir || cOff < 0 || cOff+cSize > int64(tail.total) {
		return nil, permanent("ZIP directory is invalid or too large")
	}
	var central []byte
	if cOff >= int64(tail.start) {
		begin := int(cOff - int64(tail.start))
		central = tail.data[begin:min(begin+int(cSize), len(tail.data))]
	} else if central, err = r.Range(int(cOff), int(cOff+cSize-1)); err != nil {
		return nil, err
	}

	entries := map[string]zipDirEntry{}
	for pos := 0; pos+46 <= len(central) && bytes.Equal(central[pos:pos+4], []byte("PK\x01\x02")); {
		h := central[pos:]
		flags := le.Uint16(h[8:])
		method := le.Uint16(h[10:])
		cs := int64(le.Uint32(h[20:]))
		us := int64(le.Uint32(h[24:]))
		nameLen := int(le.Uint16(h[28:]))
		extraLen := int(le.Uint16(h[30:]))
		commentLen := int(le.Uint16(h[32:]))
		lo := int64(le.Uint32(h[42:]))
		if pos+46+nameLen+extraLen > len(central) {
			break
		}
		name := strings.ReplaceAll(string(h[46:46+nameLen]), `\`, "/")
		if us == 0xFFFFFFFF || cs == 0xFFFFFFFF || lo == 0xFFFFFFFF {
			// ZIP64 extra field: only the values that overflowed are present, in this order.
			for extra := h[46+nameLen : 46+nameLen+extraLen]; len(extra) >= 4; {
				id, sz := le.Uint16(extra), int(le.Uint16(extra[2:]))
				if 4+sz > len(extra) {
					break
				}
				if id == 1 {
					d := extra[4 : 4+sz]
					for _, v := range []*int64{&us, &cs, &lo} {
						if *v == 0xFFFFFFFF && len(d) >= 8 {
							*v = int64(le.Uint64(d))
							d = d[8:]
						}
					}
				}
				extra = extra[4+sz:]
			}
		}
		entries[name] = zipDirEntry{Flags: flags, Method: method, CompSize: cs, UncompSize: us, LocalOffset: lo}
		pos += 46 + nameLen + extraLen + commentLen
	}
	if len(entries) == 0 {
		return nil, permanent("ZIP directory has no entries")
	}
	return entries, nil
}

// subRanger exposes a slice of a remote file as a file of its own. A stored zip entry is
// exactly that: the bytes of the zipmod, inside the pack.
type subRanger struct {
	outer     rangeReader
	off, size int
}

func (s subRanger) Suffix(length int) (suffixResult, error) {
	length = min(length, s.size)
	start := s.size - length
	data, err := s.outer.Range(s.off+start, s.off+s.size-1)
	if err != nil {
		return suffixResult{}, err
	}
	return suffixResult{data: data, total: s.size, start: start}, nil
}

func (s subRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end || end >= s.size {
		return nil, fmt.Errorf("invalid range %d-%d inside a %d-byte entry", start, end, s.size)
	}
	return s.outer.Range(s.off+start, s.off+end)
}

// memRanger is a file that is already in memory (an inflated entry).
type memRanger struct{ data []byte }

func (m memRanger) Suffix(length int) (suffixResult, error) {
	length = min(length, len(m.data))
	start := len(m.data) - length
	return suffixResult{data: m.data[start:], total: len(m.data), start: start}, nil
}

func (m memRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end || end >= len(m.data) {
		return nil, fmt.Errorf("invalid range %d-%d inside a %d-byte entry", start, end, len(m.data))
	}
	return m.data[start : end+1], nil
}

// pixeldrainPack is a Pixeldrain zip that holds mods. Its directory is read once (by whichever
// probe comes first) and shared by the probes of all its entries.
type pixeldrainPack struct {
	outer *pixeldrainRanger // the pack itself: /api/file/<id>, which honours Range
	once  sync.Once
	dir   map[string]zipDirEntry
	err   error
}

func (p *pixeldrainPack) entry(name string) (zipDirEntry, error) {
	p.once.Do(func() { p.dir, p.err = readZipDirectory(p.outer) })
	if p.err != nil {
		return zipDirEntry{}, p.err
	}
	e, ok := p.dir[name]
	if !ok {
		return zipDirEntry{}, permanent("entry %q not found in the zip's directory", name)
	}
	return e, nil
}

// probe reads the GUID and version of one entry of the pack.
func (p *pixeldrainPack) probe(name string) (manifestInfo, error) {
	info, err := p.probeEntry(name)
	var pe *pixeldrainError
	if errors.As(err, &pe) && pe.permanent() {
		return manifestInfo{}, permanent("not readable on Pixeldrain (%v)", pe)
	}
	return info, err
}

func (p *pixeldrainPack) probeEntry(name string) (manifestInfo, error) {
	e, err := p.entry(name)
	if err != nil {
		return manifestInfo{}, err
	}
	if e.Flags&1 != 0 {
		return manifestInfo{}, permanent("encrypted entry")
	}
	if e.LocalOffset < 0 || e.LocalOffset+30 > int64(p.outer.size) {
		return manifestInfo{}, permanent("invalid zip local file header")
	}
	lh, err := p.outer.Range(int(e.LocalOffset), int(e.LocalOffset)+29)
	if err != nil {
		return manifestInfo{}, err
	}
	if !bytes.Equal(lh[:4], []byte("PK\x03\x04")) {
		return manifestInfo{}, permanent("invalid zip local file header")
	}
	dataAt := e.LocalOffset + 30 + int64(binary.LittleEndian.Uint16(lh[26:])) + int64(binary.LittleEndian.Uint16(lh[28:]))
	if e.CompSize <= 0 || dataAt+e.CompSize > int64(p.outer.size) {
		return manifestInfo{}, permanent("entry data lies outside the file")
	}
	switch e.Method {
	case 0: // stored: the entry is the zipmod itself, read it in place
		return readManifest(subRanger{outer: p.outer, off: int(dataAt), size: int(e.CompSize)})
	case 8: // deflated: no random access; fetch the entry and inflate it
		if e.CompSize > pixeldrainMaxInflate || e.UncompSize > pixeldrainMaxInflate {
			return manifestInfo{}, permanent("deflated entry of %d bytes is too large to read from a pack", e.UncompSize)
		}
		comp, err := p.outer.Range(int(dataAt), int(dataAt+e.CompSize-1))
		if err != nil {
			return manifestInfo{}, err
		}
		raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(comp)), pixeldrainMaxInflate+1))
		if err != nil {
			return manifestInfo{}, permanent("unreadable entry (%v)", err)
		}
		if len(raw) > pixeldrainMaxInflate || len(raw) == 0 {
			return manifestInfo{}, permanent("deflated entry has an unusable size")
		}
		return readManifest(memRanger{raw})
	default:
		return manifestInfo{}, permanent("entry uses unsupported compression method %d", e.Method)
	}
}

// ---------------------------------------------------------------------------
// Range reader
// ---------------------------------------------------------------------------

// pixeldrainRanger reads byte ranges of one file. It implements rangeReader. The size is
// known from the listing, so no suffix ranges are needed.
type pixeldrainRanger struct {
	c        *pixeldrainClient
	endpoint string // API path of the bytes: /file/<id> or /filesystem/<path>
	size     int
}

func pixeldrainFileRanger(c *pixeldrainClient, id string, size int) *pixeldrainRanger {
	return &pixeldrainRanger{c: c, endpoint: "/file/" + id, size: size}
}

func pixeldrainDirRanger(c *pixeldrainClient, fsPath string, size int) *pixeldrainRanger {
	return &pixeldrainRanger{c: c, endpoint: "/filesystem" + pixeldrainEscapePath(fsPath), size: size}
}

func (p *pixeldrainRanger) Suffix(length int) (suffixResult, error) {
	length = min(length, p.size)
	start := p.size - length
	data, err := p.Range(start, p.size-1)
	if err != nil {
		return suffixResult{}, err
	}
	return suffixResult{data: data, total: p.size, start: start}, nil
}

func (p *pixeldrainRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end || end >= p.size {
		return nil, fmt.Errorf("invalid range %d-%d for a file of %d bytes", start, end, p.size)
	}
	want := end - start + 1
	return pixeldrainRetry(func() ([]byte, error) {
		res, err := p.c.do(pixeldrainAPIBase+p.endpoint, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end)})
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusPartialContent && !(res.StatusCode == http.StatusOK && start == 0) {
			return nil, errors.New("Pixeldrain ignored the Range request; stopped to avoid downloading a whole file")
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, int64(want)+1))
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusOK && len(data) > want {
			data = data[:want] // a full response from the start: keep just what was asked for
		}
		if len(data) != want {
			return nil, fmt.Errorf("Pixeldrain returned %d bytes for a %d-byte range", len(data), want)
		}
		return data, nil
	})
}

// probe reads the GUID and version of one file.
func (p *pixeldrainRanger) probe() (manifestInfo, error) {
	if p.size == 0 {
		return manifestInfo{}, permanent("empty file")
	}
	info, err := readManifest(p)
	var pe *pixeldrainError
	if errors.As(err, &pe) && pe.permanent() {
		return manifestInfo{}, permanent("not readable on Pixeldrain (%v)", pe)
	}
	return info, err
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

type pixeldrainProvider struct{}

func (pixeldrainProvider) Kind() string { return pixeldrainKind }

func (pixeldrainProvider) ID(raw string) (string, error) {
	if id, err := parsePixeldrainListLink(raw); err == nil {
		return id, nil
	}
	share, sub, err := parsePixeldrainDirLink(raw)
	if err != nil {
		return "", fmt.Errorf("not a Pixeldrain list or directory link: %q (expected https://pixeldrain.com/l/<id> or https://pixeldrain.com/d/<id>)", raw)
	}
	return "d:" + share + sub, nil
}

// loadPixeldrainDir lists a /d/ link: a directory is walked recursively; a file is a folder of one.
func loadPixeldrainDir(raw string) (*remoteFolder, error) {
	share, sub, err := parsePixeldrainDirLink(raw)
	if err != nil {
		return nil, err
	}
	client := newPixeldrainClient()
	node, children, err := client.stat("/" + share + sub)
	if err != nil {
		return nil, err
	}
	name := node.Name
	if name == "" {
		name = share
	}
	folder := &remoteFolder{Name: name, Listing: map[string]fileSig{}, Labels: map[string]string{}}
	rangers := map[string]*pixeldrainRanger{}
	add := func(n pixeldrainNode, label string) {
		u := pixeldrainDirURL(n.Path)
		folder.Labels[u] = label
		folder.Listing[u] = fileSig{Mtime: n.mtime(), Size: strconv.FormatInt(n.size(), 10)}
		rangers[u] = pixeldrainDirRanger(client, n.Path, int(n.size()))
	}
	if node.isDir() {
		files, names, err := client.walk(node, children)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			add(f, names[f.Path])
		}
	} else if isZipmodName(node.Name) && node.size() > 0 {
		add(node, node.Name)
	}
	folder.Probe = func(u string) (manifestInfo, error) {
		r, ok := rangers[u]
		if !ok {
			return manifestInfo{}, permanent("file is no longer in the Pixeldrain directory")
		}
		return r.probe()
	}
	return folder, nil
}

func (pixeldrainProvider) Load(raw string) (*remoteFolder, error) {
	if _, _, err := parsePixeldrainDirLink(raw); err == nil {
		return loadPixeldrainDir(raw)
	}
	id, err := parsePixeldrainListLink(raw)
	if err != nil {
		return nil, err
	}
	client := newPixeldrainClient()
	title, files, err := client.list(id)
	if err != nil {
		return nil, err
	}
	folder := &remoteFolder{Name: title, Listing: map[string]fileSig{}, Labels: map[string]string{}}
	probes := map[string]func() (manifestInfo, error){}
	for _, f := range files {
		if f.ID == "" || f.Size <= 0 || !isZipmodName(f.Name) {
			continue
		}
		if err := addPixeldrainFile(client, folder, probes, f); err != nil {
			return nil, err
		}
	}
	folder.Probe = pixeldrainProbeFrom(probes, "list")
	return folder, nil
}

func (pixeldrainProvider) FileID(raw string) (string, error) { return parsePixeldrainFileLink(raw) }

func (pixeldrainProvider) FileURL(raw string) (string, error) {
	id, err := parsePixeldrainFileLink(raw)
	if err != nil {
		return "", err
	}
	return pixeldrainFileURL(id), nil
}

// LoadFile looks at one public file link. The result lists the file only if its name says
// it is a mod archive; the folder name is the file's name.
func (pixeldrainProvider) LoadFile(raw string) (*remoteFolder, error) {
	id, err := parsePixeldrainFileLink(raw)
	if err != nil {
		return nil, err
	}
	client := newPixeldrainClient()
	f, err := client.fileInfo(id)
	if err != nil {
		return nil, err
	}
	u := pixeldrainFileURL(id)
	folder := &remoteFolder{Name: f.Name, Listing: map[string]fileSig{}, Labels: map[string]string{u: f.Name}}
	if !isZipmodName(f.Name) || f.Size <= 0 {
		return folder, nil
	}
	probes := map[string]func() (manifestInfo, error){}
	if err := addPixeldrainFile(client, folder, probes, f); err != nil {
		return nil, err
	}
	folder.Probe = pixeldrainProbeFrom(probes, "file")
	return folder, nil
}

// RunPixeldrain indexes the Pixeldrain lists and files listed in config/pixeldrain.json and
// returns how many GUIDs are new and how many items failed. args are command-line style
// flags (nil = defaults).
func RunPixeldrain(args []string) (newGuids, failures int) {
	return runHosted(pixeldrainProvider{}, args)
}
