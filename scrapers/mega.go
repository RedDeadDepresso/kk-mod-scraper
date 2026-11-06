package scrapers

// GUID index for zipmods hosted in a public Mega folder.
//
// Mega files are end-to-end encrypted, so the BetterRepack trick (HTTP Range requests for
// the zip's tail and manifest.xml) needs one extra step. It works because:
//   - the folder link carries the folder key; the folder listing (one API call) holds every
//     file's name, size, date and key, all encrypted with the folder key;
//   - file contents are AES-128-CTR, which is seekable: any byte range can be decrypted on
//     its own once the counter is set to offset/16;
//   - the download URL Mega returns for a file accepts a byte range ("<url>/<start>-<end>").
//
// So per new or changed file this reads a few KB (the zip's tail, then manifest.xml), never
// the whole file, and then reuses the same zip/manifest code as the BetterRepack scraper
// (readManifest) and the same incremental cache and index writer.
//
// Configuration: config/mega.json is a JSON array of public folder links:
//
//	["https://mega.nz/folder/<id>#<key>", ...]
//
// Output, one pair of files per folder, named after the folder as it is called on Mega:
//
//	indices/mega/<folder name>.json   {"generated": "...", "game_type": "mega", "count": 123,
//	                                   "guids": {"com.example.mod": ["https://mega.nz/folder/<id>#<key>/file/<node>", ...]}}
//	cache/mega/<folder name>.json     incremental-update cache ({url: {mtime, size, guid, version, error}})
//
// The URLs are Mega's standard "file inside a folder link" form; the cache's mtime is the
// file's upload time on Mega. Two folders with the same name get "<name> (<id>)". Files of
// folders that were removed from the config (or renamed on Mega) are deleted at the end of a
// run in which every folder could be read.

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	megaUserAgent     = "kk-mod-scraper-mega-index-builder/1.0"
	defaultMegaConfig = "config/mega.json"
	defaultCacheDir   = "cache"
	megaSubdir        = "mega" // <indices dir>/mega/ and <cache dir>/mega/
	megaGameType      = "mega"
)

var (
	megaHTTPClient = &http.Client{Timeout: 60 * time.Second}
	megaAPIURL     = "https://g.api.mega.co.nz/cs" // a variable so tests can point it at a fake
)

// ---------------------------------------------------------------------------
// Links and base64
// ---------------------------------------------------------------------------

type megaFolderLink struct {
	Handle string // public folder handle (the "n" parameter of API calls)
	KeyB64 string
	Key    []byte // 16 bytes
}

var (
	megaFolderRe = regexp.MustCompile(`^https?://mega\.(?:nz|io|co\.nz)/folder/([A-Za-z0-9_-]{8})#([A-Za-z0-9_-]{22})(?:[/?].*)?$`)
	megaLegacyRe = regexp.MustCompile(`^https?://mega\.(?:nz|io|co\.nz)/#F!([A-Za-z0-9_-]{8})!([A-Za-z0-9_-]{22})(?:[!/?].*)?$`)
)

func parseMegaFolderLink(s string) (megaFolderLink, error) {
	s = strings.TrimSpace(s)
	m := megaFolderRe.FindStringSubmatch(s)
	if m == nil {
		m = megaLegacyRe.FindStringSubmatch(s)
	}
	if m == nil {
		return megaFolderLink{}, fmt.Errorf("not a Mega folder link: %q (expected https://mega.nz/folder/<id>#<key>)", s)
	}
	key, err := megaB64Decode(m[2])
	if err != nil || len(key) != 16 {
		return megaFolderLink{}, errors.New("the Mega folder key in the link is invalid")
	}
	return megaFolderLink{Handle: m[1], KeyB64: m[2], Key: key}, nil
}

func (l megaFolderLink) fileURL(handle string) string {
	return fmt.Sprintf("https://mega.nz/folder/%s#%s/file/%s", l.Handle, l.KeyB64, handle)
}

// megaB64Decode decodes Mega's base64 (URL-safe alphabet, no padding; the standard alphabet
// and padding are tolerated).
func megaB64Decode(s string) ([]byte, error) {
	s = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(s, "="))
	return base64.RawURLEncoding.DecodeString(s)
}

// ---------------------------------------------------------------------------
// Crypto
// ---------------------------------------------------------------------------

// megaECBDecrypt decrypts data block by block (Mega encrypts node keys this way).
func megaECBDecrypt(key, data []byte) ([]byte, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("encrypted key has an invalid length")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		c.Decrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return out, nil
}

// megaAttrKey is the AES key that protects a node's attributes: a folder's own 16-byte key,
// or the first half of a file's 32-byte key XOR the second half.
func megaAttrKey(nodeKey []byte) ([]byte, error) {
	switch len(nodeKey) {
	case 16:
		return nodeKey, nil
	case 32:
		k := make([]byte, 16)
		for i := range k {
			k[i] = nodeKey[i] ^ nodeKey[i+16]
		}
		return k, nil
	}
	return nil, fmt.Errorf("unexpected node key length %d", len(nodeKey))
}

// megaDecryptName returns the file name from a node's encrypted attributes (AES-CBC, zero
// IV, plaintext "MEGA{...json...}" zero-padded).
func megaDecryptName(nodeKey []byte, attrB64 string) (string, error) {
	aesKey, err := megaAttrKey(nodeKey)
	if err != nil {
		return "", err
	}
	data, err := megaB64Decode(attrB64)
	if err != nil {
		return "", err
	}
	if rem := len(data) % aes.BlockSize; rem != 0 {
		data = append(data, make([]byte, aes.BlockSize-rem)...)
	}
	if len(data) == 0 {
		return "", errors.New("empty attributes")
	}
	c, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(c, make([]byte, aes.BlockSize)).CryptBlocks(plain, data)
	if len(plain) < 4 || string(plain[:4]) != "MEGA" {
		return "", errors.New("attributes did not decrypt (wrong key?)")
	}
	js := plain[4:]
	if i := strings.IndexByte(string(js), 0); i >= 0 {
		js = js[:i]
	}
	var attrs struct {
		N string `json:"n"`
	}
	if err := json.Unmarshal(js, &attrs); err != nil {
		return "", fmt.Errorf("unreadable attributes: %w", err)
	}
	return attrs.N, nil
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// megaError is a negative error code returned by Mega's API.
type megaError int

func (e megaError) Error() string {
	names := map[megaError]string{
		-3: "EAGAIN (try again)", -4: "ERATELIMIT", -8: "EEXPIRED", -9: "ENOENT (not found)",
		-11: "EACCESS", -16: "EBLOCKED (taken down)", -17: "EOVERQUOTA (bandwidth quota exceeded)",
		-18: "ETEMPUNAVAIL",
	}
	if n, ok := names[e]; ok {
		return fmt.Sprintf("Mega API error %d %s", int(e), n)
	}
	return fmt.Sprintf("Mega API error %d", int(e))
}

func (e megaError) retryable() bool { return e == -3 || e == -4 || e == -18 }

type megaClient struct {
	link megaFolderLink
	seq  atomic.Int64
}

func newMegaClient(link megaFolderLink) *megaClient {
	c := &megaClient{link: link}
	c.seq.Store(time.Now().UnixNano() % 1_000_000_000)
	return c
}

// call sends one API command about the folder and decodes the result into out.
func (c *megaClient) call(req map[string]any, out any) error {
	body, err := json.Marshal([]any{req})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 500 * time.Millisecond)
		}
		lastErr = c.callOnce(body, out)
		if lastErr == nil {
			return nil
		}
		var me megaError
		if errors.As(lastErr, &me) && !me.retryable() {
			return lastErr // a definite answer from Mega; asking again won't change it
		}
	}
	return lastErr
}

func (c *megaClient) callOnce(body []byte, out any) error {
	u := fmt.Sprintf("%s?id=%d&n=%s", megaAPIURL, c.seq.Add(1), c.link.Handle)
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", megaUserAgent)
	req.Header.Set("Content-Type", "application/json")
	res, err := megaHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP Error %d: %s", res.StatusCode, http.StatusText(res.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
	if err != nil {
		return err
	}
	return decodeMegaResponse(data, out)
}

// decodeMegaResponse handles both reply shapes: a bare error code, or an array holding one
// result per command (which may itself be an error code).
func decodeMegaResponse(data []byte, out any) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return errors.New("empty reply from Mega")
	}
	if code, err := strconv.Atoi(string(data)); err == nil {
		return megaError(code)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil || len(arr) == 0 {
		return fmt.Errorf("unexpected reply from Mega: %.80s", data)
	}
	if code, err := strconv.Atoi(strings.TrimSpace(string(arr[0]))); err == nil {
		return megaError(code)
	}
	return json.Unmarshal(arr[0], out)
}

type megaNode struct {
	H  string `json:"h"`
	P  string `json:"p"`
	T  int    `json:"t"` // 0 file, 1 folder
	A  string `json:"a"`
	K  string `json:"k"`
	S  int64  `json:"s"`
	Ts int64  `json:"ts"`
}

type megaFile struct {
	Handle string
	Name   string
	Size   int64
	Ts     int64
	Key    []byte // 32 bytes
}

// listFiles returns the folder's name and every file in it (all levels), with decrypted
// names and keys. The folder name is "" if it can't be read.
func (c *megaClient) listFiles() (string, []megaFile, error) {
	var resp struct {
		F []megaNode `json:"f"`
	}
	if err := c.call(map[string]any{"a": "f", "c": 1, "r": 1}, &resp); err != nil {
		return "", nil, err
	}
	known := make(map[string]bool, len(resp.F))
	for _, n := range resp.F {
		known[n.H] = true
	}
	// The folder's root node is the one whose parent isn't part of the listing; node keys
	// are tagged "<root handle>:<key>" (possibly several, separated by "/").
	rootHandle, folderName := "", ""
	for _, n := range resp.F {
		if n.T == 1 && !known[n.P] {
			rootHandle = n.H
			if key, err := c.decryptNodeKey(n.K, rootHandle); err == nil {
				folderName, _ = megaDecryptName(key, n.A)
			}
			break
		}
	}

	var files []megaFile
	for _, n := range resp.F {
		if n.T != 0 {
			continue
		}
		key, err := c.decryptNodeKey(n.K, rootHandle)
		if err != nil || len(key) != 32 {
			slog.Warn("Skipping Mega file with an unreadable key", "handle", n.H, "err", err)
			continue
		}
		name, err := megaDecryptName(key, n.A)
		if err != nil {
			slog.Warn("Skipping Mega file with unreadable attributes", "handle", n.H, "err", err)
			continue
		}
		files = append(files, megaFile{Handle: n.H, Name: name, Size: n.S, Ts: n.Ts, Key: key})
	}
	return folderName, files, nil
}

func (c *megaClient) decryptNodeKey(k, rootHandle string) ([]byte, error) {
	var candidate string
	for _, part := range strings.Split(k, "/") {
		h, key, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		if h == rootHandle {
			candidate = key
			break
		}
		if candidate == "" {
			candidate = key
		}
	}
	if candidate == "" {
		return nil, errors.New("no key for this folder")
	}
	raw, err := megaB64Decode(candidate)
	if err != nil {
		return nil, err
	}
	return megaECBDecrypt(c.link.Key, raw)
}

// downloadURL asks Mega for a (temporary) download URL of a file in the folder.
func (c *megaClient) downloadURL(handle string) (string, int64, error) {
	var resp struct {
		G string `json:"g"`
		S int64  `json:"s"`
		E int    `json:"e"`
	}
	if err := c.call(map[string]any{"a": "g", "g": 1, "ssl": 1, "n": handle}, &resp); err != nil {
		return "", 0, err
	}
	if resp.E != 0 {
		return "", 0, megaError(resp.E)
	}
	if resp.G == "" {
		return "", 0, errors.New("Mega returned no download URL")
	}
	return resp.G, resp.S, nil
}

// ---------------------------------------------------------------------------
// Decrypting range reader
// ---------------------------------------------------------------------------

// megaRanger reads decrypted byte ranges of one Mega file. It implements rangeReader.
type megaRanger struct {
	url   string
	size  int
	block cipher.Block
	nonce [8]byte
}

func newMegaRanger(downloadURL string, size int, fileKey []byte) (*megaRanger, error) {
	if len(fileKey) != 32 {
		return nil, errors.New("file key must be 32 bytes")
	}
	aesKey, _ := megaAttrKey(fileKey)
	b, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	m := &megaRanger{url: downloadURL, size: size, block: b}
	copy(m.nonce[:], fileKey[16:24]) // words 4 and 5 of the key are the CTR nonce
	return m, nil
}

func (m *megaRanger) Suffix(length int) (suffixResult, error) {
	length = min(length, m.size)
	start := m.size - length
	data, err := m.Range(start, m.size-1)
	if err != nil {
		return suffixResult{}, err
	}
	return suffixResult{data: data, total: m.size, start: start}, nil
}

// Range returns the decrypted bytes start..end (inclusive).
func (m *megaRanger) Range(start, end int) ([]byte, error) {
	if start < 0 || start > end || end >= m.size {
		return nil, fmt.Errorf("invalid range %d-%d for a file of %d bytes", start, end, m.size)
	}
	aligned := start &^ (aes.BlockSize - 1)
	raw, err := m.fetch(aligned, end)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, aes.BlockSize)
	copy(iv, m.nonce[:])
	binary.BigEndian.PutUint64(iv[8:], uint64(aligned/aes.BlockSize))
	cipher.NewCTR(m.block, iv).XORKeyStream(raw, raw)
	return raw[start-aligned:], nil
}

// fetch downloads the still-encrypted bytes start..end (inclusive).
func (m *megaRanger) fetch(start, end int) ([]byte, error) {
	want := end - start + 1
	return withRetries(func() ([]byte, error) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/%d-%d", m.url, start, end), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", megaUserAgent)
		req.Header.Set("Accept-Encoding", "identity")
		res, err := megaHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		switch {
		case res.StatusCode == 509:
			return nil, errors.New("HTTP 509: Mega bandwidth quota exceeded for this IP")
		case res.StatusCode < 200 || res.StatusCode > 299:
			return nil, fmt.Errorf("HTTP Error %d: %s", res.StatusCode, http.StatusText(res.StatusCode))
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, int64(want)+1))
		if err != nil {
			return nil, err
		}
		if len(data) != want {
			return nil, fmt.Errorf("Mega returned %d bytes for a %d-byte range (not the expected partial download)", len(data), want)
		}
		return data, nil
	})
}

// ---------------------------------------------------------------------------
// Config and file names
// ---------------------------------------------------------------------------

// loadMegaConfig reads the list of folder links (a JSON array of strings).
func loadMegaConfig(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read the Mega config: %w", err)
	}
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s must be a JSON array of Mega folder links: %w", path, err)
	}
	links := make([]string, 0, len(raw))
	for _, l := range raw {
		if l = strings.TrimSpace(l); l != "" {
			links = append(links, l)
		}
	}
	return links, nil
}

// safeFileName turns a Mega folder name into something usable as a file name on any
// platform (keeps letters of any script, digits, spaces and common punctuation).
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

// megaFileBase picks the file name (without ".json") for a folder: its Mega name, or
// "<name> (<id>)" if another folder of this run already took that name, or just the id if
// the folder has no usable name. used maps a lowercase name to the folder handle that owns it
// (file systems may be case-insensitive).
func megaFileBase(name, handle string, used map[string]string) string {
	base := safeFileName(name)
	if base == "" {
		base = handle
	}
	if owner, taken := used[strings.ToLower(base)]; taken && owner != handle {
		base = fmt.Sprintf("%s (%s)", base, handle)
	}
	used[strings.ToLower(base)] = handle
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
// Entry point
// ---------------------------------------------------------------------------

type megaOptions struct {
	configPath  string
	indicesDir  string // index files go to <indicesDir>/mega/
	cacheDir    string // cache files go to <cacheDir>/mega/
	full        bool
	retryFailed bool
	maxURLs     int
	workers     int
}

func (o megaOptions) indexDir() string { return filepath.Join(o.indicesDir, megaSubdir) }
func (o megaOptions) cacheSub() string { return filepath.Join(o.cacheDir, megaSubdir) }

func isZipmodName(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".zipmod") || strings.HasSuffix(l, ".zip")
}

// syncMegaFolder indexes one folder and returns the base name of its two files. unreachable
// is how many files couldn't be probed this time (they are retried next run).
func syncMegaFolder(link megaFolderLink, opt megaOptions, used map[string]string) (base string, newGuids, unreachable int, err error) {
	client := newMegaClient(link)

	slog.Info("Listing the Mega folder", "folder", link.Handle)
	folderName, all, err := client.listFiles()
	if err != nil {
		return "", 0, 0, fmt.Errorf("could not list the Mega folder %s: %w", link.Handle, err)
	}
	base = megaFileBase(folderName, link.Handle, used)

	byURL := map[string]megaFile{}
	listing := map[string]fileSig{}
	skipped := 0
	for _, f := range all {
		if !isZipmodName(f.Name) {
			skipped++
			continue
		}
		u := link.fileURL(f.Handle)
		byURL[u] = f
		listing[u] = fileSig{
			Mtime: time.Unix(f.Ts, 0).UTC().Format("2006-01-02 15:04"),
			Size:  strconv.FormatInt(f.Size, 10),
		}
	}
	slog.Info("Mega folder listed", "name", base, "zipmods", len(listing), "other_files_ignored", skipped)
	if len(listing) == 0 {
		return base, 0, 0, fmt.Errorf("the Mega folder %q has no .zipmod files (wrong link, or an empty folder?)", base)
	}

	cachePath := filepath.Join(opt.cacheSub(), base+".json")
	cache := map[string]cacheEntry{}
	if opt.full {
		slog.Info("Full rescan requested", "name", base)
	} else {
		cache = loadCache(cachePath)
		slog.Info("Cache loaded", "name", base, "files", len(cache))
	}

	probe := func(u string) (manifestInfo, error) {
		f, ok := byURL[u]
		if !ok {
			return manifestInfo{}, permanent("file is no longer in the Mega folder")
		}
		if f.Size == 0 {
			return manifestInfo{}, permanent("empty file")
		}
		dl, size, err := client.downloadURL(f.Handle)
		if err != nil {
			var me megaError
			if errors.As(err, &me) && (me == -9 || me == -16 || me == -8) {
				return manifestInfo{}, permanent("unavailable on Mega (%v)", err)
			}
			return manifestInfo{}, err
		}
		if size <= 0 {
			size = f.Size
		}
		r, err := newMegaRanger(dl, int(size), f.Key)
		if err != nil {
			return manifestInfo{}, permanent("%v", err)
		}
		return readManifest(r)
	}

	entries, unreachable := refreshCache(listing, cache, nil, refreshOptions{
		full: opt.full, retryFailed: opt.retryFailed, workers: opt.workers, cachePath: cachePath,
		probe:   probe,
		label:   func(u string) string { return byURL[u].Name },
		noMoves: true, // every file has its own node handle; name/date/size matching would only mislead
	})

	// All files share one "category", so buildGameIndex just orders by newest version, then
	// newest upload, and drops identical copies.
	index := buildGameIndex(entries, []string{""}, "", opt.maxURLs)
	out := filepath.Join(opt.indexDir(), base+".json")
	prev := readIndexGuids(out)
	for guid := range index {
		if _, ok := prev[guid]; !ok {
			newGuids++
		}
	}
	wrote, err := writeIndex(out, megaGameType, index)
	if err != nil {
		return base, 0, unreachable, fmt.Errorf("could not write %s: %w", out, err)
	}
	slog.Info("Mega index", "name", base, "guids", len(index), "path", out, "changed", wrote)
	if unreachable > 0 {
		slog.Warn("Some Mega files are missing from the index; the next run will try them again",
			"name", base, "unreachable_files", unreachable)
	}
	return base, newGuids, unreachable, nil
}

// buildMegaIndexes indexes every folder of the config. newGuids counts GUIDs that weren't in
// the previous index of their folder; failures counts folders (or config problems) that
// could not be processed at all - the other folders are still done.
func buildMegaIndexes(opt megaOptions) (newGuids, failures int) {
	links, err := loadMegaConfig(opt.configPath)
	if err != nil {
		slog.Error("Mega config problem", "err", err)
		return 0, 1
	}
	if len(links) == 0 {
		slog.Warn("No Mega folders in the config; nothing to do", "config", opt.configPath)
		return 0, 0
	}

	seen := map[string]bool{}
	used := map[string]string{}
	keep := map[string]bool{}
	for _, raw := range links {
		link, err := parseMegaFolderLink(raw)
		if err != nil {
			slog.Error("Skipping an invalid Mega link in the config", "err", err)
			failures++
			continue
		}
		if seen[link.Handle] {
			slog.Warn("Mega folder listed twice in the config; using it once", "folder", link.Handle)
			continue
		}
		seen[link.Handle] = true

		base, n, _, err := syncMegaFolder(link, opt, used)
		if base != "" {
			keep[base] = true // keep its files even if this run couldn't finish them
		}
		if err != nil {
			slog.Error("Mega folder failed", "folder", link.Handle, "err", err)
			failures++
			continue
		}
		newGuids += n
	}

	// Only clean up when every folder was read: a folder that failed to list has an unknown
	// name, and its files must not be mistaken for leftovers.
	if failures == 0 {
		removeStaleJSON(opt.indexDir(), keep)
		removeStaleJSON(opt.cacheSub(), keep)
	}
	return newGuids, failures
}

// RunMega indexes the Mega folders listed in the config and returns how many GUIDs are new
// and how many folders failed. args are command-line style flags (nil = defaults).
func RunMega(args []string) (newGuids, failures int) {
	fs := flag.NewFlagSet("mega", flag.ExitOnError)
	config := fs.String("config", defaultMegaConfig, "JSON array of public Mega folder links")
	indicesDir := fs.String("indices-dir", defaultIndicesDir, "index files are written to <dir>/mega/<folder name>.json")
	cacheDir := fs.String("cache-dir", defaultCacheDir, "cache files are written to <dir>/mega/<folder name>.json")
	full := fs.Bool("full", false, "ignore the caches and probe every file")
	retryFailed := fs.Bool("retry-failed", false, "probe again the files recorded as not indexable")
	maxURLs := fs.Int("max-urls", defaultMaxURLs, "most URLs kept per GUID")
	workers := fs.Int("workers", 4, "parallel downloads (max 8; Mega rate-limits aggressively)")
	_ = fs.Parse(args)

	return buildMegaIndexes(megaOptions{
		configPath:  *config,
		indicesDir:  *indicesDir,
		cacheDir:    *cacheDir,
		full:        *full,
		retryFailed: *retryFailed,
		maxURLs:     max(1, *maxURLs),
		workers:     max(1, min(*workers, 8)),
	})
}
