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
	newMods := scrapers.ScrapeMods()
	scrapers.RunModpack(nil)

	commitChanges(fmt.Sprintf("Update indices (%d new mods)", newMods))
}
