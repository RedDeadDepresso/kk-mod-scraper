package scrapers

// GUID index for zipmods hosted on Gofile (gofile.io).
//
// Gofile has no anonymous listing: the web app creates a guest account, then calls
// GET api.gofile.io/contents/<id> with the account token as a Bearer token and a
// "website token" header, X-Website-Token: the SHA-256 of
// "<user agent>::<language>::<account token>::<4-hour slot>::<secret>". The secret is baked
// into Gofile's obfuscated script /js/wt.obf.js and rotates, so it is read out of that
// script (gofilewt.go, adapted from HeapLeach). If that fails, a pinned fallback is used;
// the GOFILE_SECRET environment variable overrides both.
//
// Gofile blocks addresses that sign badly. So once a signature is refused, the whole Gofile
// run stops (the abortable mechanism also used by Drive): one wrong secret costs one request.
//
// Files are downloaded from the storage servers (the "link" of each file) with the guest
// token as the accountToken cookie; the server honours Range requests, so per new or changed
// file only the zip's tail and manifest.xml are read (a few KB).
//
// config/gofile.json is the usual grouped config (see hostedconfig.go) of share links,
// https://gofile.io/d/<code>. A share is walked recursively. Single files have no link of
// their own: a share of one file is just a share. Output per share, named after it:
// data/gofile/<folder name>.json and cache/gofile/<folder name>.json (see hosted.go).
// The URLs in the index are https://gofile.io/d/<share code>#<file id>: the share's link
// (which is what opens it), with the file's id as a fragment so each file has its own URL.
//
// Password protected shares are skipped. Only .zipmod and .zip files are looked at.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	gofileKind           = "gofile"
	gofileUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	gofileLanguage       = "en-US"
	gofileRoot           = "https://gofile.io"
	gofileSecretEnv      = "GOFILE_SECRET"
	gofileIntervalEnv    = "GOFILE_MIN_INTERVAL"
	gofileFallbackSecret = "12af056dacea0b" // what the script held when this was written
	gofileCookie         = "accountToken"
	gofileSlot           = 4 * time.Hour
	gofileTokenTTL       = 3 * time.Hour // guest tokens live about 6 hours
	gofileSecretTTL      = time.Hour
	gofileSecretRetry    = 5 * time.Minute // how long the fallback is used before the script is tried again
	gofilePageSize       = 100
	gofileMaxPages       = 1000
	gofileMaxDepth       = 12
	gofileMaxFiles       = 20000
	gofileMaxScript      = 4 << 20
)

var (
	gofileAPIBase  = "https://api.gofile.io" // variables so tests can point them at a fake
	gofileSiteBase = "https://gofile.io"

	gofileHTTPClient = &http.Client{Timeout: 60 * time.Second}
	// Downloads must not follow redirects: a busy storage server answers with a redirect
	// to the web page, which is a failure to retry elsewhere, not a file.
	gofileDownloadClient = &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// Gofile doesn't publish its limits ("per endpoint", 429 when exceeded), so requests to
	// its API are spaced out, the spacing doubles after every 429 and eases back after
	// successes, and Retry-After is honoured. GOFILE_MIN_INTERVAL (e.g. "1s") sets the base.
	gofileMinInterval = 300 * time.Millisecond
	gofileRetryPause  = 500 * time.Millisecond
	gofileMaxInterval = 10 * time.Second
	gofileMaxWait     = 2 * time.Minute // longest Retry-After that is waited for
)

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

var (
	gofileShareRe  = regexp.MustCompile(`(?i)^https?://(?:www\.)?gofile\.io/d/([A-Za-z0-9][A-Za-z0-9-]*)/?(?:[?#].*)?$`)
	gofileLegacyRe = regexp.MustCompile(`(?i)^https?://(?:www\.)?gofile\.io/\?(?:[^#]*&)?c=([A-Za-z0-9][A-Za-z0-9-]*)(?:[&#].*)?$`)
	// Used to pull links out of free text (a post). The trailing group stops at the end of
	// the code, so a UUID (a sub-folder id) is not mistaken for a shorter code.
	gofileFindRe = regexp.MustCompile(`(?i)https?://(?:www\.)?gofile\.io/d/([A-Za-z0-9]{4,16})(?:[^A-Za-z0-9-]|$)`)
)

func parseGofileLink(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, re := range []*regexp.Regexp{gofileShareRe, gofileLegacyRe} {
		if m := re.FindStringSubmatch(s); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("not a Gofile link: %q (expected https://gofile.io/d/<code>)", s)
}

func gofileFolderURL(code string) string      { return gofileRoot + "/d/" + code }
func gofileFileURL(code, fileID string) string { return gofileFolderURL(code) + "#" + fileID }

// findGofileLinks returns the Gofile share links of a text, canonical, in order of appearance.
func findGofileLinks(text string) []string {
	var out []string
	for _, m := range gofileFindRe.FindAllStringSubmatch(text, -1) {
		out = append(out, gofileFolderURL(m[1]))
	}
	return out
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// gofileError is an error answer from Gofile: an HTTP status, and/or the API's own status
// string ("error-notFound", "error-notPremium", "error-wrongToken", ...).
type gofileError struct {
	Status     int
	Code       string
	Msg        string
	RetryAfter time.Duration // from a Retry-After header, if there was one
}

func (e *gofileError) Error() string {
	s := fmt.Sprintf("Gofile error %d", e.Status)
	if e.Code != "" {
		s += " (" + e.Code + ")"
	}
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

func (e *gofileError) is(marker string) bool { return strings.Contains(strings.ToLower(e.Code), marker) }

// permanent reports whether the share or file is gone. Everything else is retried on the
// next run and never purges a link.
func (e *gofileError) permanent() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusGone || e.is("notfound") || e.is("expired")
}
func (e *gofileError) signatureRejected() bool { return e.is("notpremium") }
func (e *gofileError) staleToken() bool        { return e.is("token") }
func (e *gofileError) passwordRequired() bool  { return e.is("password") }
func (e *gofileError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500 ||
		e.is("redirect") || e.is("webpage") || e.is("ratelimit")
}

var gofileErrCodeRe = regexp.MustCompile(`error-[A-Za-z]+`)

// gofileRetryAfter reads a Retry-After header (seconds or an HTTP date); 0 if absent.
func gofileRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(n, 0)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

func gofileParseError(status int, body []byte, h http.Header) *gofileError {
	e := &gofileError{Status: status, Msg: http.StatusText(status), RetryAfter: gofileRetryAfter(h)}
	var doc struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &doc) == nil && doc.Status != "" {
		e.Code = doc.Status
	} else {
		e.Code = gofileErrCodeRe.FindString(string(body))
	}
	return e
}

// gofileBackoff is how long to wait before retry number attempt+1: the server's Retry-After
// if it gave one (capped), else a growing pause.
func gofileBackoff(err error, attempt int) time.Duration {
	var ge *gofileError
	if errors.As(err, &ge) && ge.RetryAfter > 0 {
		return min(ge.RetryAfter, gofileMaxWait)
	}
	return time.Duration(attempt+1) * gofileRetryPause
}

// gofileRetry runs fn up to four times, retrying network errors, 429 and 5xx. A 429 also
// slows the pace of all later API requests; a success eases it back.
func gofileRetry[T any](fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 0; attempt < 4; attempt++ {
		if v, err = fn(); err == nil {
			gofileSpeedUp()
			return v, nil
		}
		var ge *gofileError
		if errors.As(err, &ge) {
			if ge.Status == http.StatusTooManyRequests {
				gofileSlowDown()
			}
			if !ge.retryable() {
				return v, err
			}
		}
		if attempt < 3 {
			wait := gofileBackoff(err, attempt)
			slog.Warn("Gofile asked to slow down or failed; waiting before retrying", "wait", wait.String(), "err", err)
			time.Sleep(wait)
		}
	}
	return v, err
}

// ---------------------------------------------------------------------------
// Session: guest account, signing secret, and the "stop the run" flag
// ---------------------------------------------------------------------------

type gofileSession struct {
	mu          sync.Mutex
	token       string
	tokenUntil  time.Time
	secret      string
	secretUntil time.Time
	stop        error // set once Gofile refused a signature: nothing more is sent this run
}

var (
	gofileState    = &gofileSession{}
	gofileThrottle sync.Mutex
	gofileLastReq  time.Time
	gofileInterval time.Duration // current spacing after 429s; 0 = the base gofileMinInterval
)

func (s *gofileSession) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token, s.tokenUntil = "", time.Time{}
	s.secret, s.secretUntil = "", time.Time{}
	s.stop = nil

	gofileThrottle.Lock()
	gofileInterval = 0
	gofileThrottle.Unlock()
	if v := strings.TrimSpace(os.Getenv(gofileIntervalEnv)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			gofileMinInterval = d
		} else {
			slog.Warn("Ignoring an invalid "+gofileIntervalEnv, "value", v)
		}
	}
}

func (s *gofileSession) stopped() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop
}

func (s *gofileSession) halt(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop == nil {
		s.stop = err
	}
	s.secret, s.secretUntil = "", time.Time{}
}

// gofilePace spaces requests to Gofile's API out by the current interval.
func gofilePace() {
	gofileThrottle.Lock()
	wait := max(max(gofileMinInterval, gofileInterval)-time.Since(gofileLastReq), 0)
	gofileLastReq = time.Now().Add(wait)
	gofileThrottle.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}

// gofileSlowDown doubles the spacing between requests (up to gofileMaxInterval).
func gofileSlowDown() {
	gofileThrottle.Lock()
	defer gofileThrottle.Unlock()
	cur := max(gofileMinInterval, gofileInterval, 250*time.Millisecond)
	gofileInterval = min(cur*2, gofileMaxInterval)
	slog.Warn("Gofile is rate limiting; spacing requests out", "interval", gofileInterval.String())
}

// gofileSpeedUp eases the spacing back towards the base after a success.
func gofileSpeedUp() {
	gofileThrottle.Lock()
	defer gofileThrottle.Unlock()
	if gofileInterval <= gofileMinInterval {
		gofileInterval = 0
		return
	}
	gofileInterval = max(gofileInterval*3/4, gofileMinInterval)
}

// currentSecret returns the secret to sign with: GOFILE_SECRET if set, else the one read from
// Gofile's script (kept for an hour), else the pinned fallback. The lock is held across the
// fetch so parallel callers don't each pull the script.
func (s *gofileSession) currentSecret() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return "", s.stop
	}
	if v := strings.TrimSpace(os.Getenv(gofileSecretEnv)); v != "" {
		return v, nil
	}
	now := time.Now()
	if s.secret != "" && now.Before(s.secretUntil) {
		return s.secret, nil
	}
	secret, err := gofileFetchSecret()
	if err != nil {
		slog.Warn("Could not read Gofile's signing secret from its script; using the built-in one", "err", err)
		s.secret, s.secretUntil = gofileFallbackSecret, now.Add(gofileSecretRetry)
		return s.secret, nil
	}
	slog.Info("Read Gofile's signing secret from its script")
	s.secret, s.secretUntil = secret, now.Add(gofileSecretTTL)
	return secret, nil
}

// accountToken returns a guest account token, creating one if needed. stale is a token the
// caller found rejected: it is not handed out again.
func (s *gofileSession) accountToken(stale string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return "", s.stop
	}
	if s.token != "" && s.token != stale && time.Now().Before(s.tokenUntil) {
		return s.token, nil
	}
	token, err := gofileMintAccount()
	if err != nil {
		return "", err
	}
	s.token, s.tokenUntil = token, time.Now().Add(gofileTokenTTL)
	return token, nil
}

func gofileFetchSecret() (string, error) {
	gofilePace()
	req, err := http.NewRequest(http.MethodGet, gofileSiteBase+gofileScriptPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", gofileUserAgent)
	req.Header.Set("Referer", gofileRoot+"/")
	res, err := gofileHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch signing script: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("fetch signing script: HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, gofileMaxScript+1))
	if err != nil {
		return "", err
	}
	if len(body) > gofileMaxScript {
		return "", errors.New("the signing script is implausibly large")
	}
	return gofileSecretFrom(string(body))
}

func gofileMintAccount() (string, error) {
	return gofileRetry(func() (string, error) {
		gofilePace()
		req, err := http.NewRequest(http.MethodPost, gofileAPIBase+"/accounts", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", gofileUserAgent)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", gofileRoot)
		req.Header.Set("Referer", gofileRoot+"/")
		res, err := gofileHTTPClient.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err != nil {
			return "", err
		}
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return "", gofileParseError(res.StatusCode, body, res.Header)
		}
		var env struct {
			Status string `json:"status"`
			Data   struct {
				Token string `json:"token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return "", err
		}
		if env.Status != "ok" || env.Data.Token == "" {
			return "", &gofileError{Status: res.StatusCode, Code: env.Status, Msg: "could not create a guest account"}
		}
		return env.Data.Token, nil
	})
}

// gofileWebsiteToken reproduces Gofile's client-side request signature.
func gofileWebsiteToken(ua, lang, accountToken, secret string, now time.Time) string {
	slot := strconv.FormatInt(now.Unix()/int64(gofileSlot/time.Second), 10)
	sum := sha256.Sum256([]byte(strings.Join([]string{ua, lang, accountToken, slot, secret}, "::")))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Content API
// ---------------------------------------------------------------------------

type gofileEnvelope struct {
	Status   string        `json:"status"`
	Data     gofileContent `json:"data"`
	Metadata struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"metadata"`
}

// gofileContent is one node of the content tree: a file or a folder.
type gofileContent struct {
	CanAccess      *bool                    `json:"canAccess"`
	ID             string                   `json:"id"`
	Type           string                   `json:"type"`
	Name           string                   `json:"name"`
	Size           int64                    `json:"size"`
	Link           string                   `json:"link"`
	Servers        []string                 `json:"servers"`
	CreateTime     json.RawMessage          `json:"createTime"`
	ModTime        json.RawMessage          `json:"modTime"`
	ChildrenIDs    []string                 `json:"childrenIds"`
	Children       map[string]gofileContent `json:"children"`
	Password       json.RawMessage          `json:"password"`
	PasswordStatus string                   `json:"passwordStatus"`
	Expire         json.RawMessage          `json:"expire"`
}

// accessError explains a canAccess:false answer.
func (c *gofileContent) accessError() error {
	if c.PasswordStatus != "" && c.PasswordStatus != "passwordOk" {
		return &gofileError{Status: 200, Code: "password", Msg: "the share is password protected"}
	}
	if p := string(c.Password); p != "" && p != "null" && p != "false" {
		return &gofileError{Status: 200, Code: "password", Msg: "the share is password protected"}
	}
	if e := string(c.Expire); e != "" && e != "null" {
		return &gofileError{Status: 200, Code: "expired", Msg: "the link has expired"}
	}
	return &gofileError{Status: 200, Code: "inaccessible", Msg: "content is not accessible (private, expired or removed)"}
}

// orderedChildren lists children in Gofile's own order, then any others by name.
func (c *gofileContent) orderedChildren() []*gofileContent {
	out := make([]*gofileContent, 0, len(c.Children))
	seen := make(map[string]bool, len(c.Children))
	for _, id := range c.ChildrenIDs {
		if child, ok := c.Children[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, &child)
		}
	}
	var rest []string
	for id := range c.Children {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	slices.SortFunc(rest, func(a, b string) int {
		return strings.Compare(strings.ToLower(c.Children[a].Name), strings.ToLower(c.Children[b].Name))
	})
	for _, id := range rest {
		child := c.Children[id]
		out = append(out, &child)
	}
	return out
}

func gofileUnix(raw json.RawMessage) int64 {
	var f float64
	if len(raw) == 0 || json.Unmarshal(raw, &f) != nil {
		return 0
	}
	return int64(f)
}

// gofileMtime is the file's upload date in the index's format ("" if unknown).
func gofileMtime(c *gofileContent) string {
	t := gofileUnix(c.CreateTime)
	if t <= 0 {
		t = gofileUnix(c.ModTime)
	}
	if t <= 0 {
		return ""
	}
	return time.Unix(t, 0).UTC().Format("2006-01-02 15:04")
}

// gofileFetchPage reads one page of one content id. A refused signature stops the run.
func gofileFetchPage(id, token string, page int) (*gofileEnvelope, error) {
	secret, err := gofileState.currentSecret()
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"page":          {strconv.Itoa(page)},
		"pageSize":      {strconv.Itoa(gofilePageSize)},
		"sortField":     {"createTime"},
		"sortDirection": {"-1"},
	}
	endpoint := gofileAPIBase + "/contents/" + url.PathEscape(id) + "?" + q.Encode()
	env, err := gofileRetry(func() (*gofileEnvelope, error) {
		gofilePace()
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", gofileUserAgent)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Website-Token", gofileWebsiteToken(gofileUserAgent, gofileLanguage, token, secret, time.Now()))
		req.Header.Set("X-BL", gofileLanguage)
		req.Header.Set("Origin", gofileRoot)
		req.Header.Set("Referer", gofileRoot+"/")
		res, err := gofileHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
		if err != nil {
			return nil, err
		}
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return nil, gofileParseError(res.StatusCode, body, res.Header)
		}
		var env gofileEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.Status != "ok" {
			return nil, &gofileError{Status: res.StatusCode, Code: env.Status, Msg: "the API refused the request"}
		}
		return &env, nil
	})
	var ge *gofileError
	if errors.As(err, &ge) && ge.signatureRejected() {
		err = fmt.Errorf("Gofile refused the request signature, so its signing secret is wrong; "+
			"stopping Gofile for this run (set %s to the current secret): %w", gofileSecretEnv, ge)
		gofileState.halt(err)
	}
	return env, err
}

// gofileFetchContent reads a content id with all its pages merged.
func gofileFetchContent(id, token string) (*gofileContent, error) {
	var head *gofileContent
	children := map[string]gofileContent{}
	var order []string
	for page := 1; page <= gofileMaxPages; page++ {
		env, err := gofileFetchPage(id, token, page)
		if err != nil {
			return nil, err
		}
		if env.Data.CanAccess != nil && !*env.Data.CanAccess {
			return nil, env.Data.accessError()
		}
		if head == nil {
			h := env.Data
			head = &h
		}
		items := env.Data.orderedChildren()
		for _, c := range items {
			if _, dup := children[c.ID]; !dup {
				order = append(order, c.ID)
			}
			children[c.ID] = *c
		}
		if !env.Metadata.HasNextPage || len(items) == 0 {
			break
		}
	}
	if head == nil {
		return nil, errors.New("Gofile returned no content")
	}
	merged := *head
	merged.Children, merged.ChildrenIDs = children, order
	return &merged, nil
}

type gofileEntry struct {
	c   *gofileContent
	rel string // path of the file inside the share
}

func gofileUsable(c *gofileContent) bool {
	return c.Type == "file" && c.Link != "" && c.Size > 0 && isZipmodName(c.Name)
}

// gofileWalk lists every .zipmod/.zip below root, recursively.
func gofileWalk(root *gofileContent, token string) ([]gofileEntry, error) {
	if root.Type == "file" { // a share of a single file
		if gofileUsable(root) {
			return []gofileEntry{{root, root.Name}}, nil
		}
		return nil, nil
	}
	var files []gofileEntry
	visited := 0
	var rec func(dir *gofileContent, rel string, depth int) error
	rec = func(dir *gofileContent, rel string, depth int) error {
		for _, child := range dir.orderedChildren() {
			if visited++; visited > gofileMaxFiles {
				return fmt.Errorf("more than %d entries in the share; stopped", gofileMaxFiles)
			}
			switch child.Type {
			case "folder":
				if depth >= gofileMaxDepth {
					continue
				}
				if err := gofileState.stopped(); err != nil {
					return err
				}
				sub, err := gofileFetchContent(child.ID, token)
				if err != nil {
					// %v, not %w: a missing sub-folder must not look like a vanished share.
					return fmt.Errorf("listing the Gofile folder %q: %v", child.Name, err)
				}
				if err := rec(sub, rel+child.Name+"/", depth+1); err != nil {
					return err
				}
			case "file":
				if gofileUsable(child) {
					files = append(files, gofileEntry{child, rel + child.Name})
				}
			}
		}
		return nil
	}
	return files, rec(root, "", 0)
}

// ---------------------------------------------------------------------------
// Range reader
// ---------------------------------------------------------------------------

// gofileRanger reads byte ranges of one file. It implements rangeReader; the size is known
// from the listing. A file often lives on several storage servers and a busy one redirects
// to the web page, so each retry rotates to the next server.
type gofileRanger struct {
	urls  []string
	token string
	size  int
}

func newGofileRanger(c *gofileContent, token string) *gofileRanger {
	urls := []string{c.Link}
	if u, err := url.Parse(c.Link); err == nil {
		for _, server := range c.Servers {
			if server == "" {
				continue
			}
			v := *u
			v.Host = server + ".gofile.io"
			if s := v.String(); !slices.Contains(urls, s) {
				urls = append(urls, s)
			}
		}
	}
	return &gofileRanger{urls: urls, token: token, size: int(c.Size)}
}

func (r *gofileRanger) Suffix(length int) (suffixResult, error) {
	length = min(length, r.size)
	start := r.size - length
	data, err := r.Range(start, r.size-1)
	if err != nil {
		return suffixResult{}, err
	}
	return suffixResult{data: data, total: r.size, start: start}, nil
}

func (r *gofileRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end || end >= r.size {
		return nil, fmt.Errorf("invalid range %d-%d for a file of %d bytes", start, end, r.size)
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var data []byte
		if data, err = gofileReadRange(r.urls[attempt%len(r.urls)], r.token, start, end); err == nil {
			return data, nil
		}
		var ge *gofileError
		if errors.As(err, &ge) && !ge.retryable() {
			return nil, err
		}
		if attempt < 2 {
			time.Sleep(gofileBackoff(err, attempt))
		}
	}
	return nil, err
}

func gofileReadRange(rawURL, token string, start, end int) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", gofileUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cookie", gofileCookie+"="+token)
	req.Header.Set("Referer", gofileRoot+"/")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	res, err := gofileDownloadClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode >= 300 && res.StatusCode < 400:
		return nil, &gofileError{Status: res.StatusCode, Code: "redirect", Msg: "the storage server sent us to the web page instead of the file"}
	case res.StatusCode < 200 || res.StatusCode > 299:
		return nil, &gofileError{Status: res.StatusCode, Msg: http.StatusText(res.StatusCode), RetryAfter: gofileRetryAfter(res.Header)}
	case strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "text/html"):
		return nil, &gofileError{Status: res.StatusCode, Code: "webpage", Msg: "the storage server answered with a web page instead of the file"}
	case res.StatusCode != http.StatusPartialContent && !(res.StatusCode == http.StatusOK && start == 0):
		return nil, errors.New("Gofile ignored the Range request; stopped to avoid downloading a whole file")
	}
	want := end - start + 1
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(want)+1))
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusOK && len(data) > want {
		data = data[:want] // a full response from the start: keep just what was asked for
	}
	if len(data) != want {
		return nil, fmt.Errorf("Gofile returned %d bytes for a %d-byte range", len(data), want)
	}
	return data, nil
}

// probe reads the GUID and version of one file.
func (r *gofileRanger) probe() (manifestInfo, error) {
	if r.size <= 0 {
		return manifestInfo{}, permanent("empty file")
	}
	info, err := readManifest(r)
	var ge *gofileError
	if errors.As(err, &ge) && ge.permanent() {
		return manifestInfo{}, permanent("no longer on Gofile (%v)", ge)
	}
	return info, err
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

type gofileProvider struct{}

func (gofileProvider) Kind() string { return gofileKind }

func (gofileProvider) ID(raw string) (string, error) { return parseGofileLink(raw) }

// Begin and Aborted make a refused signature end the whole Gofile run (see hosted.go).
func (gofileProvider) Begin()         { gofileState.reset() }
func (gofileProvider) Aborted() error { return gofileState.stopped() }

func (gofileProvider) Load(raw string) (*remoteFolder, error) {
	code, err := parseGofileLink(raw)
	if err != nil {
		return nil, err
	}
	if err := gofileState.stopped(); err != nil {
		return nil, err
	}
	token, err := gofileState.accountToken("")
	if err != nil {
		return nil, err
	}
	root, err := gofileFetchContent(code, token)
	var ge *gofileError
	if errors.As(err, &ge) && ge.staleToken() {
		if token, err = gofileState.accountToken(token); err != nil {
			return nil, err
		}
		root, err = gofileFetchContent(code, token)
	}
	var pe *gofileError
	if errors.As(err, &pe) && pe.passwordRequired() {
		slog.Info("Skipping a password protected Gofile share", "folder", code)
		return &remoteFolder{Name: code, Listing: map[string]fileSig{}, Labels: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := gofileWalk(root, token)
	if err != nil {
		return nil, err
	}

	name := root.Name
	if name == "" {
		name = code
	}
	folder := &remoteFolder{Name: name, Listing: map[string]fileSig{}, Labels: map[string]string{}}
	rangers := map[string]*gofileRanger{}
	for _, e := range entries {
		u := gofileFileURL(code, e.c.ID)
		folder.Labels[u] = e.rel
		folder.Listing[u] = fileSig{Mtime: gofileMtime(e.c), Size: strconv.FormatInt(e.c.Size, 10)}
		rangers[u] = newGofileRanger(e.c, token)
	}
	folder.Probe = func(u string) (manifestInfo, error) {
		r, ok := rangers[u]
		if !ok {
			return manifestInfo{}, permanent("file is no longer in the Gofile share")
		}
		return r.probe()
	}
	return folder, nil
}

// RunGofile indexes the Gofile shares listed in config/gofile.json and returns how many
// GUIDs are new and how many items failed. args are command-line style flags (nil = defaults).
func RunGofile(args []string) (newGuids, failures int) {
	return runHosted(gofileProvider{}, args)
}
