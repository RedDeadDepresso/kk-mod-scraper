package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/joho/godotenv"

	"kk-mod-scraper/scrapers"
)

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// ---------- git ----------

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func commitChanges(message string) {
	if out, err := git("add", "."); err != nil {
		fatal("git add failed", "err", err, "output", out)
	}

	// `git commit` exits non-zero when there's nothing to commit,
	// so check first to avoid treating that as a failure.
	status, err := git("status", "--porcelain")
	if err != nil {
		fatal("git status failed", "err", err, "output", status)
	}
	if status == "" {
		slog.Info("Nothing to commit")
		return
	}

	if out, err := git("commit", "-m", message); err != nil {
		fatal("git commit failed", "err", err, "output", out)
	}
	slog.Info("Changes committed")

	if out, err := git("push"); err != nil {
		fatal("git push failed", "err", err, "output", out)
	}
	slog.Info("Changes pushed")
}

// source is one thing the scraper can update. To add a website, add an entry to sources():
// its name is what -sources / SCRAPE_SOURCES refers to. run returns the new mods it found and
// how many of its items failed.
type source struct {
	name string
	run  func() (newMods, failures int)
}

// Flags passed on to every scraper that supports them (all but koikatsucards).
var (
	retryFailedFlag = flag.Bool("retry-failed", false, "probe again the files recorded as not indexable")
	fullFlag        = flag.Bool("full", false, "ignore the caches and probe every file")
)

// runArgs turns the flags above into the command-line style args the scrapers take.
func runArgs() []string {
	var args []string
	if *retryFailedFlag {
		args = append(args, "-retry-failed")
	}
	if *fullFlag {
		args = append(args, "-full")
	}
	return args
}

func sources() []source {
	return []source{
		{"koikatsucards", func() (int, int) { return scrapers.ScrapeMods(), 0 }},
		{"betterrepack", func() (int, int) { return scrapers.RunModpack(runArgs()), 0 }},
		{"mega", func() (int, int) { return scrapers.RunMega(runArgs()) }},
		{"drive", func() (int, int) { return scrapers.RunDrive(runArgs()) }},
		{"pixeldrain", func() (int, int) { return scrapers.RunPixeldrain(runArgs()) }},
		{"dropbox", func() (int, int) { return scrapers.RunDropbox(runArgs()) }},
		{"mediafire", func() (int, int) { return scrapers.RunMediaFire(runArgs()) }},
		{"catbox", func() (int, int) { return scrapers.RunCatbox(runArgs()) }},
	}
}

// forumKinds are the sources whose links are collected from the forum (see scrapers/forum.go):
// the forum is only searched for the kinds of links whose source is enabled.
var forumKinds = []string{"mega", "drive", "pixeldrain", "dropbox", "mediafire", "catbox"}

// enabledSources decides which sources run: the -sources flag, else the SCRAPE_SOURCES
// environment variable (in GitHub Actions: the repository variable of that name), else all.
// The value is a comma separated list of source names, or "all". A typo is an error: silently
// running nothing would look like a working run.
func enabledSources(all []source) (map[string]bool, error) {
	spec := flag.String("sources", os.Getenv("SCRAPE_SOURCES"), "comma separated sources to run (default: all)")
	flag.Parse()
	known := map[string]bool{}
	var names []string
	for _, s := range all {
		known[s.name] = true
		names = append(names, s.name)
	}
	enabled := map[string]bool{}
	if strings.TrimSpace(*spec) == "" || strings.TrimSpace(*spec) == "all" {
		for _, n := range names {
			enabled[n] = true
		}
		return enabled, nil
	}
	for _, n := range strings.Split(*spec, ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n == "" {
			continue
		}
		if !known[n] {
			return nil, fmt.Errorf("unknown source %q (valid: %s)", n, strings.Join(names, ", "))
		}
		enabled[n] = true
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("no source selected (valid: %s)", strings.Join(names, ", "))
	}
	return enabled, nil
}

// loadDotEnv reads variables from a .env file in the working directory (or the file named by
// DOTENV_FILE). Variables already set in the real environment win over the file, so GitHub
// Actions secrets and shell exports keep working. A missing file is fine; an unreadable or
// malformed one is an error.
func loadDotEnv() {
	path := ".env"
	if p := os.Getenv("DOTENV_FILE"); p != "" {
		path = p
	}
	if err := godotenv.Load(path); err != nil {
		if os.IsNotExist(err) {
			return
		}
		fatal("Could not read env file", "file", path, "err", err)
	}
	slog.Info("Loaded environment file", "file", path)
}

func main() {
	loadDotEnv()
	all := sources()
	enabled, err := enabledSources(all)
	if err != nil {
		fatal("Bad source selection", "err", err)
	}
	var on, off []string
	for _, s := range all {
		if enabled[s.name] {
			on = append(on, s.name)
		} else {
			off = append(off, s.name)
		}
	}
	slog.Info("Sources", "enabled", strings.Join(on, ","), "disabled", strings.Join(off, ","))

	// First collect new folder links from the forum into config/*.json, so the folder
	// scrapers below index them in this same run.
	var kinds []string
	for _, k := range forumKinds {
		if enabled[k] {
			kinds = append(kinds, k)
		}
	}
	forumFailures := 0
	if len(kinds) > 0 {
		_, forumFailures = scrapers.RunForum([]string{"-kinds", strings.Join(kinds, ",")})
	}

	var (
		results  []result
		failures []any
	)
	for _, s := range all {
		if !enabled[s.name] {
			continue
		}
		n, f := s.run()
		results = append(results, result{s.name, n})
		if f > 0 {
			failures = append(failures, s.name, f)
		}
	}

	commitChanges(commitMessage(results))

	// Commit what did work first, then make the run visibly fail so a broken link (or a
	// missing API key) doesn't go unnoticed.
	if forumFailures > 0 {
		fatal("The forum could not be scraped completely (see the errors above)", "groups", forumFailures)
	}
	if len(failures) > 0 {
		fatal("Some items could not be processed (see the errors above)", failures...)
	}
}

type result struct {
	name string
	n    int
}

// commitMessage lists only the sources that actually produced new mods.
func commitMessage(results []result) string {
	var (
		parts []string
		names []string
		total int
	)
	for _, src := range results {
		if src.n > 0 {
			total += src.n
			names = append(names, src.name)
			parts = append(parts, fmt.Sprintf("%d from %s", src.n, src.name))
		}
	}
	switch len(parts) {
	case 0:
		return "Update data"
	case 1:
		return fmt.Sprintf("Update data (%d new mods from %s)", total, names[0])
	default:
		return fmt.Sprintf("Update data (%d new mods, %s)", total, strings.Join(parts, ", "))
	}
}