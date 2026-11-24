package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"

	"vedetta/internal/logquiet"
)

// DataResponse is what Settings shows in "I tuoi dati" (map R7): the searches kept
// and their size on disk, where the data folder is, and whether the log is quiet.
type DataResponse struct {
	Sessions int    `json:"sessions"`
	DBBytes  int64  `json:"db_bytes"`
	DataDir  string `json:"data_dir"`
	QuietLog bool   `json:"quiet_log"`
	Removed  int    `json:"removed,omitempty"`
}

func (h *Handlers) dataState() DataResponse {
	st := DataResponse{DataDir: "data", QuietLog: logquiet.On()}
	if abs, err := filepath.Abs("data"); err == nil {
		st.DataDir = abs
	}
	if s := h.engine.Storage(); s != nil {
		st.Sessions, st.DBBytes = s.Usage()
	}
	return st
}

// Data answers GET /api/data, behind the settings code like the rest of Settings.
func (h *Handlers) Data(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorizeConfig(w, r) {
		return
	}
	writeDataJSON(w, h.dataState())
}

// DataClearSearches answers POST /api/data/clear-searches: every search stops and
// every session is deleted, overwritten on disk. Behind the settings code; the
// guard already refuses other sites.
func (h *Handlers) DataClearSearches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorizeConfig(w, r) {
		return
	}
	s := h.engine.Storage()
	if s == nil {
		writeJSONError(w, http.StatusConflict, "no database")
		return
	}
	h.runs.cancelAll(10 * time.Second)
	n, err := s.DeleteAllSessions()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "deleting the searches failed: "+err.Error())
		return
	}
	st := h.dataState()
	st.Removed = n
	writeDataJSON(w, st)
}

func writeDataJSON(w http.ResponseWriter, v DataResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
