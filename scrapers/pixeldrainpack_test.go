package scrapers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const packZipJSON = `{"size":2121270412,"children":{
"BiyocaSSR01-Base.zipmod":{"size":12607488},
"BiyocaSSR01.zipmod":{"size":40728124},
"GUID-Index-056.txt":{"size":14538},
"Koshmar_hair.zipmod":{"size":2137268},
"[2025.01.25]Curve_Slient_Voice.zipmod":{"size":18129351},
"[kyonko]Amber_hair.zipmod":{"size":124244},
"pama00_ACC.zip":{"size":12413130},
"Dup&Name.zipmod":{"size":10},
"Unknown Mod.zipmod":{"size":10},
"sub":{"children":{"[gazun]Raiden heel.zipmod":{"size":716695}}}
},"properties":["read_individual_files"]}`

func TestParseModListUnescapes(t *testing.T) {
	ml, err := parseModList("testdata/modlist_056.txt")
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := ml.lookup("Dup&Name.zipmod", []int{57}); !ok || g != "dup.in.057" {
		t.Errorf("pack 57: %q %v", g, ok)
	}
	if g, ok := ml.lookup("Dup&Name.zipmod", []int{56}); !ok || g != "dup.in.056" {
		t.Errorf("pack 56: %q %v", g, ok)
	}
	if _, ok := ml.lookup("Dup&Name.zipmod", nil); ok {
		t.Error("an ambiguous name must not be guessed")
	}
	if _, ok := ml.lookup("Unset.zipmod", nil); ok {
		t.Error("<not set> must be skipped")
	}
}

func TestMapZip(t *testing.T) {
	ml, _ := parseModList("testdata/modlist_056.txt")
	var entries []zipEntry
	raw := packZipJSON[strings.Index(packZipJSON, `"children":`)+len(`"children":`) : strings.LastIndex(packZipJSON, `,"properties"`)]
	if err := parseZipChildren([]byte(raw), "", &entries, 0); err != nil {
		t.Fatal(err)
	}
	idx, unmatched, packs := mapZip("CMFYyfJ3", entries, ml)
	if !reflect.DeepEqual(packs, []int{56}) {
		t.Errorf("packs = %v", packs)
	}
	want := map[string]string{
		"[moonlight]BiyocaSSR01-Base":   "https://pixeldrain.com/api/file/CMFYyfJ3/info/zip/BiyocaSSR01-Base.zipmod",
		"[moonlight]Curve_Slient_Voice": "https://pixeldrain.com/api/file/CMFYyfJ3/info/zip/%5B2025.01.25%5DCurve_Slient_Voice.zipmod",
		"[cha]pama00_acc":               "https://pixeldrain.com/api/file/CMFYyfJ3/info/zip/pama00_ACC.zip",
		"[gazun]Raiden heel":            "https://pixeldrain.com/api/file/CMFYyfJ3/info/zip/sub/%5Bgazun%5DRaiden%20heel.zipmod",
		"dup.in.056":                    "https://pixeldrain.com/api/file/CMFYyfJ3/info/zip/Dup&Name.zipmod",
	}
	for g, u := range want {
		if got := idx[g]; len(got) != 1 || got[0] != u {
			t.Errorf("%s: got %v want %s", g, got, u)
		}
	}
	if len(unmatched) != 1 || unmatched[0] != "Unknown Mod.zipmod" {
		t.Errorf("unmatched: %v", unmatched)
	}
}

func TestBuildPackIndex(t *testing.T) {
	zipCalls := 0
	date := "2026-11-01T10:00:00Z"
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/CMFYyfJ3/info":
			w.Write([]byte(`{"id":"CMFYyfJ3","name":"GUID-Index-056.zip","size":2121270412,"date_upload":"` + date + `"}`))
		case "/file/CMFYyfJ3/info/zip":
			zipCalls++
			w.Write([]byte(packZipJSON))
		case "/file/LhEoGfEk/info":
			w.Write([]byte(`{"id":"LhEoGfEk","name":"My_All_Missing_GUIDs.txt","size":1000,"date_upload":"` + date + `"}`))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()
	old := pixeldrainAPIBase
	pixeldrainAPIBase = srv.URL
	defer func() { pixeldrainAPIBase = old }()

	dir := t.TempDir()
	page := `<a href="https://pixeldrain.com/u/LhEoGfEk">x</a><img src="https://pixeldrain.com/api/file/LhEoGfEk/thumbnail">
<div data-url="https://pixeldrain.com/u/CMFYyfJ3"></div>`
	if err := os.WriteFile(filepath.Join(dir, "p1.htm"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out", "Forum Mod Packs.json")
	cache := filepath.Join(dir, "cache", "Forum Mod Packs.json")
	run := func(full bool) (int, int) {
		return buildPackIndex(dir, "testdata/modlist_056.txt", out, cache, full, 4)
	}

	n, f := run(false)
	if f != 0 || n == 0 || zipCalls != 1 {
		t.Fatalf("new=%d failures=%d zipCalls=%d", n, f, zipCalls)
	}
	if len(readIndexGuids(out)["[moonlight]Koshmar_hair"]) != 1 {
		t.Errorf("index: %v", readIndexGuids(out))
	}

	// Unchanged zip: served from the cache, no listing call.
	if n, f = run(false); f != 0 || n != 0 || zipCalls != 1 {
		t.Errorf("cached run: new=%d failures=%d zipCalls=%d", n, f, zipCalls)
	}
	// -full lists again.
	if _, f = run(true); f != 0 || zipCalls != 2 {
		t.Errorf("full run: failures=%d zipCalls=%d", f, zipCalls)
	}
	// Re-uploaded zip (new date): listed again.
	date = "2026-11-02T10:00:00Z"
	if _, f = run(false); f != 0 || zipCalls != 3 {
		t.Errorf("changed run: failures=%d zipCalls=%d", f, zipCalls)
	}

	// Host failing: failure counted, old entries and cache kept.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, f = run(false); f == 0 {
		t.Error("expected a failure")
	}
	if len(readIndexGuids(out)["[moonlight]Koshmar_hair"]) != 1 {
		t.Error("old entries were lost")
	}
	if len(loadPackCache(cache).Zips["CMFYyfJ3"].Entries) == 0 {
		t.Error("cache entry was lost")
	}
}

func TestParseViewerData(t *testing.T) {
	page, err := os.ReadFile("testdata/list_page.htm")
	if err != nil {
		t.Fatal(err)
	}
	title, files, err := parseViewerData(string(page))
	if err != nil {
		t.Fatal(err)
	}
	if title != "My New Album" || len(files) != 10 {
		t.Fatalf("title=%q files=%d", title, len(files))
	}
	if f := files[0]; f.ID != "X4swdWeG" || f.Name != "GUID-Index-013.zip" || f.Size != 1949317921 || f.DateUpload == "" {
		t.Errorf("first file: %+v", f)
	}
	if _, _, err := parseViewerData("<html>nothing</html>"); err == nil {
		t.Error("expected an error without viewer_data")
	}
}

func TestListLinks(t *testing.T) {
	page, _ := os.ReadFile("testdata/list_page.htm")
	zipCalls := 0
	listFails := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/l/txLCBLPr":
			if listFails {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Write(page)
		case "/file/X4swdWeG/info/zip":
			zipCalls++
			w.Write([]byte(packZipJSON))
		default:
			w.WriteHeader(http.StatusNotFound) // the list's other files are not part of this test
		}
	}))
	defer srv.Close()
	oldAPI, oldSite := pixeldrainAPIBase, pixeldrainSiteBase
	pixeldrainAPIBase, pixeldrainSiteBase = srv.URL, srv.URL
	defer func() { pixeldrainAPIBase, pixeldrainSiteBase = oldAPI, oldSite }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.htm"), []byte(`<a href="https://pixeldrain.com/l/txLCBLPr">album</a>`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, cache := filepath.Join(dir, "o", "i.json"), filepath.Join(dir, "c", "c.json")
	run := func() (int, int) { return buildPackIndex(dir, "testdata/modlist_056.txt", out, cache, false, 4) }

	if n, f := run(); f != 0 || n == 0 || zipCalls != 1 {
		t.Fatalf("new=%d failures=%d zipCalls=%d", n, f, zipCalls)
	}
	want := "https://pixeldrain.com/api/file/X4swdWeG/info/zip/%5B2025.01.25%5DCurve_Slient_Voice.zipmod"
	if got := readIndexGuids(out)["[moonlight]Curve_Slient_Voice"]; len(got) != 1 || got[0] != want {
		t.Errorf("got %v", got)
	}
	// Second run: the zip comes from the cache.
	if _, f := run(); f != 0 || zipCalls != 1 {
		t.Errorf("failures=%d zipCalls=%d", f, zipCalls)
	}
	// The list page is unreachable: counted as a failure, but the cached files keep the index whole.
	listFails = true
	if _, f := run(); f != 1 {
		t.Errorf("failures=%d, want 1", f)
	}
	if len(readIndexGuids(out)["[moonlight]Koshmar_hair"]) != 1 {
		t.Error("entries were lost while the list page was down")
	}
}
