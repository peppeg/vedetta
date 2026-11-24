package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"vedetta/internal/cache"
)

// CacheResponse is what Settings shows about the memory of searches and pages
// (map R2): its mode, how much it holds and how long it keeps things. Removed and
// Freed say what a clear deleted.
type CacheResponse struct {
	Mode           string `json:"mode"` // off, record or replay
	Files          int    `json:"files"`
	Bytes          int64  `json:"bytes"`
	SearchTTLHours int    `json:"search_ttl_hours,omitempty"`
	PageTTLHours   int    `json:"page_ttl_hours,omitempty"`
	ExpiredRemoved bool   `json:"expired_removed"` // what expires is deleted by itself
	Removed        int    `json:"removed,omitempty"`
	FreedBytes     int64  `json:"freed_bytes,omitempty"`
}

func (h *Handlers) cacheState() CacheResponse {
	c := h.engine.Cache()
	if c == nil {
		return CacheResponse{Mode: "off"}
	}
	u := c.Usage()
	h.cfgMu.RLock()
	prune := h.cfg.CachePrune
	h.cfgMu.RUnlock()
	return CacheResponse{
		Mode: c.Mode(), Files: u.Files, Bytes: u.Bytes,
		SearchTTLHours: int(c.SearchTTL().Hours()), PageTTLHours: int(c.PageTTL().Hours()),
		ExpiredRemoved: c.Mode() == cache.ModeRecord && prune,
	}
}

// Cache answers GET /api/cache with the state of the memory, behind the settings
// code like the rest of Settings.
func (h *Handlers) Cache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorizeConfig(w, r) {
		return
	}
	writeCacheJSON(w, h.cacheState())
}

// CacheClear empties the memory by hand (POST /api/cache/clear). Only Vedetta's
// own page can do it, with the settings code when there is one; a benchmark
// corpus (replay) is never emptied.
func (h *Handlers) CacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorizeConfig(w, r) {
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, notOwnPage)
		return
	}
	c := h.engine.Cache()
	if c == nil {
		writeJSONError(w, http.StatusConflict, "the memory of searches and pages is off")
		return
	}
	removed, freed, err := c.Clear()
	if errors.Is(err, cache.ErrReplay) {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	st := h.cacheState()
	st.Removed, st.FreedBytes = removed, freed
	writeCacheJSON(w, st)
}

func writeCacheJSON(w http.ResponseWriter, v CacheResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
