const STATUS_LABELS = {
  available: 'Available',
  processing: 'Processing',
  requested: 'Requested',
  notfound: 'Not found',
};

// MusicBrainz answers about one search a second, so wait for a pause in the
// typing rather than searching on every few keystrokes.
const DEBOUNCE_MS = 600;
const MIN_QUERY = 2;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

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
    loading: false,
    searched: false,
    error: '',

    selected: null, // the card (or just { id }) behind the open sheet
    detail: null,
    detailLoading: false,
    detailError: '',

    mounted() {
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

      if (q.length < MIN_QUERY) {
        this.albums = [];
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
        const res = await fetch('/api/search?q=' + encodeURIComponent(q));
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const data = await res.json();
        if (mine !== seq) return; // a newer search has superseded this one
        this.albums = data.albums;
        this.libraryOk = data.libraryOk;
        this.shownQuery = data.query;
        this.searched = true;
      } catch (e) {
        if (mine !== seq) return;
        this.albums = [];
        this.searched = false;
        this.error = e.message;
      } finally {
        if (mine === seq) this.loading = false;
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
      this.detailLoading = true;
      this.selected = card;
      document.body.style.overflow = 'hidden';
      this.loadDetail(card.id);
    },

    async loadDetail(id) {
      const mine = ++detailSeq;
      try {
        const res = await fetch('/api/album/' + id);
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const d = await res.json();
        if (mine === detailSeq) this.detail = d;
      } catch (e) {
        if (mine === detailSeq) this.detailError = e.message;
      } finally {
        if (mine === detailSeq) this.detailLoading = false;
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
