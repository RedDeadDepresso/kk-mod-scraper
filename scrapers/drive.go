package scrapers

// GUID index for zipmods hosted in public Google Drive folders.
//
// Drive, unlike Mega, doesn't encrypt anything, so this is the BetterRepack trick over the
// official Drive API (v3):
//   - files.list on the folder (and its sub-folders) gives every file's id, name, size and
//     modified time, 1000 per request;
//   - files.get?alt=media with a Range header returns just those bytes, so per new or changed
//     file only the zip's tail and manifest.xml are read (a few KB), never the whole file.
//
// It needs a free Google API key (Cloud Console -> enable "Google Drive API" -> Credentials ->
// API key), given in the GOOGLE_API_KEY environment variable (a GitHub Actions secret). The key
// is sent in a request header, so it never appears in a URL, a log line or an index. Folders
// must be shared as "Anyone with the link".
//
// config/drive.json is a JSON array of folder links (https://drive.google.com/drive/folders/<id>).
// Output per folder, named after the folder: indices/drive/<name>.json and
// cache/drive/<name>.json (see hosted.go). The URLs in the index are
// https://drive.google.com/file/d/<file id>/view.
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
	driveKind       = "drive"
	driveUserAgent  = "kk-mod-scraper-drive-index-builder/1.0"
	driveFolderMime = "application/vnd.google-apps.folder"
	driveMaxDepth   = 12
)

var (
	driveAPIBase    = "https://www.googleapis.com/drive/v3" // a variable so tests can point it at a fake
	driveHTTPClient = &http.Client{Timeout: 60 * time.Second}
)

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

var driveFolderRe = regexp.MustCompile(
	`^https?://drive\.google\.com/(?:drive/(?:mobile/|u/\d+/)?folders/|open\?id=|folderview\?id=)([A-Za-z0-9_-]{10,})(?:[/?&#].*)?$`)

func parseDriveFolderLink(s string) (string, error) {
	m := driveFolderRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("not a Google Drive folder link: %q (expected https://drive.google.com/drive/folders/<id>)", s)
	}
	return m[1], nil
}

var driveFileLinkRe = regexp.MustCompile(
	`^https?://drive\.google\.com/(?:file/(?:u/\d+/)?d/|uc\?(?:[^#]*&)?id=)([A-Za-z0-9_-]{10,})(?:[/?&#].*)?$`)

func parseDriveFileLink(s string) (string, error) {
	m := driveFileLinkRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("not a Google Drive file link: %q (expected https://drive.google.com/file/d/<id>/view)", s)
	}
	return m[1], nil
}

func driveFileURL(id string) string { return "https://drive.google.com/file/d/" + id + "/view" }

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

// driveError is an error answer from the Drive API.
type driveError struct {
	Status  int
	Reason  string
	Message string
}

func (e *driveError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("Google Drive API error %d (%s): %s", e.Status, e.Reason, e.Message)
	}
	return fmt.Sprintf("Google Drive API error %d: %s", e.Status, e.Message)
}

// Reasons of a 403 that are about limits rather than about the file: they pass with time.
var driveTransientReasons = map[string]bool{
	"userRateLimitExceeded": true, "rateLimitExceeded": true, "dailyLimitExceeded": true,
	"downloadQuotaExceeded": true, "quotaExceeded": true, "sharingRateLimitExceeded": true,
	"backendError": true,
}

// permanent reports whether the file itself can't be read (gone, or not downloadable), as
// opposed to a limit, a server problem or a bad range, which a later run may get past.
func (e *driveError) permanent() bool {
	switch e.Status {
	case http.StatusNotFound:
		return true
	case http.StatusForbidden:
		return !driveTransientReasons[e.Reason]
	}
	return false
}

func (e *driveError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func parseDriveError(status int, body []byte) *driveError {
	var doc struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	e := &driveError{Status: status, Message: http.StatusText(status)}
	if json.Unmarshal(body, &doc) == nil && doc.Error.Message != "" {
		e.Message = doc.Error.Message
		if len(doc.Error.Errors) > 0 {
			e.Reason = doc.Error.Errors[0].Reason
		} else if len(doc.Error.Details) > 0 {
			e.Reason = doc.Error.Details[0].Reason
		}
	}
	return e
}

type driveClient struct{ key string }

func newDriveClient() (*driveClient, error) {
	key := strings.TrimSpace(os.Getenv("GOOGLE_API_KEY"))
	if key == "" {
		return nil, errors.New("the GOOGLE_API_KEY environment variable is not set (see the README, \"Google Drive\")")
	}
	return &driveClient{key: key}, nil
}

// do sends one request. 2xx responses are returned (the caller closes the body); anything
// else becomes a *driveError.
func (c *driveClient) do(rawURL string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", driveUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-Goog-Api-Key", c.key) // a header, not "?key=", so it can't leak through a URL
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := driveHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		return nil, parseDriveError(res.StatusCode, body)
	}
	return res, nil
}

// driveRetry runs fn up to three times, retrying network errors, 429 and 5xx.
func driveRetry[T any](fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 0; attempt < 3; attempt++ {
		if v, err = fn(); err == nil {
			return v, nil
		}
		var de *driveError
		if errors.As(err, &de) && !de.retryable() {
			return v, err
		}
		if attempt < 2 {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
		}
	}
	return v, err
}

func (c *driveClient) getJSON(path string, params url.Values, out any) error {
	_, err := driveRetry(func() (struct{}, error) {
		res, err := c.do(driveAPIBase+path+"?"+params.Encode(), nil)
		if err != nil {
			return struct{}{}, err
		}
		defer res.Body.Close()
		return struct{}{}, json.NewDecoder(res.Body).Decode(out)
	})
	return err
}

type driveFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         string `json:"size"` // absent for Google Docs and the like
	ModifiedTime string `json:"modifiedTime"`
}

func (c *driveClient) folderName(id string) (string, error) {
	var f driveFile
	err := c.getJSON("/files/"+id, url.Values{"fields": {"id,name,mimeType"}, "supportsAllDrives": {"true"}}, &f)
	if err != nil {
		return "", err
	}
	if f.MimeType != driveFolderMime {
		return "", errors.New("the link is not a folder")
	}
	return f.Name, nil
}

// children lists everything directly inside a folder (all pages).
func (c *driveClient) children(folderID string) ([]driveFile, error) {
	var all []driveFile
	pageToken := ""
	for {
		params := url.Values{
			"q":                         {fmt.Sprintf("'%s' in parents and trashed = false", folderID)},
			"fields":                    {"nextPageToken,files(id,name,mimeType,size,modifiedTime)"},
			"pageSize":                  {"1000"},
			"supportsAllDrives":         {"true"},
			"includeItemsFromAllDrives": {"true"},
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		var page struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveFile `json:"files"`
		}
		if err := c.getJSON("/files", params, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Files...)
		if page.NextPageToken == "" {
			return all, nil
		}
		pageToken = page.NextPageToken
	}
}

// walk collects the files of a folder and all its sub-folders. Any failure fails the whole
// folder, so a partial listing never replaces a good index.
func (c *driveClient) walk(folderID string, depth int, visited map[string]bool, out *[]driveFile) error {
	if visited[folderID] || depth > driveMaxDepth {
		return nil
	}
	visited[folderID] = true
	items, err := c.children(folderID)
	if err != nil {
		return err
	}
	for _, f := range items {
		if f.MimeType == driveFolderMime {
			if err := c.walk(f.ID, depth+1, visited, out); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, f)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Range reader
// ---------------------------------------------------------------------------

// driveRanger reads byte ranges of one Drive file. It implements rangeReader. The size is
// known from the listing, so no suffix ranges are needed.
type driveRanger struct {
	c    *driveClient
	id   string
	size int
}

func (d *driveRanger) mediaURL() string {
	return driveAPIBase + "/files/" + d.id + "?alt=media&supportsAllDrives=true"
}

func (d *driveRanger) Suffix(length int) (suffixResult, error) {
	length = min(length, d.size)
	start := d.size - length
	data, err := d.Range(start, d.size-1)
	if err != nil {
		return suffixResult{}, err
	}
	return suffixResult{data: data, total: d.size, start: start}, nil
}

func (d *driveRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end || end >= d.size {
		return nil, fmt.Errorf("invalid range %d-%d for a file of %d bytes", start, end, d.size)
	}
	want := end - start + 1
	return driveRetry(func() ([]byte, error) {
		res, err := d.c.do(d.mediaURL(), map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end)})
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusPartialContent && !(res.StatusCode == http.StatusOK && start == 0) {
			return nil, errors.New("Google Drive ignored the Range request; stopped to avoid downloading a whole file")
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, int64(want)+1))
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusOK && len(data) > want {
			data = data[:want] // a full response from the start: keep just what was asked for
		}
		if len(data) != want {
			return nil, fmt.Errorf("Google Drive returned %d bytes for a %d-byte range", len(data), want)
		}
		return data, nil
	})
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

type driveProvider struct{}

func (driveProvider) Kind() string { return driveKind }

func (driveProvider) ID(raw string) (string, error) { return parseDriveFolderLink(raw) }

func (driveProvider) Load(raw string) (*remoteFolder, error) {
	id, err := parseDriveFolderLink(raw)
	if err != nil {
		return nil, err
	}
	client, err := newDriveClient()
	if err != nil {
		return nil, err
	}
	name, err := client.folderName(id)
	if err != nil {
		var de *driveError
		if errors.As(err, &de) && de.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%w (folder not found, or not shared as \"Anyone with the link\")", err)
		}
		return nil, err
	}
	var files []driveFile
	if err := client.walk(id, 0, map[string]bool{}, &files); err != nil {
		return nil, err
	}

	byURL := map[string]driveFile{}
	folder := &remoteFolder{Name: name, Listing: map[string]fileSig{}, Labels: map[string]string{}}
	for _, f := range files {
		if !isZipmodName(f.Name) || strings.HasPrefix(f.MimeType, "application/vnd.google-apps.") {
			continue
		}
		size, err := strconv.ParseInt(f.Size, 10, 64)
		if err != nil {
			continue
		}
		mtime := f.ModifiedTime
		if t, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
			mtime = t.UTC().Format("2006-01-02 15:04")
		}
		u := driveFileURL(f.ID)
		byURL[u] = f
		folder.Labels[u] = f.Name
		folder.Listing[u] = fileSig{Mtime: mtime, Size: strconv.FormatInt(size, 10)}
	}

	folder.Probe = func(u string) (manifestInfo, error) {
		f, ok := byURL[u]
		if !ok {
			return manifestInfo{}, permanent("file is no longer in the Drive folder")
		}
		size, _ := strconv.Atoi(f.Size)
		if size == 0 {
			return manifestInfo{}, permanent("empty file")
		}
		info, err := readManifest(&driveRanger{c: client, id: f.ID, size: size})
		var de *driveError
		if errors.As(err, &de) && de.permanent() {
			return manifestInfo{}, permanent("not readable on Google Drive (%v)", de)
		}
		return info, err
	}
	return folder, nil
}

func (driveProvider) FileID(raw string) (string, error) { return parseDriveFileLink(raw) }

func (driveProvider) FileURL(raw string) (string, error) {
	id, err := parseDriveFileLink(raw)
	if err != nil {
		return "", err
	}
	return driveFileURL(id), nil
}

// LoadFile looks at one public file link. The result lists the file only if its name says
// it is a mod archive; the folder name is the file's name.
func (driveProvider) LoadFile(raw string) (*remoteFolder, error) {
	id, err := parseDriveFileLink(raw)
	if err != nil {
		return nil, err
	}
	client, err := newDriveClient()
	if err != nil {
		return nil, err
	}
	var f driveFile
	err = client.getJSON("/files/"+id, url.Values{"fields": {"id,name,mimeType,size,modifiedTime"}, "supportsAllDrives": {"true"}}, &f)
	if err != nil {
		return nil, err
	}
	u := driveFileURL(id)
	folder := &remoteFolder{Name: f.Name, Listing: map[string]fileSig{}, Labels: map[string]string{u: f.Name}}
	size, serr := strconv.ParseInt(f.Size, 10, 64)
	if !isZipmodName(f.Name) || f.MimeType == driveFolderMime || strings.HasPrefix(f.MimeType, "application/vnd.google-apps.") || serr != nil {
		return folder, nil
	}
	mtime := f.ModifiedTime
	if t, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
		mtime = t.UTC().Format("2006-01-02 15:04")
	}
	folder.Listing[u] = fileSig{Mtime: mtime, Size: strconv.FormatInt(size, 10)}
	folder.Probe = func(string) (manifestInfo, error) {
		if size == 0 {
			return manifestInfo{}, permanent("empty file")
		}
		info, err := readManifest(&driveRanger{c: client, id: id, size: int(size)})
		var de *driveError
		if errors.As(err, &de) && de.permanent() {
			return manifestInfo{}, permanent("not readable on Google Drive (%v)", de)
		}
		return info, err
	}
	return folder, nil
}

// RunDrive indexes the Drive folders listed in config/drive.json and returns how many GUIDs
// are new and how many folders failed. args are command-line style flags (nil = defaults).
func RunDrive(args []string) (newGuids, failures int) { return runHosted(driveProvider{}, args) }
