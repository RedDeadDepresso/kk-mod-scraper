package scrapers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mangleTestList(t *testing.T) *modList {
	t.Helper()
	lines := []string{
		"056: 1, file_name: G11-常服初始版二版.zipmod, guid: [moonlight]G11-常服",
		"056: 2, file_name: [纸鸢]椿.zipmod, guid: zhiyuan.chun",
		"056: 3, file_name: [纸鸢]灯.zipmod, guid: zhiyuan.deng",
		"056: 4, file_name: [纸鸢]符华-月轮.zipmod, guid: zhiyuan.fuhua.moon",
		"056: 5, file_name: Plain.zipmod, guid: plain",
		"056: 6, file_name: サトノ.zipmod, guid: satono",
		"057: 1, file_name: G11-常服初始版二版.zipmod, guid: other.pack.G11",
	}
	p := filepath.Join(t.TempDir(), "All_Mod_List.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	ml, err := parseModList(p)
	if err != nil {
		t.Fatal(err)
	}
	return ml
}

func TestMangleBytes(t *testing.T) {
	// "始" in GBK is CA BC: valid UTF-8 (U+02BC), so it survives; the other bytes become one U+FFFD each.
	got := mangleBytes([]byte("G11-\xb3\xa3\xb7\xfe\xb3\xf5\xca\xbc\xb0\xe6\xb6\xfe\xb0\xe6.zipmod"))
	if want := "G11-\uFFFD\uFFFD\uFFFD\uFFFD\uFFFD\uFFFDʼ\uFFFD\uFFFD\uFFFD\uFFFD\uFFFD\uFFFD.zipmod"; got != want {
		t.Errorf("mangleBytes = %q, want %q", got, want)
	}
	// a real, valid UTF-8 name is left alone
	if got := mangleBytes([]byte("常服")); got != "常服" {
		t.Errorf("valid UTF-8 changed: %q", got)
	}
}

func TestLookupMangled(t *testing.T) {
	ml := mangleTestList(t)

	// strings as returned by Pixeldrain (generated independently of this code)
	cases := []struct {
		name  string
		packs []int
		guid  string
		ok    bool
	}{
		{"G11-������ʼ������.zipmod", []int{56}, "[moonlight]G11-常服", true},
		{"G11-������ʼ������.zipmod", []int{57}, "other.pack.G11", true},  // pack decides
		{"G11-������ʼ������.zipmod", []int{56, 57}, "", false},           // two packs, two GUIDs: not guessed
		{"G11-������ʼ������.zipmod", nil, "", false},                     // no pack known: no guess
		{"[ֽ��]����-����.zipmod", []int{56}, "zhiyuan.fuhua.moon", true}, // unique in its pack
		{"[ֽ��]��.zipmod", []int{56}, "", false},                         // 椿 and 灯 collapse to the same string
		{"�T�g�m.zipmod", []int{56}, "satono", true},                     // Shift-JIS
		{"[ֽ��]��ޱ��.zipmod", []int{56}, "", false},                      // not in the list
		{"Plain.zipmod", []int{56}, "plain", true},                       // untouched names still work
	}
	for _, c := range cases {
		g, ok := ml.lookup(c.name, c.packs)
		if g != c.guid || ok != c.ok {
			t.Errorf("lookup(%q, %v) = %q, %v; want %q, %v", c.name, c.packs, g, ok, c.guid, c.ok)
		}
	}
}

func TestMapZipMangled(t *testing.T) {
	ml := mangleTestList(t)
	entries := []zipEntry{
		{Path: "GUID-Index-056.txt", Size: 10},
		{Path: "G11-������ʼ������.zipmod", Size: 100},
		{Path: "[ֽ��]��.zipmod", Size: 100},
	}
	idx, unmatched, _ := mapZip("ZZZ", entries, ml)
	// The URL keeps the mangled name, as Pixeldrain's own URLs do.
	want := "https://pixeldrain.com/api/file/ZZZ/info/zip/G11-%EF%BF%BD%EF%BF%BD%EF%BF%BD%EF%BF%BD%EF%BF%BD%EF%BF%BD%CA%BC%EF%BF%BD%EF%BF%BD%EF%BF%BD%EF%BF%BD%EF%BF%BD%EF%BF%BD.zipmod"
	if got := idx["[moonlight]G11-常服"]; len(got) != 1 || got[0] != want {
		t.Errorf("got %v, want %s", got, want)
	}
	if len(unmatched) != 1 || unmatched[0] != "[ֽ��]��.zipmod" {
		t.Errorf("unmatched = %v", unmatched)
	}
}