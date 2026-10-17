package web

import "net/http"

// matrix/hardened: the explicit import response exposes aggregate counts only;
// transcript paths and per-session importer errors never cross the HTTP boundary.
type matrixCostSyncResponse struct {
	SessionsScanned int `json:"sessions_scanned"`
	EventsImported  int `json:"events_imported"`
	EventsSkipped   int `json:"events_skipped"`
	ErrorCount      int `json:"error_count"`
}

func init() {
	// matrix/hardened: register the manual mutation separately from read-only
	// cost queries; the standard Host and CSRF middleware protect this route.
	registerFeatureRoutes(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("/api/costs/sync", s.handleMatrixCostSync)
	})
}

func (s *Server) handleMatrixCostSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, ErrCodeMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorizeRequest(r) {
		writeAPIError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "unauthorized")
		return
	}
	if s.cfg.ReadOnly {
		writeAPIError(w, http.StatusForbidden, ErrCodeReadOnly, "cost synchronization is disabled in read-only mode")
		return
	}
	if !s.checkMutationsAllowed(w) {
		return
	}
	s.costSyncMu.RLock()
	syncCosts := s.costSync
	s.costSyncMu.RUnlock()
	if syncCosts == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "cost synchronization is not available")
		return
	}
	// matrix/hardened: duplicates do not enqueue transcript work or consume the
	// mutation burst. Keep this ownership until the synchronous callback returns,
	// including cancellation when an importer cannot stop mid-file.
	if !s.costSyncRunning.CompareAndSwap(false, true) {
		writeAPIError(w, http.StatusConflict, "SYNC_IN_PROGRESS", "cost synchronization is already in progress")
		return
	}
	defer s.costSyncRunning.Store(false)
	if !s.checkMutationRateLimit(w) {
		return
	}
	result, err := syncCosts(r.Context())
	if err != nil || result == nil {
		writeAPIError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to synchronize costs")
		return
	}
	writeJSON(w, http.StatusOK, matrixCostSyncResponse{
		SessionsScanned: result.SessionsScanned,
		EventsImported:  result.EventsImported,
		EventsSkipped:   result.EventsSkipped,
		ErrorCount:      len(result.Errors),
	})
}
