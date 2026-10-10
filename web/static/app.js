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

    statusLabel(s) {
      return STATUS_LABELS[s] || s;
    },
  };
}

PetiteVue.createApp({ App }).mount('#app');
