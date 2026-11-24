package cache

import (
	"context"

	"vedetta/internal/search"
)

// cachedSearch wraps a search provider. It sits outside the composite chain,
// so fallback and academic routing are cached too.
type cachedSearch struct {
	inner search.Provider
	s     *Session
}

// WrapSearch returns p with the cache in front of it.
func (s *Session) WrapSearch(p search.Provider) search.Provider {
	if p == nil {
		return nil
	}
	return &cachedSearch{inner: p, s: s}
}

func (w *cachedSearch) Name() string { return w.inner.Name() }

func (w *cachedSearch) Search(ctx context.Context, query string, opts search.Options) ([]search.Result, error) {
	c := w.s.c
	key := SearchKey(w.inner.Name(), query, opts)
	if e, ok := c.store.Get(KindSearch, key); ok && c.fresh(e) {
		w.s.searchHits.Add(1)
		return e.Results, nil
	}
	w.s.searchMisses.Add(1)
	res, err := w.inner.Search(ctx, query, opts)
	if err != nil {
		// Network errors, quota and failed chains are not facts about the query.
		return res, err
	}
	ttl := c.searchTTL
	if len(res) == 0 {
		ttl = shortTTL
	}
	c.put(Entry{Key: key, Kind: KindSearch, Results: res}, ttl)
	return res, nil
}

// FallbackStatus forwards to the wrapped provider. On a cache hit the inner
// provider is not called, so no fallback is reported for that search.
func (w *cachedSearch) FallbackStatus(sessionID ...string) (bool, string) {
	if fb, ok := w.inner.(search.FallbackReporter); ok {
		return fb.FallbackStatus(sessionID...)
	}
	return false, ""
}

// ResetFallback forwards to the wrapped provider.
func (w *cachedSearch) ResetFallback(sessionID ...string) {
	if fb, ok := w.inner.(search.FallbackResetter); ok {
		fb.ResetFallback(sessionID...)
	}
}
