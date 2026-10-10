# albumreq

*Search for an album. See at a glance whether you already have it.*

albumreq is a small request frontend for [Lidarr](https://lidarr.audio), in the
spirit of Seerr for movies and TV. **MVP 1 is search and status only**: it
never writes to Lidarr, it only issues `GET` requests. Requesting an album
(adding it to Lidarr) comes next.

## What it does

A search page with an album poster grid. Each card shows the cover, a **type
badge** (Album, EP, Single, …) and, when Lidarr knows about the album, a
**status chip**:

| Chip | Meaning |
|---|---|
| **Available** | Every track has a file in the library. |
| **Processing** | An item for the album is in Lidarr's download queue. |
| **Requested** | Monitored in Lidarr, not searched for yet. |
| **Not found** | Monitored and searched, but nothing was grabbed. |
| *(none)* | Lidarr does not know the album, or knows it but it is not monitored. |

"Not found" is inferred from the album's `lastSearchTime`, because Lidarr has
no explicit "search failed" state. Secondary types (Live, Soundtrack,
Compilation, …) are shown as small tags under the title.

Search goes through Lidarr's own `album/lookup` (MusicBrainz), so results and
their order are Lidarr's. Cover art is fetched through Lidarr's cover proxy and
re-served by albumreq, so the browser never talks to Lidarr or to third-party
image hosts directly.

## Configuration

Environment variables:

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `ALBUMREQ_SETTING_LIDARR_URL` | yes | | Lidarr base URL, e.g. `http://lidarr:8686` |
| `ALBUMREQ_SETTING_LIDARR_API_KEY` | yes | | Lidarr API key (Settings → General) |
| `ALBUMREQ_SETTING_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `ALBUMREQ_ADDR` | no | `127.0.0.1:8768` | Listen address. The container image sets `:8768`. |

### There is no login yet

Anyone who can reach the port can search your Lidarr. That is why the default
bind is loopback only, and why the server logs a warning when started on
anything wider. Until a login is added, run it on a network only you can reach,
or behind a proxy that authenticates. Lidarr has no scoped API keys, so the key
albumreq holds can do everything the Lidarr UI can; in MVP 1 the code only ever
reads.

## Development

```sh
go test ./... -race
ALBUMREQ_SETTING_LIDARR_URL=http://localhost:8686 \
ALBUMREQ_SETTING_LIDARR_API_KEY=... \
go run .          # http://127.0.0.1:8768
```

Layout: `internal/lidarr` (read-only API client), `internal/catalog` (search and
status derivation), `internal/api` (HTTP handlers), `web/static` (petite-vue
frontend, embedded into the binary).

petite-vue is vendored at `web/static/vendor/petite-vue.iife.js` (sha256
`774bb8e88b09936246a57f0dfed88a375258a8235b893561c96880411dabc4d5`, the same
file reaparr and composelens ship).

### Image

The binary is cross-compiled with Go and copied into a distroless image, the
same as reaparr:

```sh
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o dist/linux/$arch/albumreq .
done
docker build --build-arg TARGETARCH=arm64 -t albumreq:local .
```

## Roadmap

1. **MVP 1 (this):** search, poster grid, type badges, status chips.
2. Request an album: add the artist unmonitored (so no other albums are pulled
   in), then monitor just that album and search. The artist needs a metadata
   profile that allows the album's type, otherwise a requested live album or
   soundtrack does not exist for the artist and is silently dropped.
3. Request records, an approval step, "My requests".
4. Login (Authelia OIDC).
