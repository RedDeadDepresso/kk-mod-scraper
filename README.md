# kkafio-mod-index
## Sources and files

| Source | Scraper | Config | Index | Cache |
|---|---|---|---|---|
| Telegram archive (via koikatsucards.com) | `scrapers/telegram.go` | - | `indices/telegram.json` | - |
| BetterRepack | `scrapers/modpack.go` | - | `indices/kk_modpack.json`, `indices/kks_modpack.json` | `cache/modpack.json` |
| Mega folders | `scrapers/mega.go` | `config/mega.json` | `indices/mega/<folder name>.json` | `cache/mega/<folder name>.json` |
| Google Drive folders | `scrapers/drive.go` | `config/drive.json` | `indices/drive/<folder name>.json` | `cache/drive/<folder name>.json` |

### Mega and Google Drive folders

`config/mega.json` and `config/drive.json` are JSON arrays of public folder links:

```json
["https://mega.nz/folder/<id>#<key>"]
["https://drive.google.com/drive/folders/<id>"]
```

Add a link to index another folder; remove one and its files are deleted on the next fully
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

Flags (all optional) for both: `-config`, `-indices-dir`, `-cache-dir`, `-full`,
`-retry-failed`, `-max-urls`, `-workers`.

### Published site

The Pages workflow publishes `kk_modpack.json`, `kks_modpack.json` and a merged `mod.json`
built from `telegram.json`, every `indices/mega/*.json` and every `indices/drive/*.json`:

```json
{ "com.example.mod": { "mega": ["https://mega.nz/folder/..."], "drive": ["https://drive.google.com/file/d/.../view"], "telegram": ["https://t.me/..."] } }
```

All three lists are always present (empty if the GUID isn't known from that source);
telegram links are newest first.
