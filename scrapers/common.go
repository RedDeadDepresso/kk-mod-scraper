package scrapers

import (
	"log/slog"
	"os"
)

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
