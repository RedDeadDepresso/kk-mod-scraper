package scrapers

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseMegaFolderLink(t *testing.T) {
	for _, s := range []string{
		"https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg",
		"https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg/file/6AEB2bSD",
		"https://mega.nz/#F!HM0FSAYC!Gu2wMS8oer6bISpDzVdBWg",
	} {
		l, err := parseMegaFolderLink(s)
		if err != nil || l.Handle != "HM0FSAYC" || len(l.Key) != 16 {
			t.Errorf("%q: got %+v, %v", s, l, err)
		}
	}
	for _, s := range []string{"", "https://mega.nz/file/abc#def", "https://example.com/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg", "https://mega.nz/folder/HM0FSAYC"} {
		if _, err := parseMegaFolderLink(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}

// Real nodes from the public folder (key, attributes) decrypt to the real names.
func TestMegaRealNodes(t *testing.T) {
	link, _ := parseMegaFolderLink("https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg")
	c := newMegaClient(link)

	rootKey, err := c.decryptNodeKey("iJET2ZZL:LP_-N08ifPp-b7H1M7ikfQ", "iJET2ZZL")
	if err != nil {
		t.Fatal(err)
	}
	if name, err := megaDecryptName(rootKey, "emg9_y2Nv8RtKdaj3jbK6WKivfMMO7TRzVpsWJ9SXHI"); err != nil || name != "anse mod" {
		t.Errorf("root name = %q, %v", name, err)
	}

	fileKey, err := c.decryptNodeKey("iJET2ZZL:T9YEidTAhD7WeHew-HlZIytCYSW28lukgEnwKR8Kk-U", "iJET2ZZL")
	if err != nil || len(fileKey) != 32 {
		t.Fatalf("file key: %v (len %d)", err, len(fileKey))
	}
	name, err := megaDecryptName(fileKey, "e-LUp05HFCavjGu1Mf9vurajxGrj2wdXuT8t218WC7cbc36_BmosPzpu4jtP0wAcBY3WHJveu0E3uZJK_1TLlf3c5SPneZtCuI7FU03-J_E")
	if err != nil || name != "anse_cardboard_cutout.zipmod" {
		t.Errorf("file name = %q, %v", name, err)
	}
	// A wrong key must be detected, not silently produce garbage.
	if _, err := megaDecryptName(make([]byte, 32), "e-LUp05HFCavjGu1Mf9vurajxGrj2wdXuT8t218WC7cbc36_BmosPzpu4jtP0wAcBY3WHJveu0E3uZJK_1TLlf3c5SPneZtCuI7FU03-J_E"); err == nil {
		t.Error("wrong key accepted")
	}
}

func TestDecodeMegaResponse(t *testing.T) {
	var out struct{ G string }
	var me megaError
	if err := decodeMegaResponse([]byte("-3"), &out); !errors.As(err, &me) || me != -3 || !me.retryable() {
		t.Errorf("bare code: %v", err)
	}
	if err := decodeMegaResponse([]byte("[-9]"), &out); !errors.As(err, &me) || me != -9 || me.retryable() {
		t.Errorf("array code: %v", err)
	}
	if err := decodeMegaResponse([]byte(`[{"G":"x"}]`), &out); err != nil || out.G != "x" {
		t.Errorf("object: %v %+v", err, out)
	}
	if err := decodeMegaResponse([]byte("<html>"), &out); err == nil {
		t.Error("garbage accepted")
	}
}

// ---------- fake Mega ----------

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func ecbEncrypt(key, data []byte) []byte {
	c, _ := aes.NewCipher(key)
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += 16 {
		c.Encrypt(out[i:i+16], data[i:i+16])
	}
	return out
}

func encryptAttrs(nodeKey []byte, name string) string {
	ak, _ := megaAttrKey(nodeKey)
	js, _ := json.Marshal(map[string]string{"n": name})
	plain := append([]byte("MEGA"), js...)
	if rem := len(plain) % 16; rem != 0 {
		plain = append(plain, make([]byte, 16-rem)...)
	}
	c, _ := aes.NewCipher(ak)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(c, make([]byte, 16)).CryptBlocks(out, plain)
	return b64(out)
}

// encryptContent encrypts like Mega: AES-CTR, key = k[0:16]^k[16:32], nonce = k[16:24], counter from 0.
func encryptContent(fileKey, plain []byte) []byte {
	ak, _ := megaAttrKey(fileKey)
	c, _ := aes.NewCipher(ak)
	iv := make([]byte, 16)
	copy(iv, fileKey[16:24])
	out := make([]byte, len(plain))
	cipher.NewCTR(c, iv).XORKeyStream(out, plain)
	return out
}

func makeZipmod(t *testing.T, guid, version string, bigFirst bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, data []byte) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	manifest := fmt.Sprintf(`<?xml version="1.0"?><manifest><guid>%s</guid><name>x</name><version>%s</version></manifest>`, guid, version)
	if bigFirst {
		junk := make([]byte, 600*1024) // incompressible: pushes manifest.xml past the first 64 KB
		rand.Read(junk)
		add("abdata/big.unity3d", junk)
		add("manifest.xml", []byte(manifest))
	} else {
		add("manifest.xml", []byte(manifest))
		add("abdata/small.txt", []byte("hello"))
	}
	zw.Close()
	return buf.Bytes()
}

type fakeFile struct {
	handle, name string
	ts           int64
	key          []byte
	cipher       []byte
}

func newFakeFile(t *testing.T, handle, name string, ts int64, zipData []byte) *fakeFile {
	key := make([]byte, 32)
	rand.Read(key)
	return &fakeFile{handle, name, ts, key, encryptContent(key, zipData)}
}

type fakeMega struct {
	link             megaFolderLink
	files            map[string]*fakeFile
	apiF, apiG, gets atomic.Int64
	storage          *httptest.Server
	api              *httptest.Server
}

func newFakeMega(t *testing.T, link megaFolderLink, files ...*fakeFile) *fakeMega {
	f := &fakeMega{link: link, files: map[string]*fakeFile{}}
	for _, ff := range files {
		f.files[ff.handle] = ff
	}
	f.storage = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gets.Add(1)
		var h string
		var s, e int
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		h = parts[0]
		fmt.Sscanf(parts[1], "%d-%d", &s, &e)
		ff := f.files[h]
		if ff == nil || s < 0 || e >= len(ff.cipher) || s > e {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Write(ff.cipher[s : e+1])
	}))
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("n") != link.Handle {
			t.Errorf("API call without the folder handle: %s", r.URL)
		}
		var reqs []map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &reqs); err != nil || len(reqs) != 1 {
			t.Errorf("bad API body %s", body)
		}
		switch reqs[0]["a"] {
		case "f":
			f.apiF.Add(1)
			rootKey := make([]byte, 16)
			nodes := []map[string]any{{
				"h": "ROOT0000", "p": "PARENT00", "u": "U", "t": 1, "ts": 1,
				"a": encryptAttrs(rootKey, "fake mod folder"), "k": "ROOT0000:" + b64(ecbEncrypt(link.Key, rootKey)),
			}}
			for _, ff := range f.files {
				nodes = append(nodes, map[string]any{
					"h": ff.handle, "p": "ROOT0000", "u": "U", "t": 0, "s": len(ff.cipher), "ts": ff.ts,
					"a": encryptAttrs(ff.key, ff.name), "k": "ROOT0000:" + b64(ecbEncrypt(link.Key, ff.key)),
				})
			}
			json.NewEncoder(w).Encode([]any{map[string]any{"f": nodes}})
		case "g":
			f.apiG.Add(1)
			h := reqs[0]["n"].(string)
			ff := f.files[h]
			if ff == nil {
				w.Write([]byte("[-9]"))
				return
			}
			json.NewEncoder(w).Encode([]any{map[string]any{"g": f.storage.URL + "/" + h, "s": len(ff.cipher)}})
		}
	}))
	megaAPIURL = f.api.URL
	t.Cleanup(func() { f.api.Close(); f.storage.Close() })
	return f
}

func TestMegaRangerMatchesPlaintext(t *testing.T) {
	plain := make([]byte, 5000)
	rand.Read(plain)
	ff := &fakeFile{handle: "H", key: make([]byte, 32)}
	rand.Read(ff.key)
	ff.cipher = encryptContent(ff.key, plain)
	link, _ := parseMegaFolderLink("https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg")
	fm := newFakeMega(t, link, ff)

	r, err := newMegaRanger(fm.storage.URL+"/H", len(plain), ff.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]int{{0, 0}, {0, 15}, {1, 1}, {15, 16}, {17, 4000}, {4999, 4999}, {4990, 4999}, {1234, 1234 + 777}} {
		got, err := r.Range(c[0], c[1])
		if err != nil || !bytes.Equal(got, plain[c[0]:c[1]+1]) {
			t.Errorf("range %v: err=%v equal=%v", c, err, err == nil && bytes.Equal(got, plain[c[0]:c[1]+1]))
		}
	}
	tail, err := r.Suffix(100)
	if err != nil || tail.total != 5000 || tail.start != 4900 || !bytes.Equal(tail.data, plain[4900:]) {
		t.Errorf("suffix: %v %+v", err, tail)
	}
	if _, err := r.Range(10, 5000); err == nil {
		t.Error("out-of-bounds range accepted")
	}
	// A short answer (e.g. different end-offset semantics) must be an error, not silent corruption.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("abc")) }))
	defer short.Close()
	r2, _ := newMegaRanger(short.URL+"/H", 5000, ff.key)
	if _, err := r2.Range(0, 99); err == nil {
		t.Error("short read accepted")
	}
}

func writeConfig(t *testing.T, dir string, links ...string) string {
	t.Helper()
	data, _ := json.Marshal(links)
	path := filepath.Join(dir, "config", "mega.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildMegaIndexesEndToEnd(t *testing.T) {
	linkStr := "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg"
	link, _ := parseMegaFolderLink(linkStr)
	small := newFakeFile(t, "AAAAAAAA", "Small.zipmod", 1700000000, makeZipmod(t, "com.test.small", "1.0", false))
	big := newFakeFile(t, "BBBBBBBB", "Big.zipmod", 1700000100, makeZipmod(t, "com.test.big", "2.1", true))
	dup := newFakeFile(t, "CCCCCCCC", "Big (older).zipmod", 1600000000, makeZipmod(t, "com.test.big", "1.9", false))
	junk := newFakeFile(t, "DDDDDDDD", "readme.txt", 1700000200, []byte("not a mod"))
	notZip := newFakeFile(t, "EEEEEEEE", "Broken.zipmod", 1700000300, []byte("this is not a zip file at all"))
	fm := newFakeMega(t, link, small, big, dup, junk, notZip)

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeConfig(t, dir, linkStr, linkStr+"/file/AAAAAAAA"), // the second is the same folder
		indicesDir: filepath.Join(dir, "indices"), cacheDir: filepath.Join(dir, "cache"),
		maxURLs: 4, workers: 3,
	}
	indexPath := filepath.Join(dir, "indices", "mega", "fake mod folder.json")
	cachePath := filepath.Join(dir, "cache", "mega", "fake mod folder.json")

	// Leftovers of a folder that is not in the config (and a non-json file that must be left alone).
	for _, f := range []string{
		filepath.Join(dir, "indices", "mega", "removed folder.json"),
		filepath.Join(dir, "cache", "mega", "removed folder.json"),
		filepath.Join(dir, "indices", "mega", "notes.txt"),
	} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("{}"), 0o644)
	}

	if newGuids, failures := buildMegaIndexes(opt); newGuids != 2 || failures != 0 {
		t.Fatalf("first run: newGuids=%d failures=%d", newGuids, failures)
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var idx modpackIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Count != 2 {
		t.Errorf("index header: %+v", idx)
	}
	want := map[string][]string{
		"com.test.small": {link.fileURL("AAAAAAAA")},
		"com.test.big":   {link.fileURL("BBBBBBBB"), link.fileURL("CCCCCCCC")}, // newest version first
	}
	for g, urls := range want {
		if strings.Join(idx.Guids[g], ",") != strings.Join(urls, ",") {
			t.Errorf("%s: got %v want %v", g, idx.Guids[g], urls)
		}
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Errorf("per-folder cache missing: %v", err)
	}
	if fm.apiF.Load() != 1 {
		t.Errorf("a folder listed twice in the config was listed %d times", fm.apiF.Load())
	}
	if fm.apiG.Load() != 4 { // 4 zipmod-named files probed; readme.txt is ignored
		t.Errorf("first run probed %d files, want 4", fm.apiG.Load())
	}
	for _, gone := range []string{
		filepath.Join(dir, "indices", "mega", "removed folder.json"),
		filepath.Join(dir, "cache", "mega", "removed folder.json"),
	} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("stale file not removed: %s", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "indices", "mega", "notes.txt")); err != nil {
		t.Error("a non-json file was deleted")
	}

	// Second run: nothing changed, so nothing is probed and nothing is downloaded.
	gets, gcalls := fm.gets.Load(), fm.apiG.Load()
	if newGuids, failures := buildMegaIndexes(opt); newGuids != 0 || failures != 0 {
		t.Fatalf("second run: newGuids=%d failures=%d", newGuids, failures)
	}
	if fm.gets.Load() != gets || fm.apiG.Load() != gcalls {
		t.Errorf("second run hit Mega again (gets %d->%d, g calls %d->%d)", gets, fm.gets.Load(), gcalls, fm.apiG.Load())
	}

	// A re-uploaded file (new date) is probed again; the others are not.
	small.ts += 60
	small.cipher = encryptContent(small.key, makeZipmod(t, "com.test.small", "1.1", false))
	gcalls = fm.apiG.Load()
	if _, failures := buildMegaIndexes(opt); failures != 0 {
		t.Fatal("unexpected failure")
	}
	if fm.apiG.Load() != gcalls+1 {
		t.Errorf("expected exactly one re-probe, got %d", fm.apiG.Load()-gcalls)
	}
	cache := loadCache(cachePath)
	if e := cache[link.fileURL("AAAAAAAA")]; e.Version != "1.1" {
		t.Errorf("cache not updated: %+v", e)
	}
	if e := cache[link.fileURL("EEEEEEEE")]; e.Error == "" || e.Guid != "" {
		t.Errorf("broken zipmod should be recorded as unindexable: %+v", e)
	}
}

// A bad entry in the config must not stop the good ones, must be counted as a failure, and
// must stop the stale-file cleanup (a folder that failed has an unknown name).
func TestBuildMegaIndexesOneBadLink(t *testing.T) {
	linkStr := "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg"
	link, _ := parseMegaFolderLink(linkStr)
	small := newFakeFile(t, "AAAAAAAA", "Small.zipmod", 1700000000, makeZipmod(t, "com.test.small", "1.0", false))
	newFakeMega(t, link, small)

	dir := t.TempDir()
	opt := hostedOptions{
		configPath: writeConfig(t, dir, "https://example.com/not-mega", linkStr),
		indicesDir: filepath.Join(dir, "indices"), cacheDir: filepath.Join(dir, "cache"),
		maxURLs: 4, workers: 2,
	}
	stale := filepath.Join(dir, "indices", "mega", "maybe the failed one.json")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("{}"), 0o644)

	newGuids, failures := buildMegaIndexes(opt)
	if newGuids != 1 || failures != 1 {
		t.Errorf("newGuids=%d failures=%d, want 1 and 1", newGuids, failures)
	}
	if _, err := os.Stat(filepath.Join(dir, "indices", "mega", "fake mod folder.json")); err != nil {
		t.Errorf("the good folder was not indexed: %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Error("cleanup ran although a folder failed")
	}
}

func TestMegaConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, " https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg ", "", "https://mega.nz/folder/AAAAAAAA#Gu2wMS8oer6bISpDzVdBWg")
	links, err := loadLinkConfig(path)
	if err != nil || len(links) != 2 || links[0] != "https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg" {
		t.Errorf("links=%v err=%v", links, err)
	}
	if _, err := loadLinkConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing config accepted")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"links": []}`), 0o644)
	if _, err := loadLinkConfig(bad); err == nil {
		t.Error("an object instead of an array was accepted")
	}
	// An empty list is fine: nothing to do, and nothing deleted.
	empty := writeConfig(t, t.TempDir())
	if n, f := buildMegaIndexes(hostedOptions{configPath: empty}); n != 0 || f != 0 {
		t.Errorf("empty config: %d %d", n, f)
	}
	if _, f := buildMegaIndexes(hostedOptions{configPath: filepath.Join(dir, "missing.json")}); f != 1 {
		t.Errorf("missing config should count as a failure, got %d", f)
	}
}

func TestMegaFileNames(t *testing.T) {
	for in, want := range map[string]string{
		"anse mod":              "anse mod",
		"a/b\\c:d*e?f\"g<h>i|j": "a_b_c_d_e_f_g_h_i_j",
		"  .hidden.  ":          "hidden",
		"纸鸢 mods":               "纸鸢 mods",
		"":                      "",
		"...":                   "",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	if long := safeFileName(strings.Repeat("x", 300)); len(long) != 100 {
		t.Errorf("long name not shortened: %d", len(long))
	}

	used := map[string]string{}
	if got := uniqueFileBase("Mods", "HANDLE01", used); got != "Mods" {
		t.Errorf("first: %q", got)
	}
	if got := uniqueFileBase("Mods", "HANDLE01", used); got != "Mods" {
		t.Errorf("same folder again must keep its name: %q", got)
	}
	if got := uniqueFileBase("mods", "HANDLE02", used); got != "mods (HANDLE02)" {
		t.Errorf("collision (case-insensitive): %q", got)
	}
	if got := uniqueFileBase("", "HANDLE03", used); got != "HANDLE03" {
		t.Errorf("unnamed: %q", got)
	}
}

func TestMegaTailHandlingSmallFile(t *testing.T) {
	// Files smaller than the 256 KB tail window must work (Suffix is clamped to the size).
	z := makeZipmod(t, "com.test.tiny", "1.0", false)
	ff := newFakeFile(t, "TINYTINY", "t.zipmod", 1, z)
	link, _ := parseMegaFolderLink("https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg")
	fm := newFakeMega(t, link, ff)
	r, _ := newMegaRanger(fm.storage.URL+"/TINYTINY", len(z), ff.key)
	info, err := readManifest(r)
	if err != nil || info.Guid != "com.test.tiny" || info.Version != "1.0" {
		t.Fatalf("%+v %v", info, err)
	}
}
