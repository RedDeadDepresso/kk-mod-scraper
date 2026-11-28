// buildsite merges data/ into the mod.json published on GitHub Pages (see pages.yml).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"kk-mod-scraper/internal/siteindex"
)

func main() {
	data := flag.String("data", "data", "directory with the scraped indices")
	out := flag.String("out", "_site/indices/mod.json", "file to write")
	modpackOut := flag.String("modpack-out", "_site/indices/modpack.json", "file to write the modpack index to (empty = skip)")
	flag.Parse()

	idx, err := siteindex.Build(*data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build failed:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := idx.Write(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *modpackOut != "" {
		mp, err := siteindex.BuildModpack(*data)
		if err != nil {
			fmt.Fprintln(os.Stderr, "modpack build failed:", err)
			os.Exit(1)
		}
		if err := mp.Write(*modpackOut); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("Generated %d modpack entries\n", len(mp))
	}
	versions, unknown := 0, 0
	for _, list := range idx {
		versions += len(list)
		for _, e := range list {
			if e.Version == siteindex.Unknown {
				unknown++
			}
		}
	}
	fmt.Printf("Generated %d mod entries (%d versions in total, %d unknown)\n", len(idx), versions, unknown)
}
