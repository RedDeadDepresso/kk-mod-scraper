package scrapers

// GUID index for zipmods hosted on Catbox (catbox.moe).
//
// Catbox has no account-free API for listing anything, but it serves files unencrypted from
// files.catbox.moe and honours Range requests, so reading a zip's manifest is the usual
// trick: per new file only the zip's tail and manifest.xml are fetched (a few KB).
//
// There are two kinds of links:
//   - files: https://files.catbox.moe/<id>.<ext>. The file name is the random id plus the
//     extension, and the id never changes its content, so a file is read once and then
//     remembered. One cheap ranged request gives its size and upload date;
//   - albums: https://catbox.moe/c/<id>. An album is a web page, so it is read like one: the
//     files.catbox.moe links in the page are the album's files. Only the page is fetched to
//     list an album; the files themselves are only read when they are new. The folder name is
//     the album's title (or its id if the title is missing or just says "Catbox").
//
// config/catbox.json is the usual grouped config (see hostedconfig.go) of album and file
// links. Output per album, named after it: data/catbox/<album title>.json and
// cache/catbox/<album title>.json (see hosted.go); all file links are indexed together in
// "(single files)". The URLs in the index are https://files.catbox.moe/<id>.<ext>.
//
// Only .zipmod and .zip files are looked at (a link whose extension says otherwise is not a
// mod; forum posts are full of Catbox images, so the forum scraper doesn't even collect
// those). A .zip that is an archive of many mods (rather than one mod with manifest.xml at
// its root) is recorded as not indexable.
//
// Not covered: Litterbox (litterbox.catbox.moe), whose files expire after at most three days.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	catboxKind      = "catbox"
	catboxUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0 kk-mod-scraper-catbox-index-builder/1.0"
)

var (
	catboxSiteBase   = "https://catbox.moe" // variables so tests can point them at a fake
	catboxFilesBase  = "https://files.catbox.moe"
	catboxHTTPClient = &http.Client{Timeout: 60 * time.Second}
)

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

const catboxName = `[A-Za-z0-9_-]{4,24}\.[A-Za-z0-9]{1,10}`

var (
	catboxAlbumRe = regexp.MustCompile(`(?i)^https?://(?:www\.)?catbox\.moe/c/([A-Za-z0-9]{4,12})(?:[/?#].*)?$`)
	catboxFileRe  = regexp.MustCompile(`(?i)^https?://files\.catbox\.moe/(` + catboxName + `)(?:[?#].*)?$`)

	// Used to pull links out of free text (a post). Files are only collected if they are zips.
	catboxFindAlbumRe = regexp.MustCompile(`(?i)https?://(?:www\.)?catbox\.moe/c/([A-Za-z0-9]{4,12})\b`)
	catboxFindFileRe  = regexp.MustCompile(`(?i)https?://files\.catbox\.moe/([A-Za-z0-9_-]{4,24}\.(?:zipmod|zip))\b`)

	// The files of an album page: absolute or protocol-relative links, in a href or anywhere else.
	catboxPageFileRe = regexp.MustCompile(`(?i)(?:https?:)?//files\.catbox\.moe/(` + catboxName + `)`)
)

func parseCatboxAlbumLink(s string) (string, error) {
	m := catboxAlbumRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("not a Catbox album link: %q (expected https://catbox.moe/c/<id>)", s)
	}
	return m[1], nil
}

func parseCatboxFileLink(s string) (string, error) {
	m := catboxFileRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("not a Catbox file link: %q (expected https://files.catbox.moe/<id>.<ext>)", s)
	}
	return m[1], nil
}

func catboxAlbumURL(id string) string   { return "https://catbox.moe/c/" + id }
func catboxFileURL(name string) string  { return "https://files.catbox.moe/" + name }
func catboxFetchURL(name string) string { return catboxFilesBase + "/" + name }
func catboxAlbumFetch(id string) string { return catboxSiteBase + "/c/" + id }
func catboxIsZipName(name string) bool  { return isZipmodName(name) }

// findCatboxLinks returns the Catbox album links and zip file links of a text, canonical, in
// order of appearance (albums first).
func findCatboxLinks(text string) []string {
	var out []string
	for _, m := range catboxFindAlbumRe.FindAllStringSubmatch(text, -1) {
		out = append(out, catboxAlbumURL(m[1]))
	}
	for _, m := range catboxFindFileRe.FindAllStringSubmatch(text, -1) {
		out = append(out, catboxFileURL(m[1]))
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// catboxError is an HTTP error answer from Catbox.
type catboxError struct{ Status int }

func (e *catboxError) Error() string {
	return fmt.Sprintf("Catbox error %d: %s", e.Status, http.StatusText(e.Status))
}

// permanent reports whether the file or album is gone (404/410). Everything else is retried
// on the next run and never purges a link.
func (e *catboxError) permanent() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusGone
}

func (e *catboxError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// catboxDo sends one GET request. 2xx responses are returned (the caller closes the body);
// anything else becomes a *catboxError.
func catboxDo(rawURL string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", catboxUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := catboxHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		res.Body.Close()
		return nil, &catboxError{Status: res.StatusCode}
	}
	return res, nil
}

// catboxRetry runs fn up to three times, retrying network errors, 429 and 5xx.
func catboxRetry[T any](fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 0; attempt < 3; attempt++ {
		if v, err = fn(); err == nil {
			return v, nil
		}
		var ce *catboxError
		if errors.As(err, &ce) && !ce.retryable() {
			return v, err
		}
		if attempt < 2 {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
		}
	}
	return v, err
}

// catboxRanger reads byte ranges of one file. It implements rangeReader. The size isn't
// known beforehand (album pages don't give it), so Suffix learns it from Content-Range.
type catboxRanger struct{ name string }

func (r catboxRanger) Suffix(length int) (suffixResult, error) {
	return catboxRetry(func() (suffixResult, error) {
		res, err := catboxDo(catboxFetchURL(r.name), map[string]string{"Range": fmt.Sprintf("bytes=-%d", length)})
		if err != nil {
			return suffixResult{}, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusPartialContent {
			return suffixResult{}, errors.New("Catbox ignored the tail Range request; stopped to avoid downloading a whole file")
		}
		m := contentRangeRe.FindStringSubmatch(strings.TrimSpace(res.Header.Get("Content-Range")))
		if m == nil {
			return suffixResult{}, errors.New("Catbox returned an invalid Content-Range")
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

func (r catboxRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end {
		return nil, fmt.Errorf("invalid range %d-%d", start, end)
	}
	want := end - start + 1
	return catboxRetry(func() ([]byte, error) {
		res, err := catboxDo(catboxFetchURL(r.name), map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end)})
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusPartialContent && !(res.StatusCode == http.StatusOK && start == 0) {
			return nil, errors.New("Catbox ignored the Range request; stopped to avoid downloading a whole file")
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, int64(want)+1))
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusOK && len(data) > want {
			data = data[:want] // a full response from the start: keep just what was asked for
		}
		if len(data) != want {
			return nil, fmt.Errorf("Catbox returned %d bytes for a %d-byte range", len(data), want)
		}
		return data, nil
	})
}

// probe reads the GUID and version of one file.
func (r catboxRanger) probe() (manifestInfo, error) {
	info, err := readManifest(r)
	var ce *catboxError
	if errors.As(err, &ce) && ce.permanent() {
		return manifestInfo{}, permanent("no longer on Catbox (%v)", ce)
	}
	return info, err
}

// catboxFileSig learns the size and upload date of a file from a one-byte ranged request.
func catboxFileSig(name string) (fileSig, error) {
	return catboxRetry(func() (fileSig, error) {
		res, err := catboxDo(catboxFetchURL(name), map[string]string{"Range": "bytes=0-0"})
		if err != nil {
			return fileSig{}, err
		}
		defer res.Body.Close() // not read: a server that ignores Range must not make us download the file
		size := res.ContentLength
		if m := contentRangeRe.FindStringSubmatch(strings.TrimSpace(res.Header.Get("Content-Range"))); m != nil {
			size, _ = strconv.ParseInt(m[3], 10, 64)
		}
		sig := fileSig{Size: strconv.FormatInt(size, 10)}
		if t, err := http.ParseTime(res.Header.Get("Last-Modified")); err == nil {
			sig.Mtime = t.UTC().Format("2006-01-02 15:04")
		}
		return sig, nil
	})
}

// ---------------------------------------------------------------------------
// Albums
// ---------------------------------------------------------------------------

var catboxTitleNoise = regexp.MustCompile(`(?i)^\s*catbox(?:\.moe)?\s*[-|:–—]?\s*|\s*[-|:–—]?\s*catbox(?:\.moe)?\s*$`)

// parseCatboxAlbum reads an album page: its title ("" if it has none worth using) and the
// names of the files it links to, in order, without duplicates.
func parseCatboxAlbum(body []byte) (title string, names []string) {
	if doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body)); err == nil {
		title = strings.TrimSpace(doc.Find("title").First().Text())
		if title = strings.TrimSpace(catboxTitleNoise.ReplaceAllString(title, "")); strings.EqualFold(title, "catbox") {
			title = ""
		}
	}
	seen := map[string]bool{}
	for _, m := range catboxPageFileRe.FindAllSubmatch(body, -1) {
		if n := string(m[1]); !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return title, names
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

type catboxProvider struct{}

func (catboxProvider) Kind() string { return catboxKind }

func (catboxProvider) ID(raw string) (string, error) { return parseCatboxAlbumLink(raw) }

func (catboxProvider) Load(raw string) (*remoteFolder, error) {
	id, err := parseCatboxAlbumLink(raw)
	if err != nil {
		return nil, err
	}
	body, err := catboxRetry(func() ([]byte, error) {
		res, err := catboxDo(catboxAlbumFetch(id), nil)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		return io.ReadAll(io.LimitReader(res.Body, 16<<20))
	})
	if err != nil {
		return nil, err
	}
	title, names := parseCatboxAlbum(body)
	if title == "" {
		title = id
	}
	folder := &remoteFolder{Name: title, Listing: map[string]fileSig{}, Labels: map[string]string{}}
	for _, n := range names {
		if !catboxIsZipName(n) {
			continue
		}
		u := catboxFileURL(n)
		folder.Labels[u] = n
		// A file's content never changes under its id, so no size or date is needed to
		// recognise a changed file: an empty signature means "read once, then remembered".
		folder.Listing[u] = fileSig{}
	}
	folder.Probe = func(u string) (manifestInfo, error) {
		name, err := parseCatboxFileLink(u)
		if err != nil {
			return manifestInfo{}, permanent("not a Catbox file: %s", u)
		}
		return catboxRanger{name}.probe()
	}
	return folder, nil
}

func (catboxProvider) FileID(raw string) (string, error) { return parseCatboxFileLink(raw) }

func (catboxProvider) FileURL(raw string) (string, error) {
	name, err := parseCatboxFileLink(raw)
	if err != nil {
		return "", err
	}
	return catboxFileURL(name), nil
}

// LoadFile looks at one public file link. The result lists the file only if its extension
// says it is a mod archive; the folder name is the file's name.
func (catboxProvider) LoadFile(raw string) (*remoteFolder, error) {
	name, err := parseCatboxFileLink(raw)
	if err != nil {
		return nil, err
	}
	u := catboxFileURL(name)
	folder := &remoteFolder{Name: name, Listing: map[string]fileSig{}, Labels: map[string]string{u: name}}
	if !catboxIsZipName(name) {
		return folder, nil
	}
	sig, err := catboxFileSig(name)
	if err != nil {
		return nil, err
	}
	if sig.Size == "0" || sig.Size == "-1" {
		return folder, nil // empty (or of unknown size): nothing to read
	}
	folder.Listing[u] = sig
	folder.Probe = func(string) (manifestInfo, error) { return catboxRanger{name}.probe() }
	return folder, nil
}

// RunCatbox indexes the Catbox albums and files listed in config/catbox.json and returns how
// many GUIDs are new and how many items failed. args are command-line style flags (nil =
// defaults).
func RunCatbox(args []string) (newGuids, failures int) {
	return runHosted(catboxProvider{}, args)
}
