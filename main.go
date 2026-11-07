package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

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

func main() {
	// First collect new folder links from the forum into config/*.json, so the folder
	// scrapers below index them in this same run.
	_, forumFailures := scrapers.RunForum(nil)

	kkcMods := scrapers.ScrapeMods()
	modpackMods := scrapers.RunModpack(nil)
	megaMods, megaFailures := scrapers.RunMega(nil)
	driveMods, driveFailures := scrapers.RunDrive(nil)

	commitChanges(commitMessage(kkcMods, modpackMods, megaMods, driveMods))

	// Commit what did work first, then make the run visibly fail so a broken link (or a
	// missing API key) doesn't go unnoticed.
	if forumFailures > 0 {
		fatal("The forum could not be scraped completely (see the errors above)", "groups", forumFailures)
	}
	if megaFailures+driveFailures > 0 {
		fatal("Some folders could not be indexed (see the errors above)", "mega", megaFailures, "drive", driveFailures)
	}
}

// commitMessage lists only the sources that actually produced new mods.
func commitMessage(kkcMods, modpackMods, megaMods, driveMods int) string {
	var (
		parts []string
		names []string
		total int
	)
	for _, src := range []struct {
		n    int
		name string
	}{{kkcMods, "koikatsucards"}, {modpackMods, "betterrepack"}, {megaMods, "mega"}, {driveMods, "drive"}} {
		if src.n > 0 {
			total += src.n
			names = append(names, src.name)
			parts = append(parts, fmt.Sprintf("%d from %s", src.n, src.name))
		}
	}
	switch len(parts) {
	case 0:
		return "Update indices"
	case 1:
		return fmt.Sprintf("Update indices (%d new mods from %s)", total, names[0])
	default:
		return fmt.Sprintf("Update indices (%d new mods, %s)", total, strings.Join(parts, ", "))
	}
}
