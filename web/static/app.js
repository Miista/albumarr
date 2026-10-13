const STATUS_LABELS = {
  available: 'Available',
  processing: 'Processing',
  requested: 'Requested',
  notfound: 'Not found',
  pending: 'Pending approval',
};

// MusicBrainz answers about one search a second, so wait for a pause in the
// typing rather than searching on every few keystrokes.
const DEBOUNCE_MS = 600;
const SWIPE_W = 96; // how far a row slides to show its Delete button
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
const HASHES = { requests: '#/requests', issues: '#/issues', settings: '#/settings', account: '#/account' };
const PAGE_OF_HASH = { '#/requests': 'requests', '#/approvals': 'requests', '#/issues': 'issues', '#/settings': 'settings', '#/settings/users': 'settings', '#/settings/notifications': 'settings', '#/account': 'account' };
const TAB_OF_HASH = { '#/settings/users': 'users', '#/settings/notifications': 'notifications' };
const HASH_OF_TAB = { users: '#/settings/users', notifications: '#/settings/notifications', lidarr: '#/settings' };

function App() {
  // Kept out of the reactive scope: timers and the in-flight counters have no
  // reason to trigger a re-render.
  let timer = null;
  let seq = 0;
  let detailSeq = 0;
  let artistSeq = 0;

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

    // artists: matches for the search, and the artist page being browsed
    artistHits: [],
    artistHitsLoading: false, // the artist row is still being fetched: show placeholders
    artist: null, // { id, name, disambiguation, type, albums, libraryOk } once loading starts
    artistLoading: false,
    artistError: '',
    artistGroup: 'albums', // albums | eps | singles | more

    selected: null, // the card (or just { id }) behind the open sheet
    detail: null,
    detailLoading: false,
    detailError: '',
    requesting: false,
    requestNote: '',
    requestError: '',

    // --- settings page ---

    page: 'search',
    settingsTab: 'lidarr', // lidarr | notifications | users
    me: null, // GET /api/me: who is signed in and what they may do

    // requests: a person's own, or everyone's for approvers and admins
    reqs: [],
    prog: {}, // album id -> where Lidarr has it: { status, stage }
    reqsLoaded: false,
    reqsError: '',
    sort: 'recent', // recent | modified
    filter: 'all', // all | pending | approved | processing | failed | declined | available
    menuOpen: false,
    pendingCount: 0,
    notice: '',
    busy: {}, // request id -> an approve/decline is in flight
    rowError: {}, // request id -> why the last one failed
    declining: null, // the request id whose decline form is open
    deleting: null, // the request id waiting for the second press of Delete
    swiped: null, // the request id whose row is slid open to show Delete (touch)
    swipedAt: 0,
    drag: null, // a swipe in progress: { id, x0, y0, dx, base, active }
    declineReason: '',

    // Issues (admins)
    issues: [],
    issuesLoaded: false,
    issuesError: '',
    issuesChecking: false,
    issueCount: 0,

    // Settings -> Users (admins)
    users: [],
    usersLoaded: false,
    usersError: '',
    userBusy: {},
    cfg: null, // GET /api/settings (admins)
    cfgError: '',
    form: { url: '', api_key: '', root_folder: '', quality_profile_id: 0, web_url: '' },
    options: null, // root folders and profiles Lidarr offered on the last successful test
    testing: false,
    testResult: null,
    saving: false,
    saveState: '',
    saveError: '',
    warning: '',

    // Notifications (admins): the ntfy server and the shared channel
    ncfg: null, // GET /api/notifications
    nerror: '',
    nform: null,
    ntesting: false,
    ntest: null,
    nsaving: false,
    nstate: '',
    nsaveError: '',

    // Account (everyone): where I hear about my own requests
    acct: null, // GET /api/me/notifications
    acctError: '',
    aform: { topic: '', types: [] },
    asaving: false,
    astate: '',
    asaveError: '',
    atesting: false,
    atest: null,

    go(page) {
      if (page === 'search') {
        // The Search tab is also the way out of an artist's page.
        if (this.artist && this.page === 'search') {
          if (this.selected) this.close();
          this.leaveArtist(true);
          return;
        }
        this.artist = null;
      }
      if (page === this.page) return;
      if (this.selected) this.close();
      history.pushState(null, '', location.pathname + (HASHES[page] || location.search));
      this.showPage(page);
    },

    showPage(page) {
      this.page = page;
      this.notice = '';
      this.declining = null;
      this.deleting = null;
      if (this.me) this.enter();
    },

    // mayView: which pages a role gets. The server enforces the same on every route;
    // this only keeps someone from landing on a page they cannot use.
    mayView(page) {
      if (!this.me || this.me.role === 'none') return false;
      if (page === 'settings' || page === 'issues') return this.me.role === 'admin';
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
        this.loadNotifications();
      } else if (page === 'account') {
        this.loadAccount();
      } else if (page === 'requests') {
        this.loadRequests();
      } else if (page === 'issues') {
        this.loadIssues(false);
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
        web_url: v.web_url || '',
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
      if (!this.lock('lidarr.web_url') && f.web_url.trim() !== (v.web_url || '')) out.web_url = f.web_url.trim();
      for (const k of ['quality_profile_id']) {
        const n = Number(f[k]) || 0;
        if (!this.lock('lidarr.' + k) && n !== v[k]) out[k] = n;
      }
      return out;
    },

    dirty() {
      return Object.keys(this.changes()).length > 0;
    },

    // The save bar is Reaparr's: it appears only when it has something to say (a
    // failure, a save under way, a moment's "saved", or unsaved changes) and is
    // hidden otherwise. "Saved" wins for its few seconds even if the form is dirty
    // again at once: after the connection is saved the only root folder is
    // pre-filled, which is a change still to confirm, and showing "unsaved changes"
    // straight away would read as if the save had not worked.
    bar() {
      if (this.saveState === 'failed') return { cls: 'failed', msg: this.saveError };
      if (this.saving) return { cls: 'dirty', msg: 'Saving…' };
      if (this.saveState === 'saved') return { cls: 'saved', msg: 'Settings saved' };
      if (this.dirty()) return { cls: 'dirty', msg: 'You have unsaved changes' };
      return { cls: '', msg: '' };
    },

    // discard puts the form back to what the server has.
    discard() {
      if (!this.cfg) return;
      this.applySettings(this.cfg);
      this.saveState = '';
      this.saveError = '';
    },

    goSettingsTab(tab) {
      if (tab === this.settingsTab) return;
      this.settingsTab = tab;
      history.pushState(null, '', location.pathname + HASH_OF_TAB[tab]);
    },

    // openLink follows an Issues link to the settings tab that can fix it.
    openLink(link) {
      if (this.selected) this.close();
      if (!link.startsWith('#/settings')) return this.go('issues');
      this.settingsTab = TAB_OF_HASH[link] || 'lidarr';
      history.pushState(null, '', location.pathname + link);
      this.showPage('settings');
    },

    // ---- Settings -> Notifications ----

    nlock(key) {
      return this.ncfg && this.ncfg.locks ? this.ncfg.locks['notifications.' + key] : null;
    },

    nlockText(key) {
      const l = this.nlock(key);
      if (!l) return '';
      if (l.reason === 'env') return 'Managed by ' + l.env_var;
      if (l.reason === 'paired') return 'Locked with ' + l.env_var;
      return 'Read-only';
    },

    async loadNotifications() {
      this.nerror = '';
      try {
        const res = await api('/api/notifications', { cache: 'no-store' });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        this.applyNotifications(await res.json());
      } catch (e) {
        this.nerror = e.message;
      }
    },

    // Secrets never come to the page: their fields start empty, and empty means keep.
    applyNotifications(c) {
      this.ncfg = c;
      const v = c.values;
      this.nform = {
        enabled: v.enabled, url: v.url, topic: v.topic, tags: v.tags, auth: v.auth,
        username: v.username, password: '', token: '', priority: v.priority,
        embed_poster: v.embed_poster, types: (v.types || []).slice(), app_url: v.app_url,
      };
    },

    sameSet(a, b) {
      return a.length === b.length && a.every((x) => b.includes(x));
    },

    nchanges() {
      if (!this.ncfg || !this.nform) return {};
      const v = this.ncfg.values;
      const f = this.nform;
      const out = {};
      const text = (k) => { if (!this.nlock(k) && f[k].trim() !== v[k]) out[k] = f[k].trim(); };
      for (const k of ['url', 'topic', 'tags', 'username', 'app_url']) text(k);
      if (!this.nlock('auth') && f.auth !== v.auth) out.auth = f.auth;
      for (const k of ['enabled', 'embed_poster']) if (!this.nlock(k) && f[k] !== v[k]) out[k] = f[k];
      if (!this.nlock('priority') && Number(f.priority) !== v.priority) out.priority = Number(f.priority);
      if (!this.nlock('password') && f.password !== '') out.password = f.password;
      if (!this.nlock('token') && f.token !== '') out.token = f.token;
      if (!this.nlock('types') && !this.sameSet(f.types, v.types)) out.types = f.types.slice();
      return out;
    },

    ndirty() {
      return Object.keys(this.nchanges()).length > 0;
    },

    nbar() {
      if (this.nstate === 'failed') return { cls: 'failed', msg: this.nsaveError };
      if (this.nsaving) return { cls: 'dirty', msg: 'Saving…' };
      if (this.nstate === 'saved') return { cls: 'saved', msg: 'Settings saved' };
      if (this.ndirty()) return { cls: 'dirty', msg: 'You have unsaved changes' };
      return { cls: '', msg: '' };
    },

    ndiscard() {
      if (!this.ncfg) return;
      this.applyNotifications(this.ncfg);
      this.nstate = '';
      this.nsaveError = '';
    },

    toggleType(list, id, on) {
      const i = list.indexOf(id);
      if (on && i < 0) list.push(id);
      if (!on && i >= 0) list.splice(i, 1);
    },

    async saveNotifications() {
      const body = this.nchanges();
      if (this.nsaving || !Object.keys(body).length) return;
      this.nsaving = true;
      this.nstate = '';
      try {
        const res = await api('/api/notifications', {
          method: 'POST',
          headers: { 'X-Albumreq': '1', 'Content-Type': 'application/json' },
          body: JSON.stringify({ ntfy: body }),
        });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        this.applyNotifications((await res.json()).settings);
        this.nstate = 'saved';
        setTimeout(() => { if (this.nstate === 'saved') this.nstate = ''; }, 3000);
        this.ntest = null;
        this.loadIssues(true, true);
      } catch (e) {
        this.nsaveError = e.message;
        this.nstate = 'failed';
      } finally {
        this.nsaving = false;
      }
    },

    // The test sends what is on the page, so a server can be tried before it is
    // saved. A secret left empty is the saved one; the server decides if that is allowed.
    async testNotifications() {
      if (this.ntesting || !this.nform) return;
      this.ntesting = true;
      this.ntest = null;
      const f = this.nform;
      try {
        const res = await api('/api/notifications/test', {
          method: 'POST',
          headers: { 'X-Albumreq': '1', 'Content-Type': 'application/json' },
          body: JSON.stringify({
            url: f.url.trim(), topic: f.topic.trim(), auth: f.auth, username: f.username.trim(),
            password: f.password, token: f.token, priority: Number(f.priority), tags: f.tags.trim(),
          }),
        });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        this.ntest = await res.json();
      } catch (e) {
        this.ntest = { ok: false, message: e.message };
      } finally {
        this.ntesting = false;
      }
    },

    // ---- Account ----

    async loadAccount() {
      this.acctError = '';
      try {
        const res = await api('/api/me/notifications', { cache: 'no-store' });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        this.applyAccount(await res.json());
      } catch (e) {
        this.acctError = e.message;
      }
    },

    applyAccount(v) {
      this.acct = v;
      this.aform = { topic: v.topic, types: (v.types || []).slice() };
    },

    adirty() {
      return !!this.acct && (this.aform.topic.trim() !== this.acct.topic || !this.sameSet(this.aform.types, this.acct.types));
    },

    abar() {
      if (this.astate === 'failed') return { cls: 'failed', msg: this.asaveError };
      if (this.asaving) return { cls: 'dirty', msg: 'Saving…' };
      if (this.astate === 'saved') return { cls: 'saved', msg: 'Saved' };
      if (this.adirty()) return { cls: 'dirty', msg: 'You have unsaved changes' };
      return { cls: '', msg: '' };
    },

    adiscard() {
      if (this.acct) this.applyAccount(this.acct);
      this.astate = '';
      this.asaveError = '';
    },

    async saveAccount() {
      if (this.asaving || !this.adirty()) return;
      this.asaving = true;
      this.astate = '';
      try {
        const res = await api('/api/me/notifications', {
          method: 'POST',
          headers: { 'X-Albumreq': '1', 'Content-Type': 'application/json' },
          body: JSON.stringify({ topic: this.aform.topic.trim(), types: this.aform.types }),
        });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        this.applyAccount(await res.json());
        this.atest = null;
        this.astate = 'saved';
        setTimeout(() => { if (this.astate === 'saved') this.astate = ''; }, 3000);
      } catch (e) {
        this.asaveError = e.message;
        this.astate = 'failed';
      } finally {
        this.asaving = false;
      }
    },

    // Sends to the saved topic, so unsaved edits are saved first by the person.
    async testAccount() {
      if (this.atesting) return;
      this.atesting = true;
      this.atest = null;
      try {
        const res = await api('/api/me/notifications/test', { method: 'POST', headers: { 'X-Albumreq': '1', 'Content-Type': 'application/json' }, body: '{}' });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        this.atest = await res.json();
      } catch (e) {
        this.atest = { ok: false, message: e.message };
      } finally {
        this.atesting = false;
      }
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
        this.loadIssues(true, true);
      } catch (e) {
        this.saveError = e.message;
        this.saveState = 'failed';
      } finally {
        this.saving = false;
      }
    },

    mounted() {
      // What is downloading moves on its own, so the lists that show it refresh
      // while they are in view.
      setInterval(() => {
        if (document.hidden || !this.me) return;
        if (this.page === 'requests') this.loadRequests();
      }, 15000);
      window.addEventListener('click', (e) => {
        if (this.menuOpen && !e.target.closest('.menu-wrap')) this.menuOpen = false;
        if (this.swiped !== null && !e.target.closest('.swipe-action')) this.swiped = null;
      });
      window.addEventListener('keydown', (e) => { if (e.key === 'Escape') this.menuOpen = false; });
      api('/api/me', { cache: 'no-store' }).then((r) => r.ok && r.json()).then((m) => {
        if (!m) return;
        this.me = m;
        this.enter();
        if (this.canApprove()) this.refreshPendingCount();
        if (this.me.role === 'admin') this.loadIssues(false, true);
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
        this.artistHits = [];
        this.artistHitsLoading = false;
        this.albums = [];
        this.hasMore = false;
        this.searched = false;
        this.loading = false;
        this.error = '';
        this.replaceQuery('');
        return;
      }

      // Nothing from the last search stays on screen: it would be someone else's results.
      this.albums = [];
      this.hasMore = false;
      this.artistHits = [];
      this.artistHitsLoading = true;
      this.loading = true;
      this.error = '';
      this.replaceQuery('?q=' + encodeURIComponent(q));
      this.searchArtists(q, mine); // alongside the albums; a failure only costs the artist row
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

    async searchArtists(q, mine) {
      this.artistHitsLoading = true;
      try {
        const res = await api('/api/artists?q=' + encodeURIComponent(q));
        if (!res.ok) throw new Error(res.statusText);
        const data = await res.json();
        if (mine === seq) this.artistHits = data.artists || [];
      } catch (e) {
        if (mine === seq) this.artistHits = [];
      } finally {
        if (mine === seq) this.artistHitsLoading = false;
      }
    },

    // --- an artist's page ---

    openArtist(hit) {
      history.pushState({ artist: hit.id }, '', location.pathname + location.search + '#artist=' + hit.id);
      this.showArtist(hit.id, hit.name, hit.disambiguation);
    },

    async showArtist(id, name, disambiguation) {
      const mine = ++artistSeq;
      this.artist = { id, name: name || '', disambiguation: disambiguation || '', albums: [], libraryOk: true };
      this.artistError = '';
      this.artistLoading = true;
      window.scrollTo(0, 0);
      try {
        const res = await api('/api/artist/' + id, { cache: 'no-store' });
        if (!res.ok) throw new Error(res.status === 404 ? 'That artist could not be found.' : (await res.text()).trim() || res.statusText);
        const data = await res.json();
        if (mine !== artistSeq) return;
        this.artist = data;
        this.artistGroup = ['albums', 'eps', 'singles', 'more'].find((g) => data.albums.some((a) => a.group === g)) || 'albums';
      } catch (e) {
        if (mine === artistSeq) this.artistError = e.message;
      } finally {
        if (mine === artistSeq) this.artistLoading = false;
      }
    },

    // leaveArtist goes back to the search the artist was opened from. Opened by a
    // pasted link there is nothing to go back to, so the address is rewritten.
    leaveArtist(fromTab) {
      artistSeq++;
      if (!fromTab && history.state && history.state.artist) {
        history.back();
        return;
      }
      this.artist = null;
      history.replaceState(null, '', location.pathname + location.search);
    },

    artistGroups() {
      if (!this.artist) return [];
      const names = { albums: 'Albums', eps: 'EPs', singles: 'Singles', more: 'More' };
      return Object.keys(names)
        .map((g) => ({ id: g, label: names[g], n: this.artist.albums.filter((a) => a.group === g).length }))
        .filter((g) => g.n);
    },

    // gridAlbums is what the poster grid shows: the search results, or one group of
    // the open artist's albums.
    gridAlbums() {
      return this.artist ? this.artist.albums.filter((a) => a.group === this.artistGroup) : this.albums;
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
        const card = this.albums.find((a) => a.id === id) || (this.artist && this.artist.albums.find((a) => a.id === id));
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
        // The request carries what the card already showed: the artist's ID and the
        // names for the approver's queue.
        const v = this.view();
        const res = await api('/api/album/' + id + '/request', {
          method: 'POST',
          headers: { 'X-Albumreq': '1', 'Content-Type': 'application/json' },
          body: JSON.stringify({ artistId: v.artistId, title: v.title, artist: v.artist }),
        });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const data = await res.json();
        if (!this.selected || this.selected.id !== id) return;
        this.requestNote = {
          added: 'Requested.',
          pending: 'Requested. An approver will look at it first.',
          added_unsearched: 'Requested, but it could not be started right now. Try again in a moment.',
        }[data.result] || 'This album is already on its way.';
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
      // Links in old notifications point at the approvals page: that is Requests, pending.
      if (location.hash === '#/approvals') {
        this.filter = 'pending';
        history.replaceState(null, '', location.pathname + '#/requests');
      }
      const page = PAGE_OF_HASH[location.hash];
      if (page === 'settings') this.settingsTab = TAB_OF_HASH[location.hash] || 'lidarr';
      if (page) {
        if (this.selected) {
          detailSeq++;
          this.selected = null;
          document.body.style.overflow = '';
        }
        if (page !== this.page) this.showPage(page);
        return;
      }
      // An artist's hash shows their page in Search; an album's hash opens its sheet
      // over whatever is showing, so the artist's page stays behind it.
      const am = /^#artist=(.+)$/.exec(location.hash);
      if (am && UUID.test(am[1])) {
        if (this.selected) {
          detailSeq++;
          this.selected = null;
          document.body.style.overflow = '';
        }
        if (this.page !== 'search') this.showPage('search');
        if (!this.artist || this.artist.id !== am[1]) this.showArtist(am[1]);
        return;
      }
      if (!/^#album=/.test(location.hash)) this.artist = null;
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
        artistId: d.artistId || s.artistId || '',
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
      const body = await res.json();
      // Lidarr's view of the approved ones, by album: merged, so a page that asked
      // about fewer albums does not wipe what another list showed.
      this.prog = Object.assign({}, this.prog, body.progress || {});
      return body.requests;
    },

    // loadRequests fetches what this person may see: their own; with approving
    // rights also what waits on them; an admin sees everyone's.
    async loadRequests() {
      this.reqsError = '';
      try {
        let list;
        if (this.me.role === 'admin') list = await this.fetchRequests('all');
        else if (this.canApprove()) {
          const [mine, waiting] = await Promise.all([this.fetchRequests('mine'), this.fetchRequests('pending')]);
          const seen = new Set(mine.map((r) => r.id));
          list = mine.concat(waiting.filter((r) => !seen.has(r.id)));
        } else list = await this.fetchRequests('mine');
        this.reqs = list;
        if (this.canApprove()) this.pendingCount = list.filter((r) => r.state === 'pending').length;
        this.reqsLoaded = true;
      } catch (e) {
        this.reqsError = e.message;
      }
    },

    async refreshPendingCount() {
      try {
        this.pendingCount = (await this.fetchRequests('pending')).length;
      } catch (e) {
        /* the badge is a convenience; the page reports real errors */
      }
    },

    // kind puts a request in one of the filters, each in exactly one: approved means
    // approved and not yet on its way; processing is in the download queue.
    kind(r) {
      if (r.state !== 'approved') return r.state;
      const p = this.prog[r.albumId];
      if (r.lidarrResult === 'added_unsearched' || (p && p.stage === 'stalled')) return 'failed';
      if (p && p.status === 'available') return 'available';
      if (p && p.stage) return 'processing';
      return 'approved';
    },

    // shown is the list as filtered and sorted. Last modified is when it was last
    // decided, or asked if it has not been.
    shown() {
      const when = (r) => Date.parse(this.sort === 'modified' && r.decidedAt ? r.decidedAt : r.createdAt) || 0;
      return this.reqs
        .filter((r) => this.filter === 'all' || this.kind(r) === this.filter)
        .sort((a, b) => when(b) - when(a) || b.id - a.id);
    },

    pick(group, value) {
      this[group] = value;
      this.menuOpen = false;
    },

    filterLabel() {
      return { all: 'All', pending: 'Pending', approved: 'Approved', processing: 'Processing', failed: 'Failed', declined: 'Declined', available: 'Available' }[this.filter];
    },

    // openRequest opens the album's sheet over the current page.
    openRequest(r) {
      // A tap on a row that is slid open (or just swiped) closes it instead of opening.
      if (this.swiped !== null || Date.now() - this.swipedAt < 400) {
        this.swiped = null;
        return;
      }
      this.open({ id: r.albumId, title: r.title, artist: r.artist, secondaryTypes: [], cover: '/api/cover/' + r.albumId });
    },

    // --- swipe a row to reveal Delete, as on iOS (touch only) ---

    swipeStart(e, r) {
      if (e.pointerType !== 'touch' || !this.canDelete(r)) return;
      this.drag = { id: r.id, x0: e.clientX, y0: e.clientY, dx: 0, active: false, base: this.swiped === r.id ? -SWIPE_W : 0 };
    },

    swipeMove(e, r) {
      const d = this.drag;
      if (!d || d.id !== r.id) return;
      const dx = e.clientX - d.x0;
      const dy = e.clientY - d.y0;
      if (!d.active) {
        if (Math.abs(dx) > 10 && Math.abs(dx) > Math.abs(dy) * 1.5) d.active = true; // sideways: ours
        else if (Math.abs(dy) > 10) this.drag = null; // downwards: a scroll
        if (!d.active) return;
        if (this.swiped !== null && this.swiped !== r.id) this.swiped = null; // one open row at a time
      }
      d.dx = dx;
    },

    swipeEnd(e, r) {
      const d = this.drag;
      this.drag = null;
      if (!d || !d.active) return;
      this.swipedAt = Date.now(); // the click that follows a swipe must not open the album
      this.swiped = this.shift(d) < -SWIPE_W / 2 ? r.id : null;
    },

    shift(d) {
      return Math.min(0, Math.max(-SWIPE_W, d.base + d.dx));
    },

    // rowShift is how far the row is slid left: with the finger while dragging,
    // else all the way open or shut.
    rowShift(r) {
      const d = this.drag;
      if (d && d.active && d.id === r.id) return this.shift(d);
      return this.swiped === r.id ? -SWIPE_W : 0;
    },

    // removeNow is the revealed Delete button: the swipe was the deliberate step.
    removeNow(r) {
      this.deleting = r.id;
      this.remove(r);
    },

    // canDelete mirrors the server: an admin may delete any request, anyone else only
    // their own that is still waiting. The server decides; this only hides the button.
    canDelete(r) {
      if (!this.me) return false;
      return this.me.role === 'admin' || (r.state === 'pending' && r.requester.sub === this.me.sub);
    },

    // remove asks twice, since a record cannot be brought back: the first press
    // turns the button into the question. Only the record goes; Lidarr is untouched.
    async remove(r) {
      if (this.busy[r.id]) return;
      if (this.deleting !== r.id) {
        this.deleting = r.id;
        return;
      }
      this.busy[r.id] = true;
      this.rowError[r.id] = '';
      this.notice = '';
      try {
        const res = await api('/api/requests/' + r.id, { method: 'DELETE', headers: { 'X-Albumreq': '1' } });
        if (!res.ok && res.status !== 404) throw new Error((await res.text()).trim() || res.statusText);
        this.deleting = null;
        await this.loadRequests();
      } catch (e) {
        this.rowError[r.id] = e.message;
      } finally {
        this.busy[r.id] = false;
      }
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
          ? 'Approved ' + r.title + ', but it could not be started.'
          : 'Approved ' + r.title + '.';
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
      if (this.page === 'requests') await this.loadRequests();
      else this.refreshPendingCount();
    },

    // An approved request follows Lidarr: searching, downloading 42%, importing,
    // available. Without an answer from Lidarr it is just "Approved".
    reqStateLabel(r) {
      const p = r.state === 'approved' ? this.prog[r.albumId] : null;
      if (p) {
        if (p.status === 'available') return 'Available';
        const stage = { downloading: 'Downloading', paused: 'Paused', queued: 'Queued', importing: 'Importing', stalled: 'Needs attention' }[p.stage];
        if (stage) return stage;
        if (p.status === 'requested') return 'Searching';
        if (p.status === 'notfound') return 'Not found yet';
        if (p.status === 'processing') return 'Downloading';
      }
      return { pending: 'Waiting for approval', approved: 'Approved', declined: 'Declined' }[r.state] || r.state;
    },

    reqStateClass(r) {
      const p = r.state === 'approved' ? this.prog[r.albumId] : null;
      if (p) {
        if (p.stage === 'stalled') return 'notfound';
        if (['available', 'processing', 'requested', 'notfound'].includes(p.status)) return p.status;
      }
      return r.state;
    },

    // --- issues ---

    // loadIssues asks the server what is wrong. fresh runs the checks again now
    // (they talk to Lidarr), otherwise a recent answer will do. quiet only updates
    // the count in the nav, for the page load and after a save.
    async loadIssues(fresh, quiet) {
      if (this.issuesChecking) return;
      if (!quiet) {
        this.issuesChecking = true;
        this.issuesError = '';
      }
      try {
        const res = await api('/api/issues' + (fresh ? '?fresh=1' : ''), { cache: 'no-store' });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const data = await res.json();
        this.issueCount = data.count;
        if (!quiet) {
          this.issues = data.issues;
          this.issuesLoaded = true;
        }
      } catch (e) {
        if (!quiet) this.issuesError = e.message;
      } finally {
        if (!quiet) this.issuesChecking = false;
      }
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


    // shownStatus: "In Lidarr" is not shown. It means Lidarr knows the album but is not
    // looking for it, which to a person is the same as not having asked: the Request
    // button says what they can do, and naming Lidarr only confuses.
    shownStatus(s) {
      return !!s && s !== 'inlidarr';
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
