# albumreq

*Search for an album. See at a glance whether you already have it.*

albumreq is a small request frontend for [Lidarr](https://lidarr.audio), in the
spirit of Seerr for movies and TV. It searches, shows what you already have,
opens an album's details and track list, and can **add an album and its artist
to Lidarr** (see "Requesting"). Everything except that one write is a `GET`,
and the write is **off unless you configure it**.

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
| **In Lidarr** | In Lidarr but not monitored, so Lidarr is not looking for it. A request fixes that. |
| *(none)* | Lidarr does not know the album. |

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

## Requesting

When it is turned on, an album that Lidarr does not have, or has but is not
looking for (the *In Lidarr* chip), shows a **Request** button in its sheet. A
request does three things in Lidarr, as three calls:

1. **Adds** the album, monitored, and its artist if Lidarr lacks it, with the
   artist **not** monitored and new releases by the artist not monitored.
2. Makes sure the album is **monitored**. For an album Lidarr already had (added
   earlier, or imported with its artist) this is the whole of step 1.
3. Starts an **`AlbumSearch`** for that one album. If a release fits the quality
   profile, Lidarr grabs it and the download client takes it from there.

The chips then follow Lidarr: *Requested* while it waits, *Processing* when it is
in the download queue, *Available* when every track is on disk, *Not found* if
it searched and grabbed nothing.

What that means in Lidarr (verified against throwaway instances with synthetic
data, never against a real library):

- Why three calls: Lidarr only searches on add when it also adds the artist, so
  for an artist you already have the add alone would monitor the album and never
  look for it. And the artist's `addOptions.monitor` is deliberately not sent:
  Lidarr applies it, after the add returns, to every album imported with a new
  artist, which unmonitored the requested one. Without it, only the requested
  album is monitored, also when the profile imports several (checked with
  Standard, over several refreshes).
- The artist's *other* albums are imported as well, but only those the
  configured metadata profile allows, all unmonitored. The **None** profile
  imports none of them. A restrictive profile (Lidarr's Standard) gave small
  catalogues, 4 albums for an artist, where an all-types profile gave 575.
- The requested album is added even when the profile would normally exclude its
  type (a live album, a soundtrack): Lidarr keeps manually added albums.
- Requesting is idempotent. An album Lidarr is already looking for (monitored),
  or already has in full, is reported as such and is not searched again, so a
  second click never re-grabs. albumreq never writes unless it could first ask
  Lidarr what it has.
- If the album is added but the search cannot be started, the page says so: the
  album is in Lidarr and monitored, but Lidarr will not look for it by itself.
- An album Lidarr's metadata service does not know yet (a very new MusicBrainz
  entry) cannot be added, and says so.

There is **no request tracking yet**: nothing records who asked, when, or what
was decided. The only state is Lidarr's own.

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

## Settings

Lidarr is configured on the **Settings** page, not in the environment: its URL,
API key, and the three requesting settings (root folder, quality profile,
metadata profile). Saving takes effect at once, with no restart. The page checks
what it can before saving:

- A key Lidarr rejects is refused. An unreachable Lidarr saves with a warning,
  since it may just be starting.
- A root folder or profile Lidarr does not have is refused, and requesting stays
  off until all three are valid. **Test connection** lists Lidarr's own root
  folders and profiles, so you pick instead of typing IDs. Two empty fields are
  pre-filled for you to confirm with Save: the root folder when Lidarr has only
  one, and the metadata profile named **None**, which imports none of the
  artist's other albums (verified on a throwaway Lidarr: only the requested
  album appears, unmonitored). The quality profile is always your choice.
- The API key never leaves the server: the page shows only that one is saved,
  and an empty key field means "keep it". The saved key is only ever sent to the
  saved URL, so changing the URL means typing the key again, and **Test
  connection** cannot be pointed at another address to probe it with the saved
  key.

Settings are saved in the database (`albumreq.db` under `ALBUMREQ_DATA_DIR`,
default `/app/data`, mode 0600), so that directory needs a volume. Only admins can
open the page.

### Pinning a setting with an environment variable

Any setting can instead be set by `ALBUMREQ_SETTING_<NAME>`. A non-empty
variable wins over what is saved, the field shows **Managed by `<VAR>`** and is
read-only, and the value is never written to the database. URL and API key are
locked as a pair: pinning either locks both, so a key that is pinned can never be
sent to an address typed into the page.

| Setting | Variable |
|---|---|
| Lidarr URL | `ALBUMREQ_SETTING_LIDARR_URL` |
| Lidarr API key | `ALBUMREQ_SETTING_LIDARR_API_KEY` |
| Root folder | `ALBUMREQ_SETTING_LIDARR_ROOT_FOLDER` |
| Quality profile ID | `ALBUMREQ_SETTING_LIDARR_QUALITY_PROFILE_ID` |
| Metadata profile ID | `ALBUMREQ_SETTING_LIDARR_METADATA_PROFILE_ID` |

`ALBUMREQ_SETTINGS_READONLY=true` locks every setting and refuses all changes
from the page, for whoever is signed in. With the login off it is the only thing
standing between anyone who can reach the port and the page that can repoint
albumreq at another Lidarr.

An environment value Lidarr cannot use (a profile that does not exist, an ID
that is not a number) leaves requesting off, says why on the Settings page, and
does not stop the process.

## Configuration

Environment variables:

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `ALBUMREQ_SETTING_LIDARR_*` | no | | The five Lidarr settings above. Not needed when you use the Settings page. |
| `ALBUMREQ_SETTINGS_READONLY` | no | off | `true` locks the Settings page |
| `ALBUMREQ_DATA_DIR` | no | `/app/data` | Where `albumreq.db` (settings, users; holds the Lidarr key), `backup/` and `auth/` (the local account and session key) are kept |
| `ALBUMREQ_SETTING_MUSICBRAINZ_CONTACT` | yes | | An email address or a project URL. MusicBrainz requires a way to contact whoever runs an application, in the User-Agent. **There is no default:** it is yours to choose, and it is sent to MusicBrainz only, never to Lidarr's services. |
| `ALBUMREQ_SETTING_MUSICBRAINZ_URL` | no | public MusicBrainz | Another MusicBrainz web service (WS/2), e.g. a self-hosted mirror |
| `ALBUMREQ_SETTING_METADATA_URL` | no | `https://api.lidarr.audio/api/v0.4` | Another metadata service |
| `ALBUMREQ_SETTING_COVER_URL` | no | Lidarr's image CDN | Another cover source with the Cover Art Archive's paths (`/release-group/<id>/front-250`) |
| `ALBUMREQ_SETTING_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `ALBUMREQ_ADDR` | no | `127.0.0.1:8768` | Listen address. The container image sets `:8768`. |

## Storage

Everything albumreq keeps lives in one SQLite database, `albumreq.db` in the data
directory (pure-Go driver, `modernc.org/sqlite`, so the image stays a static
binary): the Lidarr settings, the users, and later the request records. The file
is 0600 and so is its write-ahead log, because it holds the Lidarr API key. The
schema is versioned in `PRAGMA user_version` and migrated at start; a database
written by a newer albumreq is refused, and a file that is not a database is an
error, never overwritten.

**Back up `backup/albumreq.db`, not `albumreq.db`.** Copying a live SQLite file can
capture the database and its write-ahead log from two different moments. albumreq
writes a consistent copy (`VACUUM INTO`, renamed into place) at start and every
six hours. The login's own state, `auth/`, is separate and is oidcgate's.

## Login and roles

The login is [oidcgate](https://github.com/Miista/oidcgate), the same as Reaparr:
on by default, a local account made on first launch (the owner, an admin), and
optional single sign-on through any OIDC provider. `/healthz` is open; everything
else, including the API, needs a sign-in (a page load is redirected, an API call
gets `401`). Sessions last 12 hours.

| Variable | Meaning |
|---|---|
| `ALBUMREQ_AUTH_DISABLED` | `true`: no login at all, and **everyone who can reach the port is an admin**. For loopback or a proxy that authenticates for it. |
| `ALBUMREQ_OIDC_ENABLED`, `_ISSUER`, `_CLIENT_ID`, `_CLIENT_SECRET`, `_BASE_URL` | Single sign-on; see oidcgate's README. Register the client with scopes `openid profile email groups`. |
| `ALBUMREQ_ROLE_ADMIN_GROUP` | The provider group whose members are **admins** |
| `ALBUMREQ_ROLE_USER_GROUP` | If set, the only way in besides the admin group: a signed-in person in neither group is turned away. Unset: anyone the provider lets sign in is a user. |

Four roles; a higher one may do all a lower one may:

| Role | From | May |
|---|---|---|
| none | signed in, not in the groups | see a "no access" page, nothing else |
| user | the user group | search and open albums |
| approver | a user an admin has made an approver | also **request** an album (for now: see below) |
| admin | the admin group, or the local account | also the settings, and who the approvers are |

Roles are enforced on the server for every route, never by hiding buttons.
Group names are the provider's: they are read when someone signs in, so **a change
of group takes effect at their next sign-in** (within 12 hours).

Approvers are the one role albumreq keeps itself. The `users` table in the
database remembers everyone who has signed in (only people who were let in), and
an admin marks approvers in it (`POST /api/users/approver`; the page for it comes
with the approval flow). Until requests can wait for approval, **a request goes
straight to Lidarr**, so only approvers and admins can make one: a plain user's
request would start a download nobody approved.

Hardening that stays: every write refuses a call without an `X-Albumreq` header
and any browser request marked cross-site, and requesting and the settings keep
their own switches. Lidarr has no scoped API keys, so the key albumreq holds can
do everything the Lidarr UI can; the code only ever adds an album, monitors it,
and searches for it.

Everything that reaches an upstream URL is validated first: album IDs must be
lowercase UUIDs, cover sizes are 250 or 500, and a cover redirect may only go to
`images.lidarr.audio`, `coverartarchive.org` or `archive.org`.

## Development

```sh
go test ./... -race
ALBUMREQ_DATA_DIR=/tmp/albumreq \
ALBUMREQ_SETTING_MUSICBRAINZ_CONTACT=https://example.org/you \
go run .          # http://127.0.0.1:8768, then open Settings
```

The frontend is embedded into the binary, so restart `go run` after changing
anything under `web/static`.

Layout:

- `internal/musicbrainz` — release-group search, one request per second
- `internal/lidarrmeta` — an album, its editions and tracks, from Lidarr's metadata service
- `internal/coverart` — cover images through Lidarr's image CDN
- `internal/lidarr` — your Lidarr: library state, queue, library tracks
- `internal/db` — the one SQLite database: opening, versioned migrations, backup copy
- `internal/store` — the Lidarr settings, in the database
- `internal/users` — who has signed in, and who is an approver
- `internal/auth` — groups and the approver flag to a role
- `internal/settings` — env-over-store resolution, locks, validation, the service behind the page
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
2. **Done:** request an album: add it, monitor it and search for it.
3. **Done:** login (oidcgate, OIDC), roles from groups, approvers.
4. Request records (who asked, when, what was decided), keyed by MusicBrainz ID,
   an approval step, "My requests", and the page where an admin picks approvers.
   See `PLAN-REQUESTS.md`.
