package scrapers

// GUID index for zipmods hosted on MediaFire.
//
// MediaFire's public API (v1.5) answers anonymous calls for public content, and its download
// servers honour Range requests, so this is the same trick as for Pixeldrain and Google Drive:
//   - GET /api/1.5/folder/get_info.php gives the name of a folder, and
//     GET /api/1.5/folder/get_content.php its files and sub-folders (in chunks);
//   - GET /api/1.5/file/get_info.php gives the name, size and upload date of a single file;
//   - a file's page (https://www.mediafire.com/file/<quick key>) holds the link to the actual
//     download (the download button, plain or base64-"scrambled"). That link is resolved once
//     per file, and again if the server refuses it (the links are short-lived);
//   - the download link is read with Range requests, so per new or changed file only the
//     zip's tail and manifest.xml are fetched (a few KB), never the whole file.
//
// No account or API key is needed. MediaFire answers requests that look like a script with a
// captcha page now and then; those files are simply tried again on the next run.
//
// config/mediafire.json is the usual grouped config (see hostedconfig.go) of folder links
// (https://www.mediafire.com/folder/<folder key>[/<name>]) and file links
// (https://www.mediafire.com/file/<quick key>[/<name>[/file]]; /view/, /download/ and
// /file_premium/ links work too). A folder is walked recursively, so a link to a folder
// indexes every .zipmod/.zip in it and below it. Output per folder, named after it:
// data/mediafire/<folder name>.json and cache/mediafire/<folder name>.json (see hosted.go);
// all file links are indexed together in "(single files)". The URLs in the index are
// https://www.mediafire.com/file/<quick key>, which MediaFire redirects to the full page.
//
// Only .zipmod and .zip files are looked at. Password protected files can't be read and are
// skipped. A .zip that is an archive of many mods (rather than one mod with manifest.xml at
// its root) is recorded as not indexable.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	mediafireKind = "mediafire"
	// MediaFire serves its download page (and sometimes its API) to browsers only.
	mediafireUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0 kk-mod-scraper-mediafire-index-builder/1.0"

	mediafireChunkSize = 1000
	mediafireMaxChunks = 100
	mediafireMaxDepth  = 12
	mediafireMaxNodes  = 20000
)

var (
	mediafireAPIBase    = "https://www.mediafire.com/api/1.5" // variables so tests can point them at a fake
	mediafirePageBase   = "https://www.mediafire.com"
	mediafireHTTPClient = &http.Client{Timeout: 60 * time.Second}
)

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

const mediafireHost = `(?:www\.|m\.)?mediafire\.com`

var (
	mediafireFolderRe = regexp.MustCompile(
		`(?i)^https?://` + mediafireHost + `/folder/([A-Za-z0-9]{8,20})(?:[/?#].*)?$`)
	mediafireFileRe = regexp.MustCompile(
		`(?i)^https?://` + mediafireHost + `/(?:file|file_premium|view|download)/([A-Za-z0-9]{8,20})(?:[/?#].*)?$`)

	// Used to pull links out of free text (a post): the \b stops at the end of the key, so a
	// trailing comma or bracket is not part of it, and an over-long token is not a link.
	mediafireFindFolderRe = regexp.MustCompile(
		`(?i)https?://` + mediafireHost + `/folder/([A-Za-z0-9]{8,20})\b`)
	mediafireFindFileRe = regexp.MustCompile(
		`(?i)https?://` + mediafireHost + `/(?:file|file_premium|view|download)/([A-Za-z0-9]{8,20})\b`)
)

func parseMediaFireFolderLink(s string) (string, error) {
	m := mediafireFolderRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("not a MediaFire folder link: %q (expected https://www.mediafire.com/folder/<key>)", s)
	}
	return m[1], nil
}

func parseMediaFireFileLink(s string) (string, error) {
	m := mediafireFileRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("not a MediaFire file link: %q (expected https://www.mediafire.com/file/<key>)", s)
	}
	return m[1], nil
}

func mediafireFolderURL(key string) string { return "https://www.mediafire.com/folder/" + key }
func mediafireFileURL(key string) string   { return "https://www.mediafire.com/file/" + key }

// findMediaFireLinks returns the MediaFire folder and file links of a text, canonical, in
// order of appearance (folders first).
func findMediaFireLinks(text string) []string {
	var out []string
	for _, m := range mediafireFindFolderRe.FindAllStringSubmatch(text, -1) {
		out = append(out, mediafireFolderURL(m[1]))
	}
	for _, m := range mediafireFindFileRe.FindAllStringSubmatch(text, -1) {
		out = append(out, mediafireFileURL(m[1]))
	}
	return out
}

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

// mediafireError is an error answer from MediaFire: an HTTP error status, or an API answer
// with "result": "Error" (then Status is 0 and Code is MediaFire's error number).
type mediafireError struct {
	Status  int
	Code    string
	Message string
}

func (e *mediafireError) Error() string {
	switch {
	case e.Code != "":
		return fmt.Sprintf("MediaFire API error %s: %s", e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("MediaFire error %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("MediaFire error %d", e.Status)
}

// permanent reports whether the folder or file is gone: HTTP 404, or the API saying the key
// is unknown (110 for files, 112 for folders). Everything else (captchas, rate limits,
// outages, private folders...) is retried on the next run and never purges a link.
func (e *mediafireError) permanent() bool {
	if e.Status == http.StatusNotFound {
		return true
	}
	if e.Code == "110" || e.Code == "112" {
		return true
	}
	return strings.Contains(strings.ToLower(e.Message), "unknown or invalid")
}

func (e *mediafireError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// mfNum is a JSON value that MediaFire sends as a string or a number (sizes, error codes).
type mfNum string

func (n *mfNum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" {
		s = ""
	}
	*n = mfNum(s)
	return nil
}

func (n mfNum) int64() int64 {
	v, _ := strconv.ParseInt(string(n), 10, 64)
	return v
}

type mfFile struct {
	QuickKey          string `json:"quickkey"`
	Name              string `json:"filename"`
	Size              mfNum  `json:"size"`
	Created           string `json:"created"`
	PasswordProtected string `json:"password_protected"`
}

type mfFolder struct {
	Key  string `json:"folderkey"`
	Name string `json:"name"`
}

type mfResponse struct {
	Result        string          `json:"result"`
	Error         mfNum           `json:"error"`
	Message       string          `json:"message"`
	MoreChunks    string          `json:"more_chunks"`
	FolderInfo    json.RawMessage `json:"folder_info"`
	FileInfo      json.RawMessage `json:"file_info"`
	FolderContent struct {
		Files   []mfFile   `json:"files"`
		Folders []mfFolder `json:"folders"`
	} `json:"folder_content"`
}

// mediafireDo sends one GET request. 2xx responses are returned (the caller closes the
// body); anything else becomes a *mediafireError.
func mediafireDo(rawURL string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", mediafireUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := mediafireHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		res.Body.Close()
		return nil, &mediafireError{Status: res.StatusCode, Message: http.StatusText(res.StatusCode)}
	}
	return res, nil
}

// mediafireRetry runs fn up to three times, retrying network errors, 429 and 5xx.
func mediafireRetry[T any](fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 0; attempt < 3; attempt++ {
		if v, err = fn(); err == nil {
			return v, nil
		}
		var me *mediafireError
		if errors.As(err, &me) && !me.retryable() {
			return v, err
		}
		if attempt < 2 {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
		}
	}
	return v, err
}

// mediafireAPI calls one API method and returns its response, or a *mediafireError.
func mediafireAPI(method string, q url.Values) (*mfResponse, error) {
	q.Set("response_format", "json")
	return mediafireRetry(func() (*mfResponse, error) {
		res, err := mediafireDo(mediafireAPIBase+"/"+method+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		var doc struct {
			Response mfResponse `json:"response"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&doc); err != nil {
			return nil, fmt.Errorf("unreadable MediaFire API answer: %w", err)
		}
		if doc.Response.Result != "Success" {
			return nil, &mediafireError{Code: string(doc.Response.Error), Message: doc.Response.Message}
		}
		return &doc.Response, nil
	})
}

func mediafireFolderName(key string) (string, error) {
	r, err := mediafireAPI("folder/get_info.php", url.Values{"folder_key": {key}})
	if err != nil {
		return "", err
	}
	var info mfFolder
	if len(r.FolderInfo) > 0 && r.FolderInfo[0] == '{' {
		if err := json.Unmarshal(r.FolderInfo, &info); err != nil {
			return "", err
		}
	}
	return info.Name, nil
}

// mediafireContent lists all files or all sub-folders of a folder, chunk by chunk.
func mediafireContent(key, contentType string) (files []mfFile, folders []mfFolder, err error) {
	for chunk := 1; chunk <= mediafireMaxChunks; chunk++ {
		r, err := mediafireAPI("folder/get_content.php", url.Values{
			"folder_key":   {key},
			"content_type": {contentType},
			"chunk":        {strconv.Itoa(chunk)},
			"chunk_size":   {strconv.Itoa(mediafireChunkSize)},
		})
		if err != nil {
			return nil, nil, err
		}
		files = append(files, r.FolderContent.Files...)
		folders = append(folders, r.FolderContent.Folders...)
		if !strings.EqualFold(r.MoreChunks, "yes") {
			return files, folders, nil
		}
	}
	return nil, nil, fmt.Errorf("more than %d chunks in MediaFire folder %s; stopped", mediafireMaxChunks, key)
}

func mediafireFileInfo(qk string) (mfFile, error) {
	r, err := mediafireAPI("file/get_info.php", url.Values{"quick_key": {qk}})
	if err != nil {
		return mfFile{}, err
	}
	var f mfFile
	if len(r.FileInfo) > 0 && r.FileInfo[0] == '{' {
		if err := json.Unmarshal(r.FileInfo, &f); err != nil {
			return mfFile{}, err
		}
	}
	if f.QuickKey == "" {
		f.QuickKey = qk
	}
	return f, nil
}

// mediafireWalk lists all zip files below a folder, recursively. Names are relative to the
// starting folder, and keyed by quick key.
func mediafireWalk(rootKey string) (files []mfFile, names map[string]string, err error) {
	names = map[string]string{}
	visited := 0
	var rec func(key, rel string, depth int) error
	rec = func(key, rel string, depth int) error {
		fs, _, err := mediafireContent(key, "files")
		if err != nil {
			return fmt.Errorf("listing %s: %w", key, err)
		}
		for _, f := range fs {
			if visited++; visited > mediafireMaxNodes {
				return fmt.Errorf("more than %d entries in the folder; stopped", mediafireMaxNodes)
			}
			if f.QuickKey == "" || f.Size.int64() <= 0 || !isZipmodName(f.Name) || strings.EqualFold(f.PasswordProtected, "yes") {
				continue
			}
			files = append(files, f)
			names[f.QuickKey] = rel + f.Name
		}
		if depth >= mediafireMaxDepth {
			return nil
		}
		_, subs, err := mediafireContent(key, "folders")
		if err != nil {
			return fmt.Errorf("listing the folders of %s: %w", key, err)
		}
		for _, s := range subs {
			if visited++; visited > mediafireMaxNodes {
				return fmt.Errorf("more than %d entries in the folder; stopped", mediafireMaxNodes)
			}
			if s.Key == "" {
				continue
			}
			if err := rec(s.Key, rel+s.Name+"/", depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	err = rec(rootKey, "", 0)
	return files, names, err
}

// mediafireMtime turns the upload date of the API ("2024-02-15 16:30:51") into the index's format.
func mediafireMtime(s string) string {
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.Format("2006-01-02 15:04")
	}
	return s
}

// ---------------------------------------------------------------------------
// Download links
// ---------------------------------------------------------------------------

var mediafireDirectRe = regexp.MustCompile(`https?://download[0-9a-z-]*\.mediafire\.com/[^\s"'<>\\]+`)

// parseMediaFireDownloadPage finds the download link on a file page. A page that says the
// file is gone is reported as a *mediafireError that is permanent().
func parseMediaFireDownloadPage(body []byte) (string, error) {
	if doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body)); err == nil {
		if btn := doc.Find("a#downloadButton").First(); btn.Length() > 0 {
			if scrambled, ok := btn.Attr("data-scrambled-url"); ok && scrambled != "" {
				if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(scrambled)); err == nil && strings.HasPrefix(string(dec), "http") {
					return string(dec), nil
				}
			}
			if href, ok := btn.Attr("href"); ok && strings.HasPrefix(href, "http") {
				return href, nil
			}
		}
	}
	if m := mediafireDirectRe.Find(body); m != nil {
		return string(m), nil
	}
	lower := strings.ToLower(string(body))
	for _, gone := range []string{"invalid or deleted file", "the key you provided for file download was invalid", "file has been deleted"} {
		if strings.Contains(lower, gone) {
			return "", &mediafireError{Status: http.StatusNotFound, Message: "the file was deleted"}
		}
	}
	return "", errors.New("no download link on the MediaFire page (password protected, blocked, or the page changed?)")
}

// mediafireDownloadLink resolves the current download link of a file.
func mediafireDownloadLink(qk string) (string, error) {
	return mediafireRetry(func() (string, error) {
		res, err := mediafireDo(mediafirePageBase+"/file/"+qk, nil)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		if err != nil {
			return "", err
		}
		return parseMediaFireDownloadPage(body)
	})
}

// ---------------------------------------------------------------------------
// Range reader
// ---------------------------------------------------------------------------

// mediafireRanger reads byte ranges of one file. It implements rangeReader. The size is
// known from the listing, so no suffix ranges are needed.
type mediafireRanger struct {
	qk   string
	size int

	mu     sync.Mutex
	direct string // the download link, resolved on first use
}

func newMediafireRanger(qk string, size int) *mediafireRanger {
	return &mediafireRanger{qk: qk, size: size}
}

func (r *mediafireRanger) link(refresh bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.direct == "" || refresh {
		l, err := mediafireDownloadLink(r.qk)
		if err != nil {
			return "", err
		}
		r.direct = l
	}
	return r.direct, nil
}

func (r *mediafireRanger) Suffix(length int) (suffixResult, error) {
	length = min(length, r.size)
	start := r.size - length
	data, err := r.Range(start, r.size-1)
	if err != nil {
		return suffixResult{}, err
	}
	return suffixResult{data: data, total: r.size, start: start}, nil
}

func (r *mediafireRanger) fetch(link string, start, end int) ([]byte, error) {
	want := end - start + 1
	return mediafireRetry(func() ([]byte, error) {
		res, err := mediafireDo(link, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end)})
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusPartialContent && !(res.StatusCode == http.StatusOK && start == 0) {
			return nil, errors.New("MediaFire ignored the Range request; stopped to avoid downloading a whole file")
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, int64(want)+1))
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusOK && len(data) > want {
			data = data[:want] // a full response from the start: keep just what was asked for
		}
		if len(data) != want {
			return nil, fmt.Errorf("MediaFire returned %d bytes for a %d-byte range", len(data), want)
		}
		return data, nil
	})
}

func (r *mediafireRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end || end >= r.size {
		return nil, fmt.Errorf("invalid range %d-%d for a file of %d bytes", start, end, r.size)
	}
	refresh := false
	for attempt := 0; ; attempt++ {
		link, err := r.link(refresh)
		if err != nil {
			return nil, err // the file page itself failed; a 404 here means the file is gone
		}
		data, err := r.fetch(link, start, end)
		if err == nil {
			return data, nil
		}
		var me *mediafireError
		if errors.As(err, &me) {
			if attempt == 0 && (me.Status == http.StatusUnauthorized || me.Status == http.StatusForbidden ||
				me.Status == http.StatusNotFound || me.Status == http.StatusGone) {
				refresh = true // the download link expired: get a fresh one and try once more
				continue
			}
			// Not %w: an error of the download server is not proof that the file is gone.
			return nil, fmt.Errorf("MediaFire download failed: %v", me)
		}
		return nil, err
	}
}

// probe reads the GUID and version of one file.
func (r *mediafireRanger) probe() (manifestInfo, error) {
	if r.size == 0 {
		return manifestInfo{}, permanent("empty file")
	}
	info, err := readManifest(r)
	var me *mediafireError
	if errors.As(err, &me) && me.permanent() {
		return manifestInfo{}, permanent("not readable on MediaFire (%v)", me)
	}
	return info, err
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

type mediafireProvider struct{}

func (mediafireProvider) Kind() string { return mediafireKind }

func (mediafireProvider) ID(raw string) (string, error) { return parseMediaFireFolderLink(raw) }

func (mediafireProvider) Load(raw string) (*remoteFolder, error) {
	key, err := parseMediaFireFolderLink(raw)
	if err != nil {
		return nil, err
	}
	name, err := mediafireFolderName(key)
	if err != nil {
		return nil, err
	}
	files, names, err := mediafireWalk(key)
	if err != nil {
		return nil, err
	}
	folder := &remoteFolder{Name: name, Listing: map[string]fileSig{}, Labels: map[string]string{}}
	rangers := map[string]*mediafireRanger{}
	for _, f := range files {
		u := mediafireFileURL(f.QuickKey)
		folder.Labels[u] = names[f.QuickKey]
		folder.Listing[u] = fileSig{Mtime: mediafireMtime(f.Created), Size: strconv.FormatInt(f.Size.int64(), 10)}
		rangers[u] = newMediafireRanger(f.QuickKey, int(f.Size.int64()))
	}
	folder.Probe = func(u string) (manifestInfo, error) {
		r, ok := rangers[u]
		if !ok {
			return manifestInfo{}, permanent("file is no longer in the MediaFire folder")
		}
		return r.probe()
	}
	return folder, nil
}

func (mediafireProvider) FileID(raw string) (string, error) { return parseMediaFireFileLink(raw) }

func (mediafireProvider) FileURL(raw string) (string, error) {
	qk, err := parseMediaFireFileLink(raw)
	if err != nil {
		return "", err
	}
	return mediafireFileURL(qk), nil
}

// LoadFile looks at one public file link. The result lists the file only if its name says
// it is a mod archive; the folder name is the file's name.
func (mediafireProvider) LoadFile(raw string) (*remoteFolder, error) {
	qk, err := parseMediaFireFileLink(raw)
	if err != nil {
		return nil, err
	}
	f, err := mediafireFileInfo(qk)
	if err != nil {
		return nil, err
	}
	u := mediafireFileURL(qk)
	folder := &remoteFolder{Name: f.Name, Listing: map[string]fileSig{}, Labels: map[string]string{u: f.Name}}
	if !isZipmodName(f.Name) || f.Size.int64() <= 0 || strings.EqualFold(f.PasswordProtected, "yes") {
		return folder, nil
	}
	folder.Listing[u] = fileSig{Mtime: mediafireMtime(f.Created), Size: strconv.FormatInt(f.Size.int64(), 10)}
	r := newMediafireRanger(qk, int(f.Size.int64()))
	folder.Probe = func(string) (manifestInfo, error) { return r.probe() }
	return folder, nil
}

// RunMediaFire indexes the MediaFire folders and files listed in config/mediafire.json and
// returns how many GUIDs are new and how many items failed. args are command-line style
// flags (nil = defaults).
func RunMediaFire(args []string) (newGuids, failures int) {
	return runHosted(mediafireProvider{}, args)
}
