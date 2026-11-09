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
// Only .zipmod and .zip files are looked at. A .zip that is an archive of many mods (rather
// than one mod with manifest.xml at its root) is recorded as not indexable.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
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
	rangers := map[string]*pixeldrainRanger{}
	for _, f := range files {
		if f.ID == "" || f.Size <= 0 || !isZipmodName(f.Name) {
			continue
		}
		u := pixeldrainFileURL(f.ID)
		folder.Labels[u] = f.Name
		folder.Listing[u] = fileSig{Mtime: pixeldrainMtime(f.DateUpload), Size: strconv.FormatInt(f.Size, 10)}
		rangers[u] = pixeldrainFileRanger(client, f.ID, int(f.Size))
	}
	folder.Probe = func(u string) (manifestInfo, error) {
		r, ok := rangers[u]
		if !ok {
			return manifestInfo{}, permanent("file is no longer in the Pixeldrain list")
		}
		return r.probe()
	}
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
	folder.Listing[u] = fileSig{Mtime: pixeldrainMtime(f.DateUpload), Size: strconv.FormatInt(f.Size, 10)}
	r := pixeldrainFileRanger(client, id, int(f.Size))
	folder.Probe = func(string) (manifestInfo, error) { return r.probe() }
	return folder, nil
}

// RunPixeldrain indexes the Pixeldrain lists and files listed in config/pixeldrain.json and
// returns how many GUIDs are new and how many items failed. args are command-line style
// flags (nil = defaults).
func RunPixeldrain(args []string) (newGuids, failures int) {
	return runHosted(pixeldrainProvider{}, args)
}
