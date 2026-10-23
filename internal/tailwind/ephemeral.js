// ephemeral-selected:ring-2 -> &[ephemeral~="selected"]. ClayJS keeps per-tab
// state (a selection, a drag) in the ephemeral attribute and never saves it.
// matchVariant only accepts bare names that `values` lists, so `values` answers
// for any simple word; the bracketed form ephemeral-[selected]: works regardless.
const anyWord = new Proxy({}, {
  get: (_, key) => (typeof key === 'string' && /^[a-z0-9_-]+$/i.test(key) ? key : undefined),
});

module.exports = function ephemeralVariant({ matchVariant }) {
  matchVariant('ephemeral', (value) => `&[ephemeral~="${value}"]`, { values: anyWord });
};
