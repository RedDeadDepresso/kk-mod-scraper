# kkafio-mod-index
## Sources and files

| Source | Scraper | Config | Index | Cache |
|---|---|---|---|---|
| Telegram archive (via koikatsucards.com) | `scrapers/telegram.go` | - | `data/telegram.json` | - |
| BetterRepack | `scrapers/modpack.go` | - | `data/modpack.json` (KK and KKS folders together) | `cache/modpack.json` |
| Mega folders | `scrapers/mega.go` | `config/mega.json` | `data/mega/<folder name>.json` | `cache/mega/<folder name>.json` |
| Google Drive folders | `scrapers/drive.go` | `config/drive.json` | `data/drive/<folder name>.json` | `cache/drive/<folder name>.json` |
| Pixeldrain lists and files | `scrapers/pixeldrain.go` | `config/pixeldrain.json` | `data/pixeldrain/<list title>.json` | `cache/pixeldrain/<list title>.json` |
| Dropbox folders and files | `scrapers/dropbox.go` | `config/dropbox.json` | `data/dropbox/<folder name>.json` | `cache/dropbox/<folder name>.json` |
| MediaFire folders and files | `scrapers/mediafire.go` | `config/mediafire.json` | `data/mediafire/<folder name>.json` | `cache/mediafire/<folder name>.json` |
| Catbox albums and files | `scrapers/catbox.go` | `config/catbox.json` | `data/catbox/<album title>.json` | `cache/catbox/<album title>.json` |
| Gofile shares | `scrapers/gofile.go`, `scrapers/gofilewt.go` | `config/gofile.json` | `data/gofile/<folder name>.json` | `cache/gofile/<folder name>.json` |

### Mega, Google Drive, Pixeldrain, Dropbox, MediaFire, Catbox and Gofile folders

`config/mega.json`, `config/drive.json` and the other configs hold public folder links, divided in groups, one
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
`https://drive.google.com/file/d/<id>/view` and `https://pixeldrain.com/u/<id>`; MediaFire folders like `https://www.mediafire.com/folder/<key>` and files like
`https://www.mediafire.com/file/<key>`; Catbox albums like `https://catbox.moe/c/<id>` and files like
`https://files.catbox.moe/<id>.zip`. The old format, a plain array of links, is still
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
  default 1.5s), `-mega-config`, `-drive-config`, `-pixeldrain-config`, `-dropbox-config` (via `scrapers.RunForum(args)`). If the forum
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
  `GOOGLE_API_KEY=... go run .`, or put it in a `.env` file (see below). The key is sent in a request header and never appears in
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

- **Dropbox**: shared links (folders `https://www.dropbox.com/scl/fo/<id>/<key>?rlkey=...` or the
  older `/sh/<id>/<key>`, files `/scl/fi/<id>/<name>?rlkey=...` or `/s/<id>/<name>`). Dropbox's API
  refuses anonymous requests, so unlike the other sources this one **needs credentials**: create a
  free app at <https://www.dropbox.com/developers/apps> (scoped access, "App folder" is fine, no
  extra permissions needed, since the shared-link calls work on any public link), then set either
  `DROPBOX_REFRESH_TOKEN` + `DROPBOX_APP_KEY` + `DROPBOX_APP_SECRET` (recommended; the refresh
  token does not expire) or just `DROPBOX_ACCESS_TOKEN` (expires after about four hours, so only
  for trying it out) as environment variables / repository secrets. They are only sent in request
  headers, never in URLs, logs or indices. Folders are walked recursively through
  `files/list_folder` with the link as `shared_link`, and zips are read with ranged
  `sharing/get_shared_link_file` calls. `config/dropbox.json` ships with a `kknow.cc` group that
  searches the forum for `https://www.dropbox.com`. Only the `rlkey` of a link is kept. A file
  inside a folder has no link of its own, so its index URL goes through the folder link: `/sh/`
  links get the path appended, `/scl/fo/` links get the directory appended plus
  `?preview=<file>&rlkey=...`. Only "not found" answers purge a link; a bad token, rate limits
  and access-denied answers never do.

- **MediaFire**: uses MediaFire's public API (`folder/get_info`, `folder/get_content`,
  `file/get_info`), which needs no account or key for public content. Folders are walked
  recursively. A file's download link is read from its page (`https://www.mediafire.com/file/<key>`),
  resolved again if the server refuses it (the links expire), and zips are read with Range
  requests, so only a few KB per file are downloaded. `config/mediafire.json` ships with a
  `kknow.cc` group that searches the forum for `https://www.mediafire.com`. Password protected
  files are skipped. Only "unknown key" and 404 answers purge a link; captchas, rate limits and
  outages never do (those files are tried again next run). The URLs in the index are
  `https://www.mediafire.com/file/<key>`.

- **Catbox**: has no listing API, but serves files from `files.catbox.moe` with Range support, so
  zips are read the same way as elsewhere (a few KB per file). A file link is
  `https://files.catbox.moe/<id>.zip`; an album link is `https://catbox.moe/c/<id>`, whose page is
  read and its `files.catbox.moe` links are the album's files (the album's title names the index).
  A file's content never changes under its id, so each file is read once and remembered. The
  forum scraper only collects Catbox links to `.zip`/`.zipmod` files (forum posts are full of
  Catbox images), plus all albums. `config/catbox.json` ships with a `kknow.cc` group that
  searches the forum for `catbox.moe`. Only 404/410
  answers purge a link. Litterbox (temporary files) is not covered.

- **Gofile**: share links like `https://gofile.io/d/<code>` (the older `https://gofile.io/?c=<code>` also
  works); shares are walked recursively. Gofile has no anonymous listing: the scraper creates a guest
  account and signs each API request with an `X-Website-Token`, a SHA-256 over the user agent, language,
  account token, a 4-hour time slot and a secret. That secret is baked into Gofile's obfuscated script
  `https://gofile.io/js/wt.obf.js` and rotates, so it is **read out of that script** (the string table is
  RC4/base64-decoded without running any JavaScript; adapted from
  [HeapLeach](https://github.com/JohanLindvall/HeapLeach)). The extraction only answers when the whole
  script decodes and exactly one string has the shape of a secret; otherwise a pinned fallback is used,
  and the `GOFILE_SECRET` environment variable / repository secret overrides both. Gofile blocks addresses
  that sign badly, so the first refused signature **stops the Gofile run** (it is reported as a failure)
  rather than being repeated for every link. Files are read with ranged downloads from the storage
  servers (a few KB each). A file has no link of its own, so the index URL is the share's link plus the
  file's id as a fragment: `https://gofile.io/d/<code>#<file id>`. Password protected shares are
  skipped (and purged from scraped groups). Only "not found" and "expired" answers purge a link.
  `config/gofile.json` ships with a `kknow.cc` group that searches the forum for `https://gofile.io`.

Flags (all optional) for all of them: `-config`, `-data-dir`, `-cache-dir`, `-full`,
`-retry-failed`, `-max-urls`, `-workers`.

### Choosing which sources run

Sources: `koikatsucards`, `betterrepack`, `mega`, `drive`, `pixeldrain`, `dropbox`, `mediafire`, `catbox`, `gofile`. By default all run. To run only some,
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
built from `telegram.json`, every `data/mega/*.json`, every `data/drive/*.json`, every
`data/pixeldrain/*.json`, every `data/dropbox/*.json` and every `data/mediafire/*.json` every `data/catbox/*.json` and every `data/gofile/*.json`:

```json
{ "com.example.mod": { "mega": ["https://mega.nz/folder/..."], "drive": ["https://drive.google.com/file/d/.../view"], "pixeldrain": ["https://pixeldrain.com/u/..."], "dropbox": ["https://www.dropbox.com/scl/fi/..."], "mediafire": ["https://www.mediafire.com/file/..."], "catbox": ["https://files.catbox.moe/..."], "gofile": ["https://gofile.io/d/...#..."], "telegram": ["https://t.me/..."] } }
```

All the lists are always present (empty if the GUID isn't known from that source);
telegram links are newest first.

## Local configuration (.env)

When run locally, the scraper reads a `.env` file from the working directory (or the file named by
`DOTENV_FILE`). Copy `.env.example` to `.env` and fill it in; `.env` is git-ignored. Variables already
set in the real environment (e.g. GitHub Actions secrets) take precedence over the file, and a missing
file is fine.
