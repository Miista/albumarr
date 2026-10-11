# albumreq

*Search for an album. See at a glance whether you already have it.*

albumreq is a small request frontend for [Lidarr](https://lidarr.audio), in the
spirit of Seerr for movies and TV. **It is read-only so far**: it searches,
shows what you already have, and opens an album's details and track list. It
never writes to Lidarr, it only issues `GET` requests. Requesting an album
(adding it to Lidarr) comes next.

## What it does

A search page with an album poster grid, 24 results at a time: a **Show more**
button fetches the next page (up to ten pages, 240 results; past that, refine the
query). Each card shows the cover, a **type
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

Clicking a card opens the album: a large cover, release date, rating, genres,
your library state, a link to MusicBrainz, and the **track list**. For an album
you own the tracks come from Lidarr, with a check mark per track that is on
disk. For any other album they come from the most representative edition (the
most common track count among its editions, earliest first), and the sheet says
which edition that is. An open album has its own URL (`#album=<id>`), so it can
be linked and the browser's Back button closes it.

## Where the data comes from

Three sources, each doing what it is best at (Lidarr's hosted service supplies
both details and covers). Albums are identified everywhere by their MusicBrainz
release-group ID.

| Source | Used for | Why |
|---|---|---|
| **MusicBrainz** | Search | Its ranking is the best available. About one request per second is allowed, so searches are queued, cached, and typing is debounced. |
| **Lidarr's hosted metadata service** (`api.lidarr.audio`) | Album details and tracks, all editions in one call | Cached at the edge, no practical rate limit. It has no documented API for third parties, so the base URL is configurable and a failure only means "details unavailable". |
| **Lidarr's image CDN** (`images.lidarr.audio`) | Covers, a cache in front of the Cover Art Archive | More dependable than archive.org's storage nodes. Covers are re-served by albumreq and cached in memory, so the browser never talks to anyone else. |
| **Your Lidarr** | Library status, download queue, the tracks of albums you own | It is where your library lives. If it is unreachable the page still works, just without chips. |

## Configuration

Environment variables:

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `ALBUMREQ_SETTING_LIDARR_URL` | yes | | Your Lidarr, e.g. `http://lidarr:8686` |
| `ALBUMREQ_SETTING_LIDARR_API_KEY` | yes | | Lidarr API key (Settings → General) |
| `ALBUMREQ_SETTING_MUSICBRAINZ_CONTACT` | yes | | An email address or a project URL. MusicBrainz requires a way to contact whoever runs an application, in the User-Agent. **There is no default:** it is yours to choose, and it is sent to MusicBrainz only, never to Lidarr's services. |
| `ALBUMREQ_SETTING_MUSICBRAINZ_URL` | no | public MusicBrainz | Another MusicBrainz web service (WS/2), e.g. a self-hosted mirror |
| `ALBUMREQ_SETTING_METADATA_URL` | no | `https://api.lidarr.audio/api/v0.4` | Another metadata service |
| `ALBUMREQ_SETTING_COVER_URL` | no | Lidarr's image CDN | Another cover source with the Cover Art Archive's paths (`/release-group/<id>/front-250`) |
| `ALBUMREQ_SETTING_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `ALBUMREQ_ADDR` | no | `127.0.0.1:8768` | Listen address. The container image sets `:8768`. |

### There is no login yet

Anyone who can reach the port can search your Lidarr and see your library
state. That is why the default bind is loopback only, and why the server logs a
warning when started on anything wider. Until a login is added, run it on a
network only you can reach, or behind a proxy that authenticates. Lidarr has no
scoped API keys, so the key albumreq holds can do everything the Lidarr UI can;
the code only ever reads.

Everything that reaches an upstream URL is validated first: album IDs must be
lowercase UUIDs, cover sizes are 250 or 500, and a cover redirect may only go to
`images.lidarr.audio`, `coverartarchive.org` or `archive.org`.

## Development

```sh
go test ./... -race
ALBUMREQ_SETTING_LIDARR_URL=http://localhost:8686 \
ALBUMREQ_SETTING_LIDARR_API_KEY=... \
ALBUMREQ_SETTING_MUSICBRAINZ_CONTACT=https://example.org/you \
go run .          # http://127.0.0.1:8768
```

The frontend is embedded into the binary, so restart `go run` after changing
anything under `web/static`.

Layout:

- `internal/musicbrainz` — release-group search, one request per second
- `internal/lidarrmeta` — an album, its editions and tracks, from Lidarr's metadata service
- `internal/coverart` — cover images through Lidarr's image CDN
- `internal/lidarr` — your Lidarr: library state, queue, library tracks
- `internal/catalog` — combines them, and degrades when one is missing
- `internal/api` — HTTP handlers
- `web/static` — the petite-vue frontend

petite-vue is vendored at `web/static/vendor/petite-vue.iife.js` (sha256
`774bb8e88b09936246a57f0dfed88a375258a8235b893561c96880411dabc4d5`, the same
file reaparr and composelens ship), and Geist is self-hosted under
`web/static/vendor/fonts` (OFL-1.1, licence included). The design follows
reaparr's. One petite-vue quirk to know: a `v-if` cannot be a direct child of
`<template v-if>`, so wrap in a real element.

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

1. **Done:** search, poster grid, type badges, status chips, album details and tracks.
2. Request an album: add the artist unmonitored (so no other albums are pulled
   in), then monitor just that album and search. The artist needs a metadata
   profile that allows the album's type, otherwise a requested live album or
   soundtrack does not exist for the artist and is silently dropped. A very new
   MusicBrainz album may not be in Lidarr's metadata yet, which needs its own
   "not available yet" state.
3. Request records, an approval step, "My requests".
4. Login (Authelia OIDC).
