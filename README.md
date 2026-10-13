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
| *(none)* | Lidarr does not know the album, or has it but is not looking for it (the API calls that `inlidarr`). The page shows no chip for either, since to a person both mean "not requested yet"; a request fixes the second. |

"Not found" is inferred from the album's `lastSearchTime`, because Lidarr has
no explicit "search failed" state. Secondary types (Live, Soundtrack,
Compilation, …) are shown as small tags under the title.

Clicking a card opens the album: a large cover, release date, rating, genres,
your library state, a link to MusicBrainz, and the **track list**. For an album
you own the tracks come from Lidarr, with a check mark per track that is on
disk. For any other album they come from the most representative edition (the
most common track count among its editions, earliest first), and the album page says
which edition that is. An open album has its own URL (`#album=<id>`), so it can
be linked and the browser's Back button closes it.

## Requests and approval

Anyone let in can ask for an album; **nothing reaches Lidarr until an approver says
yes**. A request is recorded in the database and is *pending*; the album shows a
**Pending approval** chip, and nobody can ask for it again meanwhile (a second click
by anyone is told it is already waiting). An approver then approves it, which is
what makes Lidarr add, monitor and search for it (see below), or declines it with a
reason. A declined album can be requested again, as a new record, and the old
decision stays as history. An approver's or admin's own request is approved at once,
marked automatic: they could approve it themselves a second later.

- One pending request per album, enforced by the database, so two clicks or two
  people at once never make two. Approving and declining are decided once: a
  second decision is a `409`, and approving twice never searches twice.
- If Lidarr cannot take it at approval time, the request stays pending with the
  reason, to be approved again.
- If Lidarr already wants the album, nothing is recorded: the chip says so.
- Only approving writes to Lidarr. Declining never does.
- An approved record does not block a new request: if the album has left Lidarr
  since, it can be asked for again. Lidarr's own state, not the records, says
  whether it is wanted.

| Endpoint | Who |
|---|---|
| `POST /api/album/{id}/request` with `{"artistId", "title", "artist"}` | any user: pending for a user, approved at once for an approver or admin |
| `GET /api/artists?q=`, `GET /api/artist/{id}` | any user: artist matches, and an artist's albums with Lidarr's state |
| `GET /api/requests?scope=mine` | any user: their own |
| `GET /api/issues` (`?fresh=1` to check again) | admins |
| `GET /api/requests?scope=pending` | approvers: the queue, oldest first |
| `GET /api/requests?scope=all` | admins: everything, with history |
| `POST /api/requests/{id}/approve`, `/decline` (`{"reason": "…"}`) | approvers |
| `DELETE /api/requests/{id}` | admins: any request. Anyone else: their own request while it is still pending (shown as **Cancel**). Only the record goes; Lidarr is not touched. Every deletion is logged with who and what. |

Each list returns at most 200 records. Every decision is logged with who made it.

The pages for it:

- **Requests** (everyone): one page. A user sees their own; an approver also sees what
  waits on them; an admin sees everyone's. Each request shows who asked (to approvers
  and admins), what happened, the decline reason and who decided. Approve, or decline
  with an optional reason, from the row or from the album's page. The menu at the top
  sorts (*Most recent*, *Last modified*) and filters: *All*, *Pending*, *Approved*
  (approved, Lidarr still searching), *Processing* (in the download queue), *Failed*
  (a stalled download or import, or the search could not start), *Declined*,
  *Available*. A count of what is pending shows in the nav. Old notification links to
  `#/approvals` open this page on *Pending*.
- **Settings → Users** (admins): the second tab of Settings (the first is Lidarr).
  Everyone who has signed in, with an Approver checkbox that saves at once.

A page a role may not use is not in its nav, and opening its address sends the
person to search; the server refuses the calls regardless. On a phone the nav is a
bottom tab bar (icon over label, a count on the icon), as in Reaparr, with the
Settings sub-tabs floating just above it.

**Deleting.** On a phone or tablet a row slides left to reveal a red **Delete** (or **Cancel**) button, as on iOS; tapping elsewhere or swiping back closes it. With a mouse the button stays in the row and a second press confirms. Admins can delete any request; anyone else can cancel their own request while it is still pending. Deleting removes the record only, never anything in Lidarr, and frees the album to be asked for again.

**Asking again.** Lidarr's own state decides whether an album can be requested, not the
records: an approved request does not block a new one. If an album that was approved is
later unmonitored or removed in Lidarr, its page shows **Request again**, with a note that
it was approved before (by you, or "requested", never naming anyone else) and Lidarr is no
longer monitoring it. Asking again records a new request next to the old one.

### Open in Lidarr

Admins get an **Open in Lidarr** link on an album's page, when Lidarr has the album and an optional **Web address** is set under Settings → Lidarr (where you open Lidarr in a browser, which is not the address albumreq talks to; it opens `<address>/album/<MusicBrainz id>`). Nobody else is sent the address or shown the link.

### Logging

albumreq logs to stdout, and the container runs at `debug` level (`ALBUMREQ_SETTING_LOG_LEVEL`), so
nothing is left out:

- **Every request it serves** (`request`: method, path, status, bytes, time taken, the signed-in
  user's subject, the client address from the proxy). The page's own files and `/healthz` are `debug`,
  everything else `info`, a 4xx `warn`, a 5xx `error`.
- **Every request it makes** (`outgoing request`: MusicBrainz, the metadata service, the cover CDN,
  Lidarr, ntfy, the identity provider): method, host, path, status, time. A success is `debug`, a
  failure `warn`, a slow one `info`.
- **What happened to a request** (asked, approved, declined, deleted, who by), settings changes,
  notifications sent or failed, and each pass that checks approved requests for arrival.
- **Never** a body, a header, a cookie, an API key or token, or the value of any query parameter
  whose name looks like a secret. A failed outgoing request keeps the cause but not the address it
  was sent to.

In this stack Promtail ships every container's log to Loki (kept seven days), so an error is still
there after the container is replaced; read it in Grafana with `{container="albumreq"}`. MusicBrainz
is asked again once, after two seconds or what it asks for (at most five), when a request fails in a
way that often passes by itself (a dropped connection, a 429 or a 5xx).

### Loading states

While data loads, a page shows grey skeleton shapes laid out like the real content (search results, artist chips and pages, the album page's details and tracks, Requests, Account, Issues and the Settings tabs), so nothing jumps when it arrives. Spinners are kept only inside a button whose action is under way.

### Album pages

An album opens as a page of its own (`#album=<MusicBrainz id>`), not a popup: a back link at the top
says where it goes (**Search**, **Requests**, or the artist's name when you came from an artist's
page), and the browser's Back button and Escape do the same. The page you came from is kept, so you
return to the same results at the same scroll position. Where an album was opened from is in its address (`#album=<id>&artist=<artist id>` from an artist's page, `&from=requests` from Requests, search results via the `?q=`), so a reload or a pasted link keeps the same back link; an unknown or malformed value is ignored.

### Browsing by artist

Searching also looks for artists. The strongest matches (at most six, MusicBrainz score 50 or
more) show as chips under the search box; the album results come first and do not wait for
them. A chip opens the artist's page (`#artist=<MusicBrainz ID>`, so it can be linked and the
Back button works): every release group Lidarr's metadata service lists for them, newest first,
in tabs for **Albums** (studio albums), **EPs**, **Singles** and **More** (live albums,
compilations, broadcasts). The header shows the artist's photo as a round picture when the metadata service has a poster (else a fanart picture): the artist answer already lists them, so nothing extra is asked. albumreq fetches it itself, only from the image CDN's host, refuses anything that does not decode as an image, shrinks it (480 pixels wide, or 96 for `?size=96`) and serves it as JPEG with a day's caching (`GET /api/artist/{id}/photo`); both sizes come from one download, so the browser never talks to the CDN. Without a photo the header is just the name. The artist chips in a search show the 96-pixel thumbnail over their letter circle; an artist with no photo keeps the letter. Each card carries Lidarr's usual chip, and opening one shows the
same album page as from a search, so **requesting is unchanged**. For the chips, albumreq asks
Lidarr for the artist's albums in one go (nothing is asked for an artist Lidarr does not
have); if Lidarr cannot be reached the albums are listed without chips.

### Progress on the requests

An approved request on **Requests** follows Lidarr, refreshed
every 15 seconds while the page is open: *Searching*, *Not found yet*, *Downloading*, *Queued*, *Paused*, *Importing*, *Needs attention* (a failed download
or an import Lidarr is blocked on, which an admin should look at in Lidarr), and *Available*.
It is read from Lidarr's download queue and the album's file count; the queue's field names
come from Lidarr's source and were not yet seen with a live download. If Lidarr cannot be
asked, the request simply shows *Approved*.

## Requesting

When it is turned on, an album that Lidarr does not have, or has but is not
looking for (status `inlidarr`, shown without a chip), shows a **Request** button on its page. A
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
- The artist gets Lidarr's metadata profile named **None**, always: it is a rule,
  not a setting. It allows no album types, so Lidarr imports none of the artist's
  other albums and a request adds only the album asked for. (For comparison, the
  restrictive Standard profile imported 4 albums for one artist, and an all-types
  profile 575 for another.) albumreq looks the profile up by name, ignoring case,
  when it adds an album. If Lidarr has none by that name, a request is refused
  before anything is written, and the **Issues** page says what to create: a
  profile named None that allows no album types.
- The requested album is added even when the profile would normally exclude its
  type (a live album, a soundtrack): Lidarr keeps manually added albums.
- Requesting is idempotent. An album Lidarr is already looking for (monitored),
  or already has in full, is reported as such and is not searched again, so a
  second click never re-grabs. albumreq never writes unless it could first ask
  Lidarr what it has.
- If the album is added but the search cannot be started, the page says so: the
  album is in Lidarr and monitored, but Lidarr will not look for it by itself.
- An album Lidarr's own metadata does not know yet (a very new MusicBrainz entry)
  cannot be added: Lidarr refuses it ("An album with this ID was not found") and
  the request says so.

**What a request carries.** The album's MusicBrainz ID is in the address; the body
carries what the card already showed: the artist's MusicBrainz ID and the title and
artist for the queue. Nothing else is asked: the metadata service is not involved in
requesting at all, so a request works when it is down. The IDs are checked for shape
(UUIDs), the text is trimmed and limited. A wrong artist ID cannot file an album under
the wrong artist: checked on a throwaway Lidarr, a different artist's ID or a made-up
one still gives the album its own artist, because Lidarr works the artist out from the
album and only insists that an ID is present. The artist ID is kept with the request,
so an approval, which has only the record, sends the same one.

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

## Issues

An admin-only **Issues** page (a count shows in the nav) lists what is wrong with
albumreq's own setup, each with what it means and what to do. The checks ask Lidarr
live, so **Check again** shows the result of a fix at once; the nav count is
remembered for 30 seconds. It checks:

| Issue | Severity |
|---|---|
| Lidarr is not set up | error |
| Lidarr's address cannot be used | error |
| Lidarr rejected the API key | error |
| Lidarr cannot be reached | error |
| A requesting setting (root folder, quality profile) does not match Lidarr | error |
| Lidarr has no metadata profile named **None** | error |
| A setting from the environment cannot be used (a bad number, say) | error |
| Requesting is off (no root folder or quality profile chosen) | warning |
| The login is off, or no admin group is set | warning |
| A leftover `ALBUMREQ_SETTING_LIDARR_METADATA_PROFILE_ID` | warning |

When Lidarr cannot be reached or rejects the key, the later questions are not asked.
An issue never contains the API key or a bad value from the environment. Warnings
and errors count in the nav; notes do not.

## Notifications

Push messages through **ntfy**, modelled on Seerr's. Settings → Notifications (admins) holds the
server address, authentication (none, user and password, or an access token), a priority, tags,
the application address (so a message opens the right page) and whether to attach the album
cover. A **Send test** button sends what is on the page before it is saved.

Two kinds of recipient:

- **The shared channel**, one topic with a checklist of events. Meant for whoever approves.
- **Each person**, on their own **Account** page (their name in the top bar, or the person icon
  on a phone): their own topic, and which of *approved, declined, available, could not be
  searched for* they want to hear about, for their own requests only. People cannot see or
  set anyone else's topic, and a topic belongs to one person (case-insensitive). On an open
  ntfy server anyone who knows a topic's name can read it, so the page offers a random one.

| Event | Shared channel | The requester |
|---|---|---|
| A request is waiting for approval | yes | |
| A request was approved automatically (an approver's or admin's own) | yes | |
| A request was approved | yes | yes |
| A request was declined (with the reason) | yes | yes |
| An approved album is available (all tracks in Lidarr) | yes | yes |
| An approved album could not be searched for | yes | yes |

Nobody is told about their own action. Nothing tells albumreq when a download finishes, so it
asks Lidarr every five minutes (`ALBUMREQ_NOTIFICATIONS_CHECK_EVERY`, at least 10 seconds)
about approved requests not yet announced, and gives up on a request after 90 days. Turning
notifications on never replays old events. Sending is asynchronous and never holds up a
request; a full queue drops messages rather than waiting. Errors shown are fixed phrases that
never contain an address or a secret, and a failing server shows up on the Issues page.

Settings pin like Lidarr's: `ALBUMREQ_SETTING_NOTIFICATIONS_<ENABLED|URL|TOPIC|TAGS|AUTH|USERNAME|PASSWORD|TOKEN|PRIORITY|EMBED_POSTER|TYPES|APP_URL>`.
The server address and credentials lock together, so a pinned token can never be sent to an
address typed into the page.

## Settings

Lidarr is configured on the **Settings** page, not in the environment: its URL,
API key, and the two requesting settings (root folder and quality profile).
Saving takes effect at once, with no restart. The page checks what it can before
saving:

- A key Lidarr rejects is refused. An unreachable Lidarr saves with a warning,
  since it may just be starting.
- A root folder or quality profile Lidarr does not have is refused, and
  requesting stays off until they are right. A Lidarr with no metadata profile
  named **None** (see Requesting) is not refused: the settings are saved with a
  warning, and the **Issues** page explains. **Test connection** lists Lidarr's own root
  folders and quality profiles, so you pick instead of typing IDs. An empty root
  folder is pre-filled, for you to confirm with Save, when Lidarr has only one.
  The quality profile is always your choice.
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
read-only, and the value is never written to the database. As in Reaparr, a note
under the title explains the lock chip, a pinned field carries a lock and the name
of the variable ("Managed by …") and is tinted amber and read-only (readonly, so
its value can still be selected and copied). URL and API key are
locked as a pair: pinning either locks both, so a key that is pinned can never be
sent to an address typed into the page.

| Setting | Variable |
|---|---|
| Lidarr URL | `ALBUMREQ_SETTING_LIDARR_URL` |
| Lidarr API key | `ALBUMREQ_SETTING_LIDARR_API_KEY` |
| Root folder | `ALBUMREQ_SETTING_LIDARR_ROOT_FOLDER` |
| Quality profile ID | `ALBUMREQ_SETTING_LIDARR_QUALITY_PROFILE_ID` |
| Web address (optional) | `ALBUMREQ_SETTING_LIDARR_WEB_URL` |

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
| `ALBUMREQ_SETTING_LIDARR_*` | no | | The Lidarr settings above (URL, API key, root folder, quality profile, and the optional web address). Not needed when you use the Settings page. A leftover `ALBUMREQ_SETTING_LIDARR_METADATA_PROFILE_ID` is ignored, with a warning. |
| `ALBUMREQ_SETTINGS_READONLY` | no | off | `true` locks the Settings page |
| `ALBUMREQ_SETTING_NOTIFICATIONS_*` | no | | The ntfy settings (see Notifications). Not needed when you use the Settings page. |
| `ALBUMREQ_NOTIFICATIONS_CHECK_EVERY` | no | `5m` | How often approved requests are checked for arrival (a duration, minimum `10s`) |
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
six hours. The login's own state, `auth/`, is separate and is authy's (`auth.json` signs the sessions, `local.json` holds the local account).

## Login and roles

The login is [authy](https://github.com/Miista/authy) (formerly oidcgate), the same as
Reaparr: on by default. With no `ALBUMREQ_OIDC_*` set, the first visitor creates a local
account (the owner, an admin). With them set, the identity provider signs people in and
**setup stays closed**, so nobody can claim albumreq by getting there first;
`ALBUMREQ_LOCAL_ENABLED=true` keeps the local account next to it as a way in when the
provider is down or turns someone away. `/healthz` is open; everything
else, including the API, needs a sign-in (a page load is redirected, an API call
gets `401`). Sessions last 12 hours.

| Variable | Meaning |
|---|---|
| `ALBUMREQ_AUTH_DISABLED` | `true`: no login at all, and **everyone who can reach the port is an admin**. For loopback or a proxy that authenticates for it. |
| `ALBUMREQ_OIDC_ISSUER`, `_CLIENT_ID`, `_CLIENT_SECRET`, `_BASE_URL` | Single sign-on, all four together; see authy's README. Register the client with redirect URI `<BASE_URL>/auth/callback` and scopes `openid profile email groups`. |
| `ALBUMREQ_LOCAL_ENABLED` | Default: the local account is on exactly when OIDC is off. `true` keeps it next to OIDC as an escape hatch; `false` without OIDC refuses to start. |
| `ALBUMREQ_ROLE_ADMIN_GROUP` | The provider group whose members are **admins** |
| `ALBUMREQ_ROLE_USER_GROUP` | If set, the only way in besides the admin group: a signed-in person in neither group is turned away. Unset: anyone the provider lets sign in is a user. |

Four roles; a higher one may do all a lower one may:

| Role | From | May |
|---|---|---|
| none | signed in, not in the groups | see a "no access" page, nothing else |
| user | the user group | search, open albums, and **ask for** one (it waits for an approver) |
| approver | a user an admin has made an approver | also **approve or decline** requests; their own requests go straight to Lidarr |
| admin | the admin group, or the local account | also the settings, and who the approvers are |

Roles are enforced on the server for every route, never by hiding buttons.
Group names are the provider's: they are read when someone signs in, so **a change
of group takes effect at their next sign-in** (within 12 hours).

Approvers are the one role albumreq keeps itself. The `users` table in the
database remembers everyone who has signed in (only people who were let in), and
an admin marks approvers in it, on Settings → Users (`POST /api/users/approver`).

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
- `internal/issues` — what is wrong, for the Issues page
- `internal/requests` — the request records and the approval flow
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
3. **Done:** login (authy: local account or OIDC), roles from groups, approvers.
4. **Done:** request records and the approval flow (backend, API and the Pending chip).
5. **Done:** the pages: Requests (with sort and filters) and Settings → Users.
6. **Done:** notifications (ntfy: shared channel, per-person topics, availability).
7. Deployment.
