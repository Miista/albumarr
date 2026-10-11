const STATUS_LABELS = {
  available: 'Available',
  processing: 'Processing',
  requested: 'Requested',
  notfound: 'Not found',
  inlidarr: 'In Lidarr',
  pending: 'Pending approval',
};

// MusicBrainz answers about one search a second, so wait for a pause in the
// typing rather than searching on every few keystrokes.
const DEBOUNCE_MS = 600;
const MIN_QUERY = 2;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// api is fetch, except that a 401 (the sign-in ended) reloads the page, which the
// login gate sends through sign-in and back again.
function api(url, opts) {
  return fetch(url, opts).then((res) => {
    if (res.status === 401) location.reload();
    return res;
  });
}

// The pages other than search have their own hash; search keeps the query string.
const HASHES = { requests: '#/requests', approvals: '#/approvals', settings: '#/settings' };
const PAGE_OF_HASH = { '#/requests': 'requests', '#/approvals': 'approvals', '#/settings': 'settings' };

function App() {
  // Kept out of the reactive scope: timers and the in-flight counters have no
  // reason to trigger a re-render.
  let timer = null;
  let seq = 0;
  let detailSeq = 0;

  return {
    query: '',
    shownQuery: '',
    albums: [],
    libraryOk: true,
    hasMore: false,
    nextOffset: 0,
    loadingMore: false,
    moreError: '',
    loading: false,
    searched: false,
    error: '',

    selected: null, // the card (or just { id }) behind the open sheet
    detail: null,
    detailLoading: false,
    detailError: '',
    requesting: false,
    requestNote: '',
    requestError: '',

    // --- settings page ---

    page: 'search',
    me: null, // GET /api/me: who is signed in and what they may do

    // requests: mine, the approval queue (and its history, for admins), and who acts
    mine: [],
    mineLoaded: false,
    mineError: '',
    queue: [],
    history: [],
    queueLoaded: false,
    queueError: '',
    tab: 'waiting',
    pendingCount: 0,
    notice: '',
    busy: {}, // request id -> an approve/decline is in flight
    rowError: {}, // request id -> why the last one failed
    declining: null, // the request id whose decline form is open
    declineReason: '',

    // Settings -> Users (admins)
    users: [],
    usersLoaded: false,
    usersError: '',
    userBusy: {},
    cfg: null, // GET /api/settings (admins)
    cfgError: '',
    form: { url: '', api_key: '', root_folder: '', quality_profile_id: 0 },
    options: null, // root folders and profiles Lidarr offered on the last successful test
    testing: false,
    testResult: null,
    saving: false,
    saveState: '',
    saveError: '',
    warning: '',

    go(page) {
      if (page === this.page) return;
      if (this.selected) this.close();
      history.pushState(null, '', location.pathname + (HASHES[page] || location.search));
      this.showPage(page);
    },

    showPage(page) {
      this.page = page;
      this.notice = '';
      this.declining = null;
      if (this.me) this.enter();
    },

    // mayView: which pages a role gets. The server enforces the same on every route;
    // this only keeps someone from landing on a page they cannot use.
    mayView(page) {
      if (!this.me || this.me.role === 'none') return false;
      if (page === 'approvals') return this.canApprove();
      if (page === 'settings') return this.me.role === 'admin';
      return true;
    },

    // enter loads what the current page shows, once it is known who is looking.
    enter() {
      const page = this.page;
      if (page !== 'search' && !this.mayView(page)) {
        history.replaceState(null, '', location.pathname);
        this.page = 'search';
        return;
      }
      if (page === 'settings') {
        this.loadSettings();
        this.loadUsers();
      } else if (page === 'requests') {
        this.loadMine();
      } else if (page === 'approvals') {
        this.tab = 'waiting';
        this.queueLoaded = false;
        this.loadQueue();
      }
    },

    async loadSettings() {
      this.cfgError = '';
      try {
        const res = await api('/api/settings', { cache: 'no-store' });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        this.applySettings(await res.json());
        if (this.cfg.configured && this.page === 'settings') this.runTest(false);
      } catch (e) {
        this.cfgError = e.message;
      }
    },

    // applySettings makes the form show what the server now has. The API key is
    // never sent to the page, so its field starts empty: empty means "keep".
    applySettings(c) {
      this.cfg = c;
      const v = c.values;
      this.form = {
        url: v.url,
        api_key: '',
        root_folder: v.root_folder,
        quality_profile_id: v.quality_profile_id,
      };
    },

    // mayRequest: anyone let in may ask. What happens next depends on who asks (see
    // canApprove): an approver's request goes straight to Lidarr, a user's waits.
    mayRequest() {
      return !!this.me && this.me.role !== 'none' && !!this.me.requests_enabled;
    },

    canApprove() {
      return !!this.me && (this.me.role === 'approver' || this.me.role === 'admin');
    },

    lock(key) {
      return this.cfg && this.cfg.locks ? this.cfg.locks[key] : null;
    },

    lockText(key) {
      const l = this.lock(key);
      if (!l) return '';
      if (l.reason === 'env') return 'Managed by ' + l.env_var;
      if (l.reason === 'paired') return 'Locked with ' + l.env_var;
      return 'Read-only';
    },

    // rootChoices lists Lidarr's root folders, plus the current value if Lidarr
    // does not offer it, so a saved setting is never silently dropped.
    rootChoices() {
      if (!this.options) return [];
      const l = this.options.rootFolders.slice();
      if (this.form.root_folder && !l.includes(this.form.root_folder)) l.push(this.form.root_folder);
      return l;
    },

    profileChoices(key, list) {
      const l = (list || []).slice();
      const id = this.form[key];
      if (id && !l.some((p) => p.id === id)) l.push({ id, name: 'Profile ' + id + ' (not in Lidarr)' });
      return l;
    },

    // runTest asks the server to try the connection. With the fields empty or
    // unchanged the server uses the saved key against the saved URL; the page
    // only ever sends a key it was typed. manual=false is the quiet refresh
    // that fills the dropdowns.
    async runTest(manual) {
      if (this.testing) return;
      this.testing = true;
      if (manual) this.testResult = null;
      try {
        const res = await api('/api/settings/test', {
          method: 'POST',
          headers: { 'X-Albumreq': '1', 'Content-Type': 'application/json' },
          body: JSON.stringify({ url: this.form.url, api_key: this.form.api_key }),
        });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const r = await res.json();
        if (manual || !r.ok) this.testResult = r;
        if (r.ok && r.options) {
          this.options = r.options;
          this.applyDefaults();
        }
      } catch (e) {
        this.testResult = { ok: false, message: e.message };
      } finally {
        this.testing = false;
      }
    },

    // applyDefaults pre-fills what only has one sensible answer, never over a
    // value that is set or locked: the only root folder Lidarr has. The user still
    // saves. (The metadata profile is not a choice: it is always "None".)
    applyDefaults() {
      const o = this.options;
      if (!o) return;
      if (!this.lock('lidarr.root_folder') && !this.form.root_folder && o.rootFolders.length === 1) {
        this.form.root_folder = o.rootFolders[0];
      }
    },

    // changes is what differs from the server's view; locked fields and an
    // empty API key are left out.
    changes() {
      if (!this.cfg) return {};
      const v = this.cfg.values;
      const f = this.form;
      const out = {};
      if (!this.lock('lidarr.url') && f.url.trim() !== v.url) out.url = f.url.trim();
      if (!this.lock('lidarr.api_key') && f.api_key.trim() !== '') out.api_key = f.api_key.trim();
      if (!this.lock('lidarr.root_folder') && f.root_folder.trim() !== v.root_folder) out.root_folder = f.root_folder.trim();
      for (const k of ['quality_profile_id']) {
        const n = Number(f[k]) || 0;
        if (!this.lock('lidarr.' + k) && n !== v[k]) out[k] = n;
      }
      return out;
    },

    dirty() {
      return Object.keys(this.changes()).length > 0;
    },

    // The save bar only appears when it has something to say: unsaved changes, a
    // save under way, a failure, or a moment's "Saved". An always-visible bar that
    // says "No changes" is noise, and floats between the cards like a part of
    // whatever is below it.
    showBar() {
      return this.saving || this.dirty() || this.saveState === 'failed' || this.saveState === 'saved';
    },

    // "Saved" wins for its few seconds even if the form is dirty again at once: after
    // the connection is saved the only root folder is pre-filled, which is a change
    // still to confirm, and showing "Unsaved changes" straight away would read as
    // if the save had not worked.
    barState() {
      if (this.saving) return '';
      if (this.saveState === 'failed' || this.saveState === 'saved') return this.saveState;
      return this.dirty() ? 'dirty' : '';
    },

    saveText() {
      if (this.saving) return 'Saving…';
      if (this.saveState === 'failed') return this.saveError;
      if (this.saveState === 'saved') return 'Saved';
      return this.dirty() ? 'Unsaved changes' : '';
    },

    async save() {
      const body = this.changes();
      if (this.saving || !Object.keys(body).length) return;
      this.saving = true;
      this.saveState = '';
      this.warning = '';
      try {
        const res = await api('/api/settings', {
          method: 'POST',
          headers: { 'X-Albumreq': '1', 'Content-Type': 'application/json' },
          body: JSON.stringify({ lidarr: body }),
        });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const data = await res.json();
        this.applySettings(data.settings);
        this.warning = data.warning || '';
        this.saveState = 'saved';
        // "Saved" is a confirmation, not a state: let it go after a moment, unless
        // something newer has happened.
        setTimeout(() => { if (this.saveState === 'saved') this.saveState = ''; }, 3000);
        this.testResult = null;
        this.options = null;
        if (this.cfg.configured) this.runTest(false);
      } catch (e) {
        this.saveError = e.message;
        this.saveState = 'failed';
      } finally {
        this.saving = false;
      }
    },

    mounted() {
      api('/api/me', { cache: 'no-store' }).then((r) => r.ok && r.json()).then((m) => {
        if (!m) return;
        this.me = m;
        this.enter();
        if (this.canApprove()) this.refreshPendingCount();
      }).catch(() => {});
      const q = new URLSearchParams(location.search).get('q');
      if (q) {
        this.query = q;
        this.search();
      }
      window.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && this.selected) this.close();
      });
      window.addEventListener('popstate', () => this.syncFromHash());
      this.syncFromHash();
    },

    onInput() {
      clearTimeout(timer);
      timer = setTimeout(() => this.search(), DEBOUNCE_MS);
    },

    async search() {
      clearTimeout(timer);
      const q = this.query.trim();
      const mine = ++seq;

      this.loadingMore = false;
      this.moreError = '';

      if (q.length < MIN_QUERY) {
        this.albums = [];
        this.hasMore = false;
        this.searched = false;
        this.loading = false;
        this.error = '';
        this.replaceQuery('');
        return;
      }

      this.loading = true;
      this.error = '';
      this.replaceQuery('?q=' + encodeURIComponent(q));
      try {
        const res = await api('/api/search?q=' + encodeURIComponent(q));
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const data = await res.json();
        if (mine !== seq) return; // a newer search has superseded this one
        this.albums = data.albums;
        this.libraryOk = data.libraryOk;
        this.hasMore = data.hasMore;
        this.nextOffset = data.offset + data.albums.length;
        this.shownQuery = data.query;
        this.searched = true;
      } catch (e) {
        if (mine !== seq) return;
        this.albums = [];
        this.hasMore = false;
        this.searched = false;
        this.error = e.message;
      } finally {
        if (mine === seq) this.loading = false;
      }
    },

    // loadMore appends the next page of the search that is on screen. A new
    // search in the meantime wins: its sequence number differs, so this
    // page's answer is dropped.
    async loadMore() {
      if (this.loadingMore || !this.hasMore) return;
      const mine = seq;
      this.loadingMore = true;
      this.moreError = '';
      try {
        const res = await api('/api/search?q=' + encodeURIComponent(this.shownQuery) + '&offset=' + this.nextOffset);
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const data = await res.json();
        if (mine !== seq) return;
        // MusicBrainz can shift between pages; never show an album twice.
        const seen = new Set(this.albums.map((a) => a.id));
        this.albums = this.albums.concat(data.albums.filter((a) => !seen.has(a.id)));
        this.libraryOk = this.libraryOk && data.libraryOk;
        this.hasMore = data.hasMore;
        this.nextOffset = data.offset + data.albums.length;
      } catch (e) {
        if (mine === seq) this.moreError = e.message;
      } finally {
        if (mine === seq) this.loadingMore = false;
      }
    },

    clear() {
      this.query = '';
      this.search();
    },

    // replaceQuery keeps the search in the URL without touching an open album's hash.
    replaceQuery(search) {
      history.replaceState(history.state, '', location.pathname + search + location.hash);
    },

    // --- album sheet ---

    open(card) {
      history.pushState({ album: card.id }, '', location.pathname + location.search + '#album=' + card.id);
      this.show(card);
    },

    async show(card) {
      // Switching straight from one album to another (history navigation):
      // close the open sheet first, so nothing re-evaluates against a half
      // reset state.
      if (this.selected) {
        this.selected = null;
        await PetiteVue.nextTick();
      }
      this.detail = null;
      this.detailError = '';
      this.requestNote = '';
      this.requestError = '';
      this.requesting = false;
      this.detailLoading = true;
      this.selected = card;
      document.body.style.overflow = 'hidden';
      this.loadDetail(card.id);
    },

    async loadDetail(id) {
      const mine = ++detailSeq;
      try {
        const res = await api('/api/album/' + id);
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const d = await res.json();
        if (mine === detailSeq) this.detail = d;
        // Keep the card behind the sheet in step with what Lidarr just said.
        const card = this.albums.find((a) => a.id === id);
        if (card && d.libraryOk) card.status = d.status;
      } catch (e) {
        if (mine === detailSeq) this.detailError = e.message;
      } finally {
        if (mine === detailSeq) this.detailLoading = false;
      }
    },

    // request asks Lidarr to get the open album: add it if need be, monitor it and
    // search for it, so a download starts if a release is found. The header proves the call came from this page: the
    // server refuses a write without it.
    async request() {
      if (this.requesting || !this.selected || !this.detail || !this.detail.canRequest) return;
      const id = this.selected.id;
      this.requesting = true;
      this.requestError = '';
      this.requestNote = '';
      try {
        const res = await api('/api/album/' + id + '/request', { method: 'POST', headers: { 'X-Albumreq': '1' } });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const data = await res.json();
        if (!this.selected || this.selected.id !== id) return;
        this.requestNote = {
          added: 'Requested. Lidarr is searching for it now.',
          pending: 'Requested. An approver will look at it before anything is downloaded.',
          added_unsearched: 'Added to Lidarr, but the search could not be started, so Lidarr will not look for it by itself. Try again in a moment, or search for it in Lidarr.',
        }[data.result] || 'Lidarr already has this album and is looking for it.';
        await this.loadDetail(id);
      } catch (e) {
        if (this.selected && this.selected.id === id) this.requestError = e.message;
      } finally {
        this.requesting = false;
      }
    },

    close() {
      if (!this.selected) return;
      detailSeq++;
      // `detail` is deliberately left as is: nulling it makes petite-vue
      // re-evaluate the sheet's expressions before it removes the sheet. It is
      // reset when the next album opens, while the sheet is not rendered.
      this.selected = null;
      document.body.style.overflow = '';
      // Back to the search URL; if the sheet was opened from a pasted link
      // there is no history entry to pop, so rewrite it instead.
      if (history.state && history.state.album) history.back();
      else this.replaceQuery(location.search);
      if (location.hash) history.replaceState(null, '', location.pathname + location.search);
    },

    syncFromHash() {
      const page = PAGE_OF_HASH[location.hash];
      if (page) {
        if (this.selected) {
          detailSeq++;
          this.selected = null;
          document.body.style.overflow = '';
        }
        if (page !== this.page) this.showPage(page);
        return;
      }
      // An album's own hash opens its sheet over whatever page is showing (so Back
      // from a sheet opened on My requests returns there); anything else is search.
      if (!/^#album=/.test(location.hash) && this.page !== 'search') this.showPage('search');
      const m = /^#album=(.+)$/.exec(location.hash);
      if (m && UUID.test(m[1])) {
        if (!this.selected || this.selected.id !== m[1]) {
          this.show(this.albums.find((a) => a.id === m[1]) || { id: m[1], secondaryTypes: [], cover: '/api/cover/' + m[1] });
        }
      } else if (this.selected) {
        detailSeq++;
        this.selected = null;
        document.body.style.overflow = '';
      }
    },

    // view merges what the search result already told us with the fuller
    // detail, so the sheet has a title the moment it opens and still works
    // when the metadata service is down.
    view() {
      const s = this.selected || {};
      const d = this.detail && this.detail.metadataOk ? this.detail : {};
      return {
        id: s.id,
        title: d.title || s.title || '',
        artist: d.artist || s.artist || '',
        type: d.type || s.type || '',
        secondaryTypes: d.secondaryTypes || s.secondaryTypes || [],
        year: d.year || s.year || 0,
        status: this.detail ? this.detail.status : s.status || '',
      };
    },

    // coverSrc is empty (no request) while no album is open, so the sheet's
    // image never asks for /api/cover/undefined as the sheet is torn down.
    coverSrc() {
      return this.selected ? '/api/cover/' + this.selected.id + '?size=500' : '';
    },

    tracks() {
      return this.detail ? this.detail.trackList.tracks : [];
    },

    // discs groups the track list by disc, for multi-disc albums.
    discs() {
      const out = [];
      for (const t of this.tracks()) {
        let g = out.find((x) => x.disc === t.disc);
        if (!g) out.push((g = { disc: t.disc, tracks: [] }));
        g.tracks.push(t);
      }
      return out;
    },

    trackNote() {
      const d = this.detail;
      if (!d) return '';
      const tl = d.trackList;
      if (tl.source === 'library' && d.library) {
        return `From your library: ${d.library.tracksHave} of ${d.library.tracksTotal} tracks on disk`;
      }
      if (tl.source === 'edition' && tl.edition) {
        const e = tl.edition;
        const bits = [e.date, e.format, e.country].filter(Boolean).join(' · ');
        return `Edition: ${bits || 'unknown'} · ${e.trackCount} tracks, the most common of ${e.editionsTotal} editions`;
      }
      return '';
    },

    // --- requests ---

    // send POSTs JSON the way the server's write guard wants, and throws the
    // server's own words (with .status) on a failure.
    async send(url, body) {
      const res = await api(url, {
        method: 'POST',
        headers: { 'X-Albumreq': '1', 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!res.ok) {
        const err = new Error((await res.text()).trim() || res.statusText);
        err.status = res.status;
        throw err;
      }
      return res.json();
    },

    async fetchRequests(scope) {
      const res = await api('/api/requests?scope=' + scope, { cache: 'no-store' });
      if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
      return (await res.json()).requests;
    },

    async loadMine() {
      this.mineError = '';
      try {
        this.mine = await this.fetchRequests('mine');
        this.mineLoaded = true;
      } catch (e) {
        this.mineError = e.message;
      }
    },

    async loadQueue() {
      this.queueError = '';
      try {
        if (this.tab === 'history') this.history = await this.fetchRequests('all');
        else {
          this.queue = await this.fetchRequests('pending');
          this.pendingCount = this.queue.length;
        }
        this.queueLoaded = true;
      } catch (e) {
        this.queueError = e.message;
      }
    },

    async refreshPendingCount() {
      try {
        this.pendingCount = (await this.fetchRequests('pending')).length;
      } catch (e) {
        /* the badge is a convenience; the page reports real errors */
      }
    },

    setTab(tab) {
      if (this.tab === tab) return;
      this.tab = tab;
      this.queueLoaded = false;
      this.notice = '';
      this.declining = null;
      this.loadQueue();
    },

    queueShown() {
      return this.tab === 'history' ? this.history : this.queue;
    },

    // openRequest opens the album's sheet over the current page.
    openRequest(r) {
      this.open({ id: r.albumId, title: r.title, artist: r.artist, secondaryTypes: [], cover: '/api/cover/' + r.albumId });
    },

    startDecline(r) {
      this.declining = r.id;
      this.declineReason = '';
      this.rowError[r.id] = '';
    },

    cancelDecline() {
      this.declining = null;
    },

    // approve asks the server to approve: that is what makes Lidarr add, monitor and
    // search for the album, so it is a real action and says what it did. fromSheet
    // refreshes the open sheet instead of the queue.
    async approve(r, fromSheet) {
      if (this.busy[r.id]) return;
      this.busy[r.id] = true;
      this.rowError[r.id] = '';
      this.notice = '';
      try {
        const out = await this.send('/api/requests/' + r.id + '/approve');
        this.notice = out.result === 'added_unsearched'
          ? 'Approved ' + r.title + '. It is in Lidarr, but the search could not be started.'
          : 'Approved ' + r.title + '. Lidarr is searching for it now.';
        await this.afterDecision(r, fromSheet);
      } catch (e) {
        this.rowError[r.id] = e.status === 409 ? 'Someone else has already decided this.' : e.message;
        if (e.status === 409) await this.afterDecision(r, fromSheet);
      } finally {
        this.busy[r.id] = false;
      }
    },

    async decline(r, fromSheet) {
      if (this.busy[r.id]) return;
      this.busy[r.id] = true;
      this.rowError[r.id] = '';
      this.notice = '';
      try {
        await this.send('/api/requests/' + r.id + '/decline', { reason: this.declineReason.trim() });
        this.notice = 'Declined ' + r.title + '.';
        this.declining = null;
        await this.afterDecision(r, fromSheet);
      } catch (e) {
        this.rowError[r.id] = e.status === 409 ? 'Someone else has already decided this.' : e.message;
        if (e.status === 409) await this.afterDecision(r, fromSheet);
      } finally {
        this.busy[r.id] = false;
      }
    },

    // afterDecision brings the screen up to date: the sheet if it was decided there,
    // and always the page behind it, which would otherwise keep showing the request.
    async afterDecision(r, fromSheet) {
      if (fromSheet && this.selected && this.selected.id === r.albumId) await this.loadDetail(r.albumId);
      if (this.page === 'approvals') await this.loadQueue();
      else if (this.page === 'requests') await this.loadMine();
      else this.refreshPendingCount();
    },

    reqStateLabel(r) {
      return { pending: 'Waiting for approval', approved: 'Approved', declined: 'Declined' }[r.state] || r.state;
    },

    // --- settings: users ---

    async loadUsers() {
      this.usersError = '';
      try {
        const res = await api('/api/users', { cache: 'no-store' });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        this.users = (await res.json()).users;
        this.usersLoaded = true;
      } catch (e) {
        this.usersError = e.message;
      }
    },

    // setApprover saves at once: a checkbox that waits for a Save button invites
    // forgetting it, and the server answers with the whole list, so what is shown
    // is what was stored.
    async setApprover(u, approver) {
      if (this.userBusy[u.sub]) return;
      this.userBusy[u.sub] = true;
      this.usersError = '';
      try {
        this.users = (await this.send('/api/users/approver', { sub: u.sub, approver })).users;
      } catch (e) {
        this.usersError = e.message;
        await this.loadUsers(); // put the checkbox back to what is stored
      } finally {
        this.userBusy[u.sub] = false;
      }
    },

    // --- formatting ---

    // ago says how long ago an RFC 3339 time was, in the largest sensible unit.
    ago(iso) {
      const t = Date.parse(iso);
      if (!t) return '';
      const s = Math.max(0, Math.round((Date.now() - t) / 1000));
      if (s < 60) return 'just now';
      const units = [['day', 86400], ['hour', 3600], ['minute', 60]];
      for (const [name, n] of units) {
        if (s >= n) {
          const v = Math.floor(s / n);
          return v + ' ' + name + (v === 1 ? '' : 's') + ' ago';
        }
      }
      return '';
    },


    statusLabel(s) {
      return STATUS_LABELS[s] || s;
    },

    // kindClass maps an album type to its pill colour class (album, ep, ...).
    kindClass(a) {
      return (a.type || 'album').toLowerCase().replace(/[^a-z]/g, '');
    },

    // hueFor picks a stable hue per title for the cover fallback, the same
    // palette reaparr uses.
    hueFor(title) {
      const hues = [290, 150, 60, 200, 25, 330];
      let h = 0;
      for (const c of title || '') h = (h * 31 + c.charCodeAt(0)) >>> 0;
      return hues[h % hues.length];
    },

    fmtDuration(ms) {
      if (!ms) return '';
      const s = Math.round(ms / 1000);
      return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
    },

    fmtSize(bytes) {
      if (!bytes) return '';
      const u = ['B', 'KB', 'MB', 'GB', 'TB'];
      let i = 0;
      let n = bytes;
      while (n >= 1024 && i < u.length - 1) {
        n /= 1024;
        i++;
      }
      return (i ? n.toFixed(n >= 100 ? 0 : 1) : n) + ' ' + u[i];
    },
  };
}

PetiteVue.createApp({ App }).mount('#app');
