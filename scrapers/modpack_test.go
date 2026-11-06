package scrapers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestModpackCategoriesAreTheUnionOfBothGames(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range modpackCategories {
		if seen[c] {
			t.Errorf("category listed twice: %q", c)
		}
		seen[c] = true
	}
	for _, c := range append(append([]string{}, kkCategories...), kksCategories...) {
		if !seen[c] {
			t.Errorf("category missing from the combined list: %q", c)
		}
	}
	if len(seen) != len(modpackCategories) {
		t.Error("duplicates in the combined list")
	}
	if modpackCategories[0] != mainCategory {
		t.Errorf("the main folder must rank first, got %q", modpackCategories[0])
	}
	if modpackCategories[len(modpackCategories)-1] != studioCategory {
		t.Errorf("the studio folder must rank last, got %q", modpackCategories[len(modpackCategories)-1])
	}
}

func TestMergeCategories(t *testing.T) {
	got := mergeCategories([]string{"main", "a", studioCategory}, []string{"main", "b", "a", studioCategory})
	want := []string{"main", "a", "b", studioCategory}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// One index holds mods from KK-only and KKS-only folders, and ranks them like before.
func TestCombinedIndexCoversBothGames(t *testing.T) {
	const base = "https://sideload.example/download/KKEC/"
	u := func(category, file string) string { return base + category + "/" + file }
	entries := map[string]cacheEntry{
		u("Sideloader%20Modpack", "main.zipmod"):                      {Mtime: "2024-01-01 00:00", Size: "1M", Guid: "both", Version: "1.0"},
		u("Sideloader%20Modpack%20-%20Exclusive%20KK", "kk.zipmod"):   {Mtime: "2024-01-02 00:00", Size: "1M", Guid: "kk.only", Version: "1.0"},
		u("Sideloader%20Modpack%20-%20Exclusive%20KKS", "kks.zipmod"): {Mtime: "2024-01-03 00:00", Size: "1M", Guid: "kks.only", Version: "1.0"},
		u("Sideloader%20Modpack%20-%20Studio", "both.zipmod"):         {Mtime: "2024-06-01 00:00", Size: "2M", Guid: "both", Version: "9.0"},
		u("Sideloader%20Modpack%20-%20Bleeding%20Edge", "x.zipmod"):   {Mtime: "2024-06-01 00:00", Size: "1M", Guid: "not.indexed", Version: "1.0"},
		u("Sideloader%20Modpack%20-%20Fixes", "broken.zipmod"):        {Mtime: "2024-06-01 00:00", Size: "1M", Error: "no manifest"},
	}
	index := buildGameIndex(entries, modpackCategories, base, 4)

	if len(index) != 3 {
		t.Errorf("want 3 guids (both, kk.only, kks.only), got %v", index)
	}
	for _, g := range []string{"kk.only", "kks.only", "both"} {
		if len(index[g]) == 0 {
			t.Errorf("%s missing", g)
		}
	}
	if _, ok := index["not.indexed"]; ok {
		t.Error("a category outside both lists was indexed")
	}
	// The main folder outranks studio even though studio holds the newer version.
	if got := index["both"]; len(got) != 2 || got[0] != u("Sideloader%20Modpack", "main.zipmod") {
		t.Errorf("ranking: %v", got)
	}
}

func TestModpackIndexFileIsRewrittenOnlyOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "indices", modpackIndexName)
	index := map[string][]string{"a": {"https://x/a.zipmod"}}
	if wrote, err := writeIndex(path, index); err != nil || !wrote {
		t.Fatalf("first write: %v %v", wrote, err)
	}
	if wrote, _ := writeIndex(path, index); wrote {
		t.Error("rewritten although nothing changed")
	}
	data, _ := os.ReadFile(path)
	var doc modpackIndexFile
	if err := json.Unmarshal(data, &doc); err != nil || doc.Count != 1 {
		t.Errorf("%+v %v", doc, err)
	}
}

// An index written by an older version still has "game_type"; it is rewritten once to drop it,
// even though the GUIDs are unchanged.
func TestWriteIndexDropsLegacyGameType(t *testing.T) {
	path := filepath.Join(t.TempDir(), modpackIndexName)
	legacy := `{"generated":"2025-01-01T00:00:00","game_type":"modpack","count":1,"guids":{"a":["https://x/a.zipmod"]}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	index := map[string][]string{"a": {"https://x/a.zipmod"}}
	if wrote, err := writeIndex(path, index); err != nil || !wrote {
		t.Fatalf("legacy file not rewritten: %v %v", wrote, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "game_type") {
		t.Errorf("game_type still present:\n%s", data)
	}
	if wrote, _ := writeIndex(path, index); wrote {
		t.Error("rewritten again although nothing changed")
	}
}
