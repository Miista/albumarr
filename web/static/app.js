const STATUS_LABELS = {
  available: 'Available',
  processing: 'Processing',
  requested: 'Requested',
  notfound: 'Not found',
  inlidarr: 'In Lidarr',
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
    cfg: null, // GET /api/settings (admins)
    cfgError: '',
    form: { url: '', api_key: '', root_folder: '', quality_profile_id: 0, metadata_profile_id: 0 },
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
      history.pushState(null, '', location.pathname + (page === 'settings' ? '#/settings' : location.search));
      this.showPage(page);
    },

    showPage(page) {
      this.page = page;
      if (page === 'settings') this.loadSettings();
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
        metadata_profile_id: v.metadata_profile_id,
      };
    },

    // mayRequest: a plain user's request would have to wait for an approval that
    // does not exist yet, so only approvers and admins are offered the button.
    mayRequest() {
      return !!this.me && (this.me.role === 'approver' || this.me.role === 'admin') && this.me.requests_enabled;
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
    // value that is set or locked: the only root folder Lidarr has, and the
    // metadata profile named "None", which imports none of an artist's other
    // albums so a request adds just the album asked for. The user still saves.
    applyDefaults() {
      const o = this.options;
      if (!o) return;
      if (!this.lock('lidarr.root_folder') && !this.form.root_folder && o.rootFolders.length === 1) {
        this.form.root_folder = o.rootFolders[0];
      }
      if (!this.lock('lidarr.metadata_profile_id') && !this.form.metadata_profile_id) {
        const none = o.metadataProfiles.find((p) => p.name.trim().toLowerCase() === 'none');
        if (none) this.form.metadata_profile_id = none.id;
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
      for (const k of ['quality_profile_id', 'metadata_profile_id']) {
        const n = Number(f[k]) || 0;
        if (!this.lock('lidarr.' + k) && n !== v[k]) out[k] = n;
      }
      return out;
    },

    dirty() {
      return Object.keys(this.changes()).length > 0;
    },

    saveText() {
      if (this.saving) return 'Saving…';
      if (this.saveState === 'failed') return this.saveError;
      if (this.saveState === 'saved') return 'Saved';
      return this.dirty() ? 'Unsaved changes' : 'No changes';
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
      api('/api/me', { cache: 'no-store' }).then((r) => r.ok && r.json()).then((m) => { if (m) this.me = m; }).catch(() => {});
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
      if (location.hash === '#/settings') {
        if (this.selected) {
          detailSeq++;
          this.selected = null;
          document.body.style.overflow = '';
        }
        this.showPage('settings');
        return;
      }
      if (this.page !== 'search') this.showPage('search');
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

    // --- formatting ---

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
