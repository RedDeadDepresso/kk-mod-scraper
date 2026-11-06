# kkafio-mod-index
## Sources and files

| Source | Scraper | Index | Cache |
|---|---|---|---|
| Telegram archive (via koikatsucards.com) | `scrapers/telegram.go` | `indices/telegram.json` | - |
| BetterRepack | `scrapers/modpack.go` | `indices/kk_modpack.json`, `indices/kks_modpack.json` | `cache/modpack.json` |
| Mega folders | `scrapers/mega.go` | `indices/mega/<folder name>.json` | `cache/mega/<folder name>.json` |

### Mega folders

`config/mega.json` is a JSON array of public Mega folder links:

```json
["https://mega.nz/folder/<id>#<key>"]
```

Add a link to index another folder; remove one and its files are deleted on the next
fully successful run. Each folder gets an index and a cache named after the folder as it is
called on Mega (`<name> (<id>)` if two folders share a name). Mega files are encrypted, but
the folder link carries the key and AES-CTR can be decrypted from any offset, so only the zip
tail and `manifest.xml` of each new file are downloaded.

Flags (all optional): `-config`, `-indices-dir`, `-cache-dir`, `-full`, `-retry-failed`,
`-max-urls`, `-workers`.

### Published site

The Pages workflow publishes `kk_modpack.json`, `kks_modpack.json` and a merged `mod.json`
built from `telegram.json` and every `indices/mega/*.json`:

```json
{ "com.example.mod": { "mega": ["https://mega.nz/folder/..."], "telegram": ["https://t.me/..."] } }
```

Both lists are always present (empty if the GUID is only known from the other source);
telegram links are newest first.
