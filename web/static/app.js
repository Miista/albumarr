const STATUS_LABELS = {
  available: 'Available',
  processing: 'Processing',
  requested: 'Requested',
  notfound: 'Not found',
};

const DEBOUNCE_MS = 300;
const MIN_QUERY = 2;

function App() {
  // Kept out of the reactive scope: timers and the in-flight counter have no
  // reason to trigger a re-render.
  let timer = null;
  let seq = 0;

  return {
    query: '',
    shownQuery: '',
    albums: [],
    loading: false,
    searched: false,
    error: '',

    mounted() {
      const q = new URLSearchParams(location.search).get('q');
      if (q) {
        this.query = q;
        this.search();
      }
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
        history.replaceState(null, '', location.pathname);
        return;
      }

      this.loading = true;
      this.error = '';
      history.replaceState(null, '', '?q=' + encodeURIComponent(q));
      try {
        const res = await fetch('/api/search?q=' + encodeURIComponent(q));
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        const data = await res.json();
        if (mine !== seq) return; // a newer search has superseded this one
        this.albums = data.albums;
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
  };
}

PetiteVue.createApp({ App }).mount('#app');
