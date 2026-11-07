package scrapers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFakeMegaFiles fakes the Mega API for public file links ("p" calls, no folder handle).
func newFakeMegaFiles(t *testing.T, files ...*fakeFile) {
	t.Helper()
	byHandle := map[string]*fakeFile{}
	for _, ff := range files {
		byHandle[ff.handle] = ff
	}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		var s, e int
		if len(parts) != 2 || byHandle[parts[0]] == nil {
			http.NotFound(w, r)
			return
		}
		fmt.Sscanf(parts[1], "%d-%d", &s, &e)
		ff := byHandle[parts[0]]
		if s < 0 || e >= len(ff.cipher) || s > e {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Write(ff.cipher[s : e+1])
	}))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("n") != "" {
			t.Errorf("a public file call must not carry a folder handle: %s", r.URL)
		}
		var reqs []map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &reqs); err != nil || len(reqs) != 1 || reqs[0]["a"] != "g" {
			t.Errorf("bad API body %s", body)
			return
		}
		ff := byHandle[fmt.Sprint(reqs[0]["p"])]
		if ff == nil {
			w.Write([]byte("[-9]"))
			return
		}
		json.NewEncoder(w).Encode([]any{map[string]any{
			"g": storage.URL + "/" + ff.handle, "s": len(ff.cipher), "at": encryptAttrs(ff.key, ff.name),
		}})
	}))
	megaAPIURL = api.URL
	t.Cleanup(func() { api.Close(); storage.Close() })
}

func fileLink(ff *fakeFile) string {
	return "https://mega.nz/file/" + ff.handle + "#" + b64(ff.key)
}

func TestMegaFileLinks(t *testing.T) {
	good := newFakeFile(t, "GOOD0001", "Good.zipmod", 0, makeZipmod(t, "com.test.single", "1.0", false))
	card := newFakeFile(t, "CARD0001", "card.png", 0, []byte("png"))
	broken := newFakeFile(t, "BROK0001", "Broken.zipmod", 0, []byte("this is not a zip file at all"))
	manualPng := newFakeFile(t, "MANU0001", "keepme.png", 0, []byte("png"))
	gone := newFakeFile(t, "GONE0001", "Gone.zipmod", 0, makeZipmod(t, "com.test.gone", "1.0", false))
	newFakeMegaFiles(t, good, card, broken, manualPng) // "gone" is not served: Mega answers -9

	l, err := parseMegaFileLink(fileLink(good))
	if err != nil || l.Handle != "GOOD0001" || len(l.Key) != 32 {
		t.Fatalf("parse: %+v %v", l, err)
	}
	if _, err := parseMegaFileLink("https://mega.nz/folder/HM0FSAYC#Gu2wMS8oer6bISpDzVdBWg"); err == nil {
		t.Error("a folder link was accepted as a file link")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config", "mega.json")
	cfgJSON := fmt.Sprintf(`{"groups":[
	 {"name":"manual","links":[%q]},
	 {"name":"kknow.cc","search":"https://kknow.cc/search/1/?q=x","links":[%q,%q,%q,%q,%q]}]}`,
		fileLink(manualPng), fileLink(good), fileLink(card), fileLink(broken), fileLink(gone), fileLink(good)+"/")
	writeGrouped(t, path, cfgJSON)
	opt := hostedOptions{configPath: path, indicesDir: filepath.Join(dir, "i"), cacheDir: filepath.Join(dir, "c"), maxURLs: 4, workers: 2}

	newGuids, failures := buildMegaIndexes(opt)
	// The png in the hand-maintained group is not a mod: that is an error there, not a purge.
	if failures != 1 || newGuids != 1 {
		t.Fatalf("newGuids=%d failures=%d, want 1 and 1", newGuids, failures)
	}
	cfg, _ := loadHostedConfig(path)
	if len(cfg.Groups[0].Links) != 1 {
		t.Errorf("manual group lost a link: %v", cfg.Groups[0].Links)
	}
	if got := cfg.Groups[1].Links; len(got) != 1 || got[0] != fileLink(good) {
		t.Errorf("scraped group should keep only the good mod (duplicate, card, broken zip and vanished file purged): %v", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "i", "mega", singleFilesName+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Guids map[string][]string `json:"guids"`
	}
	json.Unmarshal(data, &idx)
	if urls := idx.Guids["com.test.single"]; len(urls) != 1 || urls[0] != fileLink(good) {
		t.Errorf("index: %s", data)
	}

	// Second run is stable: nothing new, nothing more purged.
	if n, f := buildMegaIndexes(opt); n != 0 || f != 1 {
		t.Errorf("second run: newGuids=%d failures=%d", n, f)
	}
}

func TestDriveFileLinks(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "testkey")
	d := newFakeDrive(t)
	const (
		good   = "1GoodGoodGoodGoodGoodGoodGood01"
		card   = "1CardCardCardCardCardCardCard01"
		broken = "1BrokBrokBrokBrokBrokBrokBrok01"
		gone   = "1GoneGoneGoneGoneGoneGoneGone01"
	)
	d.addFile("x", &fakeDriveFile{id: good, name: "Good.zipmod", content: makeZipmod(t, "com.test.dgood", "1.0", false)})
	d.addFile("x", &fakeDriveFile{id: card, name: "card.png", content: []byte("png")})
	d.addFile("x", &fakeDriveFile{id: broken, name: "Broken.zip", content: []byte("this is not a zip file at all")})
	link := func(id string) string { return "https://drive.google.com/file/d/" + id + "/view?usp=sharing" }

	if id, err := parseDriveFileLink("https://drive.google.com/uc?export=download&id=" + good); err != nil || id != good {
		t.Errorf("uc link: %q %v", id, err)
	}
	if _, err := parseDriveFileLink("https://drive.google.com/drive/folders/" + good); err == nil {
		t.Error("a folder link was accepted as a file link")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config", "drive.json")
	writeGrouped(t, path, fmt.Sprintf(`{"groups":[{"name":"kknow.cc","search":"https://kknow.cc/search/?q=x","links":[%q,%q,%q,%q,%q]}]}`,
		link(good), link(card), link(broken), link(gone), "https://drive.google.com/file/d/"+good+"/view"))
	newGuids, failures := buildHostedIndexes(hostedOptions{configPath: path, indicesDir: filepath.Join(dir, "i"), cacheDir: filepath.Join(dir, "c"), maxURLs: 4, workers: 2}, driveProvider{})
	if newGuids != 1 || failures != 0 {
		t.Fatalf("newGuids=%d failures=%d", newGuids, failures)
	}
	cfg, _ := loadHostedConfig(path)
	if got := cfg.Groups[0].Links; len(got) != 1 || got[0] != link(good) {
		t.Errorf("links: %v (card, broken zip, vanished file and duplicate should be gone)", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "i", "drive", singleFilesName+".json"))
	if err != nil || !strings.Contains(string(data), "com.test.dgood") || !strings.Contains(string(data), "https://drive.google.com/file/d/"+good+"/view") {
		t.Errorf("index: %s %v", data, err)
	}
}
