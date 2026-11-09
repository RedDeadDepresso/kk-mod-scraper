# kkafio-mod-index
## Sources and files

| Source | Scraper | Config | Index | Cache |
|---|---|---|---|---|
| Telegram archive (via koikatsucards.com) | `scrapers/telegram.go` | - | `data/telegram.json` | - |
| BetterRepack | `scrapers/modpack.go` | - | `data/modpack.json` (KK and KKS folders together) | `cache/modpack.json` |
| Mega folders | `scrapers/mega.go` | `config/mega.json` | `data/mega/<folder name>.json` | `cache/mega/<folder name>.json` |
| Google Drive folders | `scrapers/drive.go` | `config/drive.json` | `data/drive/<folder name>.json` | `cache/drive/<folder name>.json` |
| Pixeldrain lists and files | `scrapers/pixeldrain.go` | `config/pixeldrain.json` | `data/pixeldrain/<list title>.json` | `cache/pixeldrain/<list title>.json` |

### Mega, Google Drive and Pixeldrain folders

`config/mega.json` and `config/drive.json` hold public folder links, divided in groups, one
per place they came from:

```json
{
  "groups": [
    { "name": "manual", "links": ["https://mega.nz/folder/<id>#<key>"] },
    { "name": "kknow.cc",
      "search": "https://kknow.cc/search/4583029/?q=https%3A%2F%2Fmega.nz&o=date",
      "newest_post": { "id": "post-93888", "url": "https://kknow.cc/...", "time": "2026-11-06T16:37:43Z" },
      "links": ["https://mega.nz/folder/<id>#<key>"] }
  ]
}
```

(Drive folders look like `https://drive.google.com/drive/folders/<id>`; Pixeldrain lists like
`https://pixeldrain.com/l/<id>` (or `https://pixeldrain.com/d/<id>` for file-system shares). A group's `links` may
also hold single files: `https://mega.nz/file/<id>#<key>`,
`https://drive.google.com/file/d/<id>/view` and `https://pixeldrain.com/u/<id>`. The old format, a plain array of links, is still
read, as one group.)

Single-file links are looked at with one cheap API call (name and size); only `.zipmod` and
`.zip` files count. All of a config's file links are indexed together, in
`data/<kind>/(single files).json`, instead of one index per file.

- A group **without** `search` is yours: links are never removed automatically (except exact
  duplicates of a folder listed earlier in the file).
- A group **with** `search` is filled by the forum scraper (`scrapers/forum.go`, runs first in
  every run): it runs that forum search, reads the result pages newest first until it reaches
  `newest_post` (the newest post of the previous run), opens each new post, and adds the Mega
  folder / Drive folder / Pixeldrain list links and single-file links in it. Links in quotes are ignored. Add another site or query by adding another group.
  The first run has no `newest_post` and reads everything the search returns (`-max-pages`,
  default 30); later runs only read what is new. If a post can't be read, `newest_post`
  doesn't move and the run fails after committing the rest, so nothing is skipped.
- When a folder of such a group is indexed and turns out to be gone, empty, or to contain no
  mods (no `.zipmod`/`.zip`, or none that has a `manifest.xml`; for a file link: not a zipmod,
  or not readable as one), its link is purged from the
  config. Only "not found" counts as gone: network errors, rate limits, a missing API key and Google Drive
  403 (Forbidden) answers never purge anything. A file that answers 403 keeps its link and is
  tried again on every run.
- Forum flags: `go run . ` uses defaults; `-max-pages`, `-delay` (pause between requests,
  default 1.5s), `-mega-config`, `-drive-config`, `-pixeldrain-config` (via `scrapers.RunForum(args)`). If the forum
  doesn't let guests search, set the `KKNOW_COOKIE` environment variable / repository secret to
  the `Cookie` header of a logged-in browser session (sent to kknow.cc only).

Add a link to a group to index another folder; remove one and its files are deleted on the next fully
successful run. Each folder gets an index and a cache named after the folder as it is called
on the host (`<name> (<id>)` if two folders share a name). One broken folder doesn't stop the
others, but makes the run fail after everything else has been committed. Only the zip tail and
`manifest.xml` of each new or changed file are downloaded (ranged reads), never whole files.
Only `.zipmod` and `.zip` files are looked at.

- **Mega**: files are encrypted, but the folder link carries the key and AES-CTR can be
  decrypted from any offset. No account needed.
- **Google Drive**: uses the Drive API, which needs a free API key, and folders must be shared
  as "Anyone with the link". Create the key in the
  [Google Cloud Console](https://console.cloud.google.com/apis/credentials) (new project ->
  enable "Google Drive API" -> Create credentials -> API key; restricting it to the Drive API is
  a good idea), then add it as a repository secret named `GOOGLE_API_KEY`
  (Settings -> Secrets and variables -> Actions). To run locally:
  `GOOGLE_API_KEY=... go run .`. The key is sent in a request header and never appears in
  URLs, logs or indices.

- **Pixeldrain**: files are not encrypted and the public API honours Range requests, so the
  same ranged reads work. No account needed: `/api/list/<id>` lists a list, `/api/file/<id>/info`
  describes a single file. `config/pixeldrain.json` ships with a `kknow.cc` group that searches
  the forum for `https://pixeldrain.com`; links on the alternative domains (`pixeldrain.net`,
  `pixeldrain.dev`, `pixeldra.in`) are recognised in posts, but the forum only finds them if you add a
  group with its own search query for that domain. Pixeldrain sometimes throttles anonymous
  downloads with a captcha answer (403/429); such files are not purged and are retried on
  the next run. If it happens a lot, create an
  [API key](https://pixeldrain.com/user/api_keys) and set it as the `PIXELDRAIN_API_KEY`
  environment variable / repository secret (sent as HTTP Basic auth, never in URLs or logs).
  The newer file-system shares (`https://pixeldrain.com/d/<id>[/<path>]`) are supported too: they are
  read through `/api/filesystem/<path>?stat` and walked recursively, so a link to a share or to any
  sub-directory indexes every `.zipmod`/`.zip` below it (a `/d/` link to one file works as well).
  Their index URLs look like `https://pixeldrain.com/d/<id>/<path to file>`.

Flags (all optional) for all three: `-config`, `-data-dir`, `-cache-dir`, `-full`,
`-retry-failed`, `-max-urls`, `-workers`.

### Choosing which sources run

Sources: `koikatsucards`, `betterrepack`, `mega`, `drive`, `pixeldrain`. By default all run. To run only some,
pass `-sources mega,koikatsucards` (`go run . -sources ...`) or set the `SCRAPE_SOURCES`
environment variable. In GitHub Actions, set the repository variable `SCRAPE_SOURCES`
(Settings > Secrets and variables > Actions > Variables); the next run uses it, no commit needed.
Empty or unset means all. An unknown name stops the run, so a typo can't silently disable
everything. A disabled source is skipped completely: its files in `data/` and its config stay as
they are (and stay published), and the forum isn't searched for its links. To add a website, add
an entry to `sources()` in `main.go`.

### Published site

The index files live in `data/`; the Pages site publishes them under `/indices/` (so the URLs
are `<site>/indices/mod.json` and `<site>/indices/modpack.json`, as before).

The Pages workflow publishes `modpack.json` and a merged `mod.json`
built from `telegram.json`, every `data/mega/*.json`, every `data/drive/*.json` and every
`data/pixeldrain/*.json`:

```json
{ "com.example.mod": { "mega": ["https://mega.nz/folder/..."], "drive": ["https://drive.google.com/file/d/.../view"], "pixeldrain": ["https://pixeldrain.com/u/..."], "telegram": ["https://t.me/..."] } }
```

All four lists are always present (empty if the GUID isn't known from that source);
telegram links are newest first.
