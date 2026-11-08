package scrapers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

type Mod map[string]string

const (
	// TelegramIndexFile is where the koikatsucards.com / Telegram archive mod index is stored.
	TelegramIndexFile = "data/telegram.json"
	kkcBaseURL        = "https://koikatsucards.com"
	telegramPrefix    = "https://t.me/KK_archive_modlibrary/"
)

var modAttributes = []string{"Name", "Version", "Author", "Guid", "File"}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func isModAttribute(s string) bool {
	for _, a := range modAttributes {
		if a == s {
			return true
		}
	}
	return false
}

// ---------- persistence ----------

func loadPreviousMods() []Mod {
	data, err := os.ReadFile(TelegramIndexFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("Previous data doesn't exist")
			return []Mod{}
		}
		fatal("Failed to load previous version", "err", err)
	}

	var mods []Mod
	if err := json.Unmarshal(data, &mods); err != nil {
		fatal("Failed to load previous version", "err", err)
	}
	return mods
}

func saveMods(newMods, prevMods []Mod) {
	mods := append(append([]Mod{}, newMods...), prevMods...)

	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false) // keep <, >, & unescaped
	enc.SetIndent("", "  ")
	if err := enc.Encode(mods); err != nil {
		fatal("Failed to save mods", "err", err)
	}

	if err := os.MkdirAll(filepath.Dir(TelegramIndexFile), 0o755); err != nil {
		fatal("Failed to save mods", "err", err)
	}
	if err := os.WriteFile(TelegramIndexFile, []byte(sb.String()), 0o644); err != nil {
		fatal("Failed to save mods", "err", err)
	}
	slog.Info("Mods saved successfully")
}

// ---------- parsing ----------

func outerHTML(s *goquery.Selection) string {
	h, err := goquery.OuterHtml(s)
	if err != nil {
		return ""
	}
	return h
}

// nodeText returns the text content of a node (like BeautifulSoup's .text).
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

func getMod(anchor *goquery.Selection) Mod {
	mod := Mod{}
	parentDiv := anchor.Find("div.mod-library-item-grid").First()

	parentDiv.Find("div").Each(func(_ int, div *goquery.Selection) {
		attrElm := div.Find("span.eyebrow").First()
		if attrElm.Length() == 0 {
			return
		}
		attribute := nodeText(attrElm.Nodes[0])
		if !isModAttribute(attribute) {
			return
		}

		// First child node (element or text) that isn't the eyebrow span.
		for c := div.Nodes[0].FirstChild; c != nil; c = c.NextSibling {
			if c == attrElm.Nodes[0] {
				continue
			}
			mod[attribute] = nodeText(c)
			break
		}
	})

	href, _ := anchor.Attr("href")
	if href == "" || mod["Guid"] == "" {
		slog.Warn("Skipping mod with missing link and/or guid", "name", mod["Name"], "link", href)
		return nil
	}

	// The listing now links to a page on the site; the Telegram link
	// lives on that page.
	mod["Link"] = fetchTelegramLink(href)
	return mod
}

// ---------- fetching ----------

func fetchPage(page int) *goquery.Document {
	pageURL := fmt.Sprintf("%s/mod_library?page=%d", kkcBaseURL, page)
	res, err := httpClient.Get(pageURL)
	if err != nil {
		fatal("Request failed", "url", pageURL, "err", err)
	}
	defer res.Body.Close()

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		fatal("Failed to parse HTML", "url", pageURL, "err", err)
	}
	return doc
}

// fetchTelegramLink joins the relative href (e.g. "/mod_library/6177-slug") to
// the base URL, requests that page and returns the href of the first
// a.link-pill pointing at the Telegram archive. Exits if none is found.
func fetchTelegramLink(href string) string {
	base, err := url.Parse(kkcBaseURL)
	if err != nil {
		fatal("Invalid base URL", "url", kkcBaseURL, "err", err)
	}
	ref, err := url.Parse(href)
	if err != nil {
		fatal("Invalid mod href", "href", href, "err", err)
	}
	pageURL := base.ResolveReference(ref).String()

	res, err := httpClient.Get(pageURL)
	if err != nil {
		fatal("Request failed", "url", pageURL, "err", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		fatal("Unexpected status code", "url", pageURL, "status", res.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		fatal("Failed to parse HTML", "url", pageURL, "err", err)
	}

	link := ""
	doc.Find("a.link-pill").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		if h, ok := a.Attr("href"); ok && strings.HasPrefix(h, telegramPrefix) {
			link = h
			return false
		}
		return true
	})
	if link == "" {
		fatal("Telegram link not found on mod page", "url", pageURL)
	}
	return link
}

// ---------- scrape ----------

// ScrapeMods scrapes koikatsucards.com for mods newer than the ones already in
// TelegramIndexFile, writes the updated index and returns how many mods were new.
func ScrapeMods() int {
	newMods := []Mod{}
	prevMods := loadPreviousMods()

	var endMod Mod
	if len(prevMods) > 0 {
		endMod = prevMods[0]
	}

	for page := 1; ; page++ {
		if (page-1)%10 == 0 {
			slog.Info(fmt.Sprintf("Processing pages %d-%d", page, page+9))
		}
		doc := fetchPage(page)
		anchors := doc.Find("a.mod-library-item-link")

		if anchors.Length() == 0 {
			slog.Info("No more mods found")
			saveMods(newMods, prevMods)
			return len(newMods)
		}

		reached := false
		anchors.EachWithBreak(func(_ int, a *goquery.Selection) bool {
			mod := getMod(a)
			if mod == nil {
				return true // skipped; keep going
			}
			if endMod != nil && reflect.DeepEqual(mod, endMod) {
				slog.Info("Previous start mod reached")
				reached = true
				return false
			}
			newMods = append(newMods, mod)
			return true
		})

		if reached {
			saveMods(newMods, prevMods)
			return len(newMods)
		}
	}
}
