package scrapers

// GUID index for zipmods hosted on Dropbox shared links.
//
// Dropbox serves files unencrypted and honours Range requests on its download endpoints, so
// this is the same trick as for Google Drive, over the official Dropbox API (v2) with
// "shared link" arguments, which work for any public link, not only the account's own:
//   - sharing/get_shared_link_metadata gives name, size and date of a link (folder or file);
//   - files/list_folder with a shared_link lists the folder behind a link, one directory per
//     request (sub-directories are walked here), up to 2000 entries per page;
//   - sharing/get_shared_link_file with a Range header returns just those bytes of a file, so
//     per new or changed file only the zip's tail and manifest.xml are read (a few KB), never
//     the whole file.
//
// Unlike Mega, Drive and Pixeldrain, the Dropbox API refuses anonymous requests, so this needs
// a (free) Dropbox app (https://www.dropbox.com/developers/apps, scoped access, no scopes
// needed beyond the defaults). Credentials come from environment variables (GitHub Actions
// secrets) and are sent in request headers only, never in URLs, logs or indices:
//   - DROPBOX_REFRESH_TOKEN + DROPBOX_APP_KEY + DROPBOX_APP_SECRET (recommended: a refresh
//     token does not expire; short-lived access tokens are fetched from it), or
//   - DROPBOX_ACCESS_TOKEN (simplest, but tokens created in the app console expire after
//     about four hours, so it is only good for trying things out).
//
// config/dropbox.json is the usual grouped config (see hostedconfig.go) of folder links
// (https://www.dropbox.com/scl/fo/<id>/<key>?rlkey=..., or the older /sh/<id>/<key>) and
// file links (https://www.dropbox.com/scl/fi/<id>/<name>?rlkey=..., or the older
// /s/<id>/<name>). Output per folder, named after the folder: data/dropbox/<name>.json and
// cache/dropbox/<name>.json (see hosted.go); all file links are indexed together in
// "(single files)". A folder link is walked recursively. Only the rlkey of a link is kept
// (dl=, st= and the like are dropped).
//
// URLs in the index: a file link stays as it is (canonical). A file inside a folder has no
// link of its own, so it is addressed through its folder's link: the older /sh/ links get the
// path appended (.../sh/<id>/<key>/<dir>/<file>); the newer /scl/fo/ links get the directory
// appended and the file in a preview parameter (.../scl/fo/<id>/<key>/<dir>?preview=<file>&rlkey=...).
//
// Only .zipmod and .zip files are looked at. A .zip that is an archive of many mods (rather
// than one mod with manifest.xml at its root) is recorded as not indexable.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	dropboxKind         = "dropbox"
	dropboxUserAgent    = "kk-mod-scraper-dropbox-index-builder/1.0"
	dropboxMaxDepth     = 12
	dropboxMaxRetryWait = 20 * time.Second
)

var (
	dropboxAPIBase     = "https://api.dropboxapi.com/2"         // variables so tests can point them at a fake
	dropboxContentBase = "https://content.dropboxapi.com/2"
	dropboxTokenURL    = "https://api.dropboxapi.com/oauth2/token"
	dropboxHTTPClient  = &http.Client{Timeout: 60 * time.Second}
)

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

var (
	// /sh/<id>/<key> and /scl/fo/<id>/<key>, optionally followed by a path inside the folder.
	dropboxFolderRe = regexp.MustCompile(
		`(?i)^https?://(?:www\.)?dropbox\.com/((?:sh/[A-Za-z0-9]+/[A-Za-z0-9_-]+|scl/fo/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+))((?:/[^?#]*)?)(?:\?([^#]*))?(?:#.*)?$`)
	// /s/<id>/<name> and /scl/fi/<id>/<name>
	dropboxFileRe = regexp.MustCompile(
		`(?i)^https?://(?:www\.)?dropbox\.com/((?:s/[A-Za-z0-9]+|scl/fi/[A-Za-z0-9_-]+)/[^/?#]+)/?(?:\?([^#]*))?(?:#.*)?$`)

	// Used to pull links out of free text (a post).
	dropboxFindFolderRe = regexp.MustCompile(
		`(?i)https?://(?:www\.)?dropbox\.com/(?:sh/[A-Za-z0-9]+/[A-Za-z0-9_-]+|scl/fo/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+)(?:/[^\s"'<>()\[\]?#]*)?(?:\?[^\s"'<>()\[\]#]*)?`)
	dropboxFindFileRe = regexp.MustCompile(
		`(?i)https?://(?:www\.)?dropbox\.com/(?:s/[A-Za-z0-9]+|scl/fi/[A-Za-z0-9_-]+)/[^\s"'<>()\[\]?#/]+/?(?:\?[^\s"'<>()\[\]#]*)?`)
)

// dropboxRlkey picks the rlkey out of a query string ("" if there is none).
func dropboxRlkey(query string) string {
	q, _ := url.ParseQuery(query)
	return q.Get("rlkey")
}

func dropboxRlkeyQuery(rlkey string) string {
	if rlkey == "" {
		return ""
	}
	return "?rlkey=" + url.QueryEscape(rlkey)
}

// parseDropboxFolderLink returns the link's path ("scl/fo/<id>/<key>"), the path inside the
// folder (decoded, "" for the root, no trailing slash) and the rlkey.
func parseDropboxFolderLink(s string) (base, sub, rlkey string, err error) {
	m := dropboxFolderRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", "", "", fmt.Errorf("not a Dropbox folder link: %q (expected https://www.dropbox.com/scl/fo/<id>/<key>?rlkey=...)", s)
	}
	return m[1], cleanPixeldrainSubpath(m[2]), dropboxRlkey(m[3]), nil
}

// parseDropboxFileLink returns the link's path ("scl/fi/<id>/<name>") and the rlkey.
func parseDropboxFileLink(s string) (path, rlkey string, err error) {
	m := dropboxFileRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", "", fmt.Errorf("not a Dropbox file link: %q (expected https://www.dropbox.com/scl/fi/<id>/<name>?rlkey=...)", s)
	}
	return m[1], dropboxRlkey(m[2]), nil
}

// dropboxFolderURL is the canonical link of a folder (or of a directory inside it).
func dropboxFolderURL(base, sub, rlkey string) string {
	u := "https://www.dropbox.com/" + base
	if sub != "" {
		u += pixeldrainEscapePath(sub)
	}
	return u + dropboxRlkeyQuery(rlkey)
}

func dropboxFileURL(path, rlkey string) string {
	return "https://www.dropbox.com/" + path + dropboxRlkeyQuery(rlkey)
}

// dropboxItemURL is the URL under which a file inside a folder appears in the index; full is
// the file's path inside the folder ("/dir/file.zipmod").
func dropboxItemURL(base, rlkey, full string) string {
	if strings.HasPrefix(strings.ToLower(base), "sh/") {
		return "https://www.dropbox.com/" + base + pixeldrainEscapePath(full) // old links address files by path
	}
	i := strings.LastIndex(full, "/")
	dir, name := full[:i], full[i+1:]
	q := url.Values{"preview": {name}}
	if rlkey != "" {
		q.Set("rlkey", rlkey)
	}
	u := "https://www.dropbox.com/" + base
	if dir != "" {
		u += pixeldrainEscapePath(dir)
	}
	return u + "?" + q.Encode()
}

// findDropboxLinks returns the Dropbox folder and file links of a text, canonical, in
// order of appearance (folders first, then files; the order between kinds doesn't matter).
func findDropboxLinks(text string) []string {
	text = html.UnescapeString(text)
	trim := func(s string) string { return strings.TrimRight(s, ".,;:!") }
	var out []string
	for _, m := range dropboxFindFolderRe.FindAllString(text, -1) {
		if base, sub, rlkey, err := parseDropboxFolderLink(trim(m)); err == nil {
			out = append(out, dropboxFolderURL(base, sub, rlkey))
		}
	}
	for _, m := range dropboxFindFileRe.FindAllString(text, -1) {
		if path, rlkey, err := parseDropboxFileLink(trim(m)); err == nil {
			out = append(out, dropboxFileURL(path, rlkey))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

// dropboxError is an error answer from the Dropbox API.
type dropboxError struct {
	Status     int
	Summary    string // machine readable, like "shared_link_not_found/..."
	Message    string
	RetryAfter time.Duration
}

func (e *dropboxError) Error() string {
	if e.Summary != "" {
		return fmt.Sprintf("Dropbox API error %d (%s): %s", e.Status, e.Summary, e.Message)
	}
	return fmt.Sprintf("Dropbox API error %d: %s", e.Status, e.Message)
}

// permanent reports whether the link or file is gone (a 409 saying "not found"). Everything
// else (a 401 for a bad token, rate limits, outages, access denied) is retried on the next
// run and never purges a link.
func (e *dropboxError) permanent() bool {
	return e.Status == http.StatusConflict && strings.Contains(e.Summary, "not_found")
}

func (e *dropboxError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func parseDropboxError(status int, body []byte, retryAfter string) *dropboxError {
	e := &dropboxError{Status: status, Message: http.StatusText(status)}
	var doc struct {
		ErrorSummary string `json:"error_summary"`
	}
	if json.Unmarshal(body, &doc) == nil && doc.ErrorSummary != "" {
		e.Summary = doc.ErrorSummary
	} else if t := strings.TrimSpace(string(body)); t != "" {
		if len(t) > 200 {
			t = t[:200]
		}
		e.Message = t
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs > 0 {
		e.RetryAfter = time.Duration(secs) * time.Second
	}
	return e
}

type dropboxClient struct {
	mu                         sync.Mutex
	token                      string
	refresh, appKey, appSecret string
}

// newDropboxClient reads the credentials from the environment (see the top of this file).
func newDropboxClient() (*dropboxClient, error) {
	env := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	c := &dropboxClient{
		token: env("DROPBOX_ACCESS_TOKEN"), refresh: env("DROPBOX_REFRESH_TOKEN"),
		appKey: env("DROPBOX_APP_KEY"), appSecret: env("DROPBOX_APP_SECRET"),
	}
	switch {
	case c.refresh != "":
		if c.appKey == "" || c.appSecret == "" {
			return nil, errors.New("DROPBOX_REFRESH_TOKEN needs DROPBOX_APP_KEY and DROPBOX_APP_SECRET as well (see the README, \"Dropbox\")")
		}
		if err := c.refreshToken(); err != nil {
			return nil, err
		}
	case c.token == "":
		return nil, errors.New("no Dropbox credentials: set DROPBOX_REFRESH_TOKEN, DROPBOX_APP_KEY and DROPBOX_APP_SECRET, or DROPBOX_ACCESS_TOKEN (see the README, \"Dropbox\")")
	}
	return c, nil
}

func (c *dropboxClient) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// refreshToken exchanges the refresh token for a fresh access token.
func (c *dropboxClient) refreshToken() error {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.refresh}}
	req, err := http.NewRequest(http.MethodPost, dropboxTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", dropboxUserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.appKey, c.appSecret)
	res, err := dropboxHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var doc struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode < 200 || res.StatusCode > 299 || json.Unmarshal(body, &doc) != nil || doc.AccessToken == "" {
		return fmt.Errorf("could not get a Dropbox access token from the refresh token (HTTP %d); check DROPBOX_REFRESH_TOKEN, DROPBOX_APP_KEY and DROPBOX_APP_SECRET", res.StatusCode)
	}
	c.mu.Lock()
	c.token = doc.AccessToken
	c.mu.Unlock()
	return nil
}

// do sends one POST. 2xx responses are returned (the caller closes the body); anything else
// becomes a *dropboxError. A 401 is answered once with a new access token if a refresh token
// is available.
func (c *dropboxClient) do(rawURL string, headers map[string]string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", dropboxUserAgent)
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Authorization", "Bearer "+c.currentToken()) // a header, never in a URL
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := dropboxHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode >= 200 && res.StatusCode <= 299 {
			return res, nil
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		if res.StatusCode == http.StatusUnauthorized && attempt == 0 && c.refresh != "" {
			if c.refreshToken() == nil {
				continue
			}
		}
		return nil, parseDropboxError(res.StatusCode, b, res.Header.Get("Retry-After"))
	}
}

// dropboxRetry runs fn up to three times, retrying network errors, 429 and 5xx.
func dropboxRetry[T any](fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 0; attempt < 3; attempt++ {
		if v, err = fn(); err == nil {
			return v, nil
		}
		var de *dropboxError
		if errors.As(err, &de) && !de.retryable() {
			return v, err
		}
		if attempt < 2 {
			wait := time.Duration(500*(attempt+1)) * time.Millisecond
			if de != nil && de.RetryAfter > 0 {
				wait = min(de.RetryAfter, dropboxMaxRetryWait)
			}
			time.Sleep(wait)
		}
	}
	return v, err
}

// rpc calls a JSON endpoint of the API.
func (c *dropboxClient) rpc(path string, arg, out any) error {
	body, err := json.Marshal(arg)
	if err != nil {
		return err
	}
	_, err = dropboxRetry(func() (struct{}, error) {
		res, err := c.do(dropboxAPIBase+path, map[string]string{"Content-Type": "application/json"}, body)
		if err != nil {
			return struct{}{}, err
		}
		defer res.Body.Close()
		return struct{}{}, json.NewDecoder(res.Body).Decode(out)
	})
	return err
}

type dropboxEntry struct {
	Tag            string `json:".tag"` // "file" or "folder"
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	ServerModified string `json:"server_modified"`
	ClientModified string `json:"client_modified"`
}

// mtime turns the date of the API (RFC 3339) into the index's format.
func (e dropboxEntry) mtime() string {
	s := e.ServerModified
	if s == "" {
		s = e.ClientModified
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02 15:04")
	}
	return s
}

func (c *dropboxClient) metadata(link string) (dropboxEntry, error) {
	var e dropboxEntry
	err := c.rpc("/sharing/get_shared_link_metadata", map[string]string{"url": link}, &e)
	return e, err
}

type dropboxPage struct {
	Entries []dropboxEntry `json:"entries"`
	Cursor  string         `json:"cursor"`
	HasMore bool           `json:"has_more"`
}

// listDir lists everything directly inside a directory of a shared folder (all pages). dir
// is "" for the folder's root, else "/a/b".
func (c *dropboxClient) listDir(link, dir string) ([]dropboxEntry, error) {
	var all []dropboxEntry
	var page dropboxPage
	err := c.rpc("/files/list_folder", map[string]any{
		"path": dir, "shared_link": map[string]string{"url": link}, "recursive": false, "limit": 2000,
	}, &page)
	if err != nil {
		return nil, err
	}
	for {
		all = append(all, page.Entries...)
		if !page.HasMore {
			return all, nil
		}
		cursor := page.Cursor
		page = dropboxPage{}
		if err := c.rpc("/files/list_folder/continue", map[string]string{"cursor": cursor}, &page); err != nil {
			return nil, err
		}
	}
}

// dropboxItem is a file of a shared folder; path is relative to the folder ("/dir/file").
type dropboxItem struct {
	path  string
	entry dropboxEntry
}

// walk collects the files of a directory and all its sub-directories. Any failure fails the
// whole folder, so a partial listing never replaces a good index.
func (c *dropboxClient) walk(link, dir string, depth int, out *[]dropboxItem) error {
	if depth > dropboxMaxDepth {
		return nil
	}
	entries, err := c.listDir(link, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := dir + "/" + e.Name
		switch e.Tag {
		case "folder":
			if err := c.walk(link, p, depth+1, out); err != nil {
				return err
			}
		case "file":
			*out = append(*out, dropboxItem{path: p, entry: e})
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Range reader
// ---------------------------------------------------------------------------

// dropboxHeaderJSON makes JSON safe for the Dropbox-API-Arg header: HTTP headers can't carry
// non-ASCII text, so Dropbox wants every such character as a \uXXXX escape.
func dropboxHeaderJSON(b []byte) string {
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x7f:
			sb.WriteRune(r)
		case r > 0xFFFF:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&sb, `\u%04x\u%04x`, r1, r2)
		default:
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	return sb.String()
}

// dropboxRanger reads byte ranges of one file of a shared link. It implements rangeReader.
// The size is known from the listing, so no suffix ranges are needed.
type dropboxRanger struct {
	c    *dropboxClient
	arg  string // the Dropbox-API-Arg header
	size int
}

// newDropboxRanger reads the file at path inside the shared folder link (path "" for a link
// to a single file).
func newDropboxRanger(c *dropboxClient, link, path string, size int) *dropboxRanger {
	arg := struct {
		URL  string `json:"url"`
		Path string `json:"path,omitempty"`
	}{link, path}
	b, _ := json.Marshal(arg)
	return &dropboxRanger{c: c, arg: dropboxHeaderJSON(b), size: size}
}

func (d *dropboxRanger) Suffix(length int) (suffixResult, error) {
	length = min(length, d.size)
	start := d.size - length
	data, err := d.Range(start, d.size-1)
	if err != nil {
		return suffixResult{}, err
	}
	return suffixResult{data: data, total: d.size, start: start}, nil
}

func (d *dropboxRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end || end >= d.size {
		return nil, fmt.Errorf("invalid range %d-%d for a file of %d bytes", start, end, d.size)
	}
	want := end - start + 1
	return dropboxRetry(func() ([]byte, error) {
		res, err := d.c.do(dropboxContentBase+"/sharing/get_shared_link_file",
			map[string]string{"Dropbox-API-Arg": d.arg, "Range": fmt.Sprintf("bytes=%d-%d", start, end)}, nil)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusPartialContent && !(res.StatusCode == http.StatusOK && start == 0) {
			return nil, errors.New("Dropbox ignored the Range request; stopped to avoid downloading a whole file")
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, int64(want)+1))
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusOK && len(data) > want {
			data = data[:want] // a full response from the start: keep just what was asked for
		}
		if len(data) != want {
			return nil, fmt.Errorf("Dropbox returned %d bytes for a %d-byte range", len(data), want)
		}
		return data, nil
	})
}

// probe reads the GUID and version of one file.
func (d *dropboxRanger) probe() (manifestInfo, error) {
	if d.size == 0 {
		return manifestInfo{}, permanent("empty file")
	}
	info, err := readManifest(d)
	var de *dropboxError
	if errors.As(err, &de) && de.permanent() {
		return manifestInfo{}, permanent("not readable on Dropbox (%v)", de)
	}
	return info, err
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

type dropboxProvider struct{}

func (dropboxProvider) Kind() string { return dropboxKind }

func (dropboxProvider) ID(raw string) (string, error) {
	base, sub, _, err := parseDropboxFolderLink(raw)
	if err != nil {
		return "", err
	}
	return base + sub, nil
}

func (dropboxProvider) Load(raw string) (*remoteFolder, error) {
	base, sub, rlkey, err := parseDropboxFolderLink(raw)
	if err != nil {
		return nil, err
	}
	client, err := newDropboxClient()
	if err != nil {
		return nil, err
	}
	link := dropboxFolderURL(base, "", rlkey) // what the API is given; sub is a path inside it
	md, err := client.metadata(link)
	if err != nil {
		return nil, err
	}
	if md.Tag != "folder" {
		return nil, errors.New("the link is not a folder")
	}
	name := md.Name
	if sub != "" {
		name = sub[strings.LastIndex(sub, "/")+1:]
	}
	var items []dropboxItem
	if err := client.walk(link, sub, 0, &items); err != nil {
		return nil, err
	}

	folder := &remoteFolder{Name: name, Listing: map[string]fileSig{}, Labels: map[string]string{}}
	rangers := map[string]*dropboxRanger{}
	for _, it := range items {
		if !isZipmodName(it.entry.Name) || it.entry.Size <= 0 {
			continue
		}
		u := dropboxItemURL(base, rlkey, it.path)
		folder.Labels[u] = strings.TrimPrefix(it.path, sub+"/")
		folder.Listing[u] = fileSig{Mtime: it.entry.mtime(), Size: strconv.FormatInt(it.entry.Size, 10)}
		rangers[u] = newDropboxRanger(client, link, it.path, int(it.entry.Size))
	}
	folder.Probe = func(u string) (manifestInfo, error) {
		r, ok := rangers[u]
		if !ok {
			return manifestInfo{}, permanent("file is no longer in the Dropbox folder")
		}
		return r.probe()
	}
	return folder, nil
}

func (dropboxProvider) FileID(raw string) (string, error) {
	path, _, err := parseDropboxFileLink(raw)
	return path, err
}

func (dropboxProvider) FileURL(raw string) (string, error) {
	path, rlkey, err := parseDropboxFileLink(raw)
	if err != nil {
		return "", err
	}
	return dropboxFileURL(path, rlkey), nil
}

// LoadFile looks at one public file link. The result lists the file only if its name says
// it is a mod archive; the folder name is the file's name.
func (dropboxProvider) LoadFile(raw string) (*remoteFolder, error) {
	path, rlkey, err := parseDropboxFileLink(raw)
	if err != nil {
		return nil, err
	}
	client, err := newDropboxClient()
	if err != nil {
		return nil, err
	}
	link := dropboxFileURL(path, rlkey)
	md, err := client.metadata(link)
	if err != nil {
		return nil, err
	}
	folder := &remoteFolder{Name: md.Name, Listing: map[string]fileSig{}, Labels: map[string]string{link: md.Name}}
	if md.Tag != "file" || !isZipmodName(md.Name) || md.Size <= 0 {
		return folder, nil
	}
	folder.Listing[link] = fileSig{Mtime: md.mtime(), Size: strconv.FormatInt(md.Size, 10)}
	r := newDropboxRanger(client, link, "", int(md.Size))
	folder.Probe = func(string) (manifestInfo, error) { return r.probe() }
	return folder, nil
}

// RunDropbox indexes the Dropbox folders and files listed in config/dropbox.json and returns
// how many GUIDs are new and how many items failed. args are command-line style flags (nil =
// defaults).
func RunDropbox(args []string) (newGuids, failures int) {
	return runHosted(dropboxProvider{}, args)
}
