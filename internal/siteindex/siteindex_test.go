package siteindex

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		"1": "1.0", "1.0": "1.0", "v1.0": "1.0", "V1": "1.0", "1.00": "1.0", "v1.00": "1.0",
		"1.0.0": "1.0", "v1.0.0": "1.0", "1.0.": "1.0", "1,00": "1.0", "1,0": "1.0",
		"1.0.1": "1.0.1", "1.01": "1.01", "1.1": "1.1", "v4": "4.0", "0": "0.0", "v0": "0.0",
		".5": "0.5", "v.5": "0.5", ".01": "0.01", "v.0.0": "0.0",
		"2.2b": "2.2b", "v5b": "5.0b", "1.0r1": "1.0r1", "2.7++": "2.7++", "3.01a": "3.01a",
		"version 2": "2.0", "Final 1.1": "1.1", "vFinal 1.1": "1.1", "Reset Beta 1.0": "1.0",
		"v2.0 正式发布版": "2.0", "1.1 and 2.1": "1.1", " 1.2 ": "1.2",
		"-": Unknown, "": Unknown, "null": Unknown, "NULL": Unknown, "(noVer)": Unknown,
		"unknown": Unknown, "Cinderella": Unknown, "vxiongben": Unknown, "1T.22.10": Unknown,
		"version B1.2": Unknown, "a pack": Unknown, "v【xiongben】Ganyu": Unknown,
	}
	for in, want := range cases {
		if got := NormalizeVersion(in); got != want {
			t.Errorf("NormalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	order := []string{"2.10", "2.2b", "2.2", "1.0.1", "1.0", "0.9", "0.0", Unknown} // newest first
	for i := range order {
		for j := range order {
			got := CompareVersions(order[i], order[j])
			if (i < j && got <= 0) || (i == j && got != 0) || (i > j && got >= 0) {
				t.Errorf("CompareVersions(%q, %q) = %d", order[i], order[j], got)
			}
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuild(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "mega", "a.json"), `{"guids":{"g":{"v1.0":["m1"],"1.0.0":["m1","m2"],"unknown":["m3"],"2.0":["m4"]}}}`)
	write(t, filepath.Join(d, "drive", "a.json"), `{"guids":{"g":{"1":["d1"]},"h":{"3":["d2"]}}}`)
	write(t, filepath.Join(d, "telegram.json"), `[{"Guid":"g","Version":"-","Link":"t2"},{"Guid":"g","Version":"v1","Link":"t1"},{"Guid":"","Version":"1","Link":"x"}]`)

	idx, err := Build(d)
	if err != nil {
		t.Fatal(err)
	}
	g := idx["g"]
	if len(g) != 3 {
		t.Fatalf("want 3 entries, got %+v", g)
	}
	if g[0].Version != "2.0" || g[1].Version != "1.0" || g[2].Version != Unknown {
		t.Fatalf("bad order: %+v", g)
	}
	if len(g[1].Mega) != 2 || g[1].Mega[0] != "m1" || g[1].Drive[0] != "d1" || g[1].Telegram[0] != "t1" {
		t.Errorf("bad 1.0 entry: %+v", g[1])
	}
	if g[2].Telegram[0] != "t2" || g[2].Mega[0] != "m3" || g[0].Gofile == nil {
		t.Errorf("bad unknown entry: %+v", g[2])
	}
	if len(idx["h"]) != 1 || idx["h"][0].Version != "3.0" || len(idx) != 2 {
		t.Errorf("bad h: %+v", idx)
	}
}

func TestBuildModpack(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "modpack.json"), `{"generated":"x","count":2,"guids":{
		"a":{"v1.0":["l1"],"1":["l1","l2"],"-":["l3"],"2.1":["l4"]},
		"b":{"1.0":["l5"]}}}`)
	idx, err := BuildModpack(d)
	if err != nil {
		t.Fatal(err)
	}
	a := idx["a"]
	if len(a) != 3 || a[0].Version != "2.1" || a[1].Version != "1.0" || a[2].Version != Unknown {
		t.Fatalf("bad a: %+v", a)
	}
	if len(a[1].Betterrepack) != 2 || a[1].Betterrepack[0] != "l1" || a[1].Betterrepack[1] != "l2" {
		t.Errorf("bad links: %+v", a[1])
	}
	if len(idx) != 2 || idx["b"][0].Betterrepack[0] != "l5" {
		t.Errorf("bad b: %+v", idx["b"])
	}
}
