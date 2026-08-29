package service

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/benemon/shackleton/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// sessionCookieName carries the console's token between reloads. HttpOnly and
// SameSite=Strict, scoped to the session path alone — script cannot read it,
// and no other route ever receives it. The token in memory remains the
// credential the console actually uses; this cookie only lets a reload get it
// back (the Dufflebag ADR-0021 pattern, minus renewal: the token is static).
const sessionCookieName = "shackleton_session"

const sessionPath = "/v1/session"

func NewHTTP(service *Service, token string) http.Handler {
	mux := http.NewServeMux()
	// POST verifies the presented bearer through the same middleware as every
	// mutating route and mints it into the cookie. CSRF holds because a
	// cross-site form cannot set an Authorization header.
	mux.HandleFunc("POST "+sessionPath, createSession)
	mux.HandleFunc("POST /v1/investigations", service.createInvestigation)
	mux.HandleFunc("GET /v1/investigations", service.listInvestigations)
	mux.HandleFunc("GET /v1/investigations/{id}", service.getInvestigation)
	mux.HandleFunc("GET /v1/investigations/{id}/events", service.followInvestigation)
	mux.HandleFunc("POST /v1/investigations/{id}/kb", service.saveInvestigationToKB)
	mux.HandleFunc("POST /v1/alerts", service.ingestAlerts)
	mux.HandleFunc("GET /v1/approvals", service.listApprovals)
	mux.HandleFunc("GET /v1/approvals/events", service.followApprovals)
	mux.HandleFunc("POST /v1/approvals/{id}/decision", service.decideApproval)
	mux.HandleFunc("GET /v1/audit", service.getAudit)
	mux.HandleFunc("GET /v1/inventory", service.getInventory)
	mux.HandleFunc("GET /v1/kb", service.listKB)
	mux.HandleFunc("GET /v1/kb/{slug}", service.getKB)
	mux.HandleFunc("GET /v1/config", service.getConfig)
	mux.HandleFunc("GET /v1/health", service.getHealth)
	mux.Handle("GET /metrics", promhttp.Handler())
	// GET and DELETE are the only routes exempt from the bearer middleware:
	// their credential arrives as the cookie (a booting console has no token
	// yet), and ending a session must never require a live credential.
	root := http.NewServeMux()
	root.HandleFunc("GET "+sessionPath, readSession(token))
	root.HandleFunc("DELETE "+sessionPath, deleteSession)
	root.Handle("/", bearerAuth(mux, token))
	return root
}

func sessionCookie(token string, r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     sessionPath,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	}
}

func clearedSessionCookie(r *http.Request) *http.Cookie {
	cookie := sessionCookie("", r)
	cookie.MaxAge = -1
	return cookie
}

func createSession(w http.ResponseWriter, r *http.Request) {
	provided, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	http.SetCookie(w, sessionCookie(provided, r))
	w.WriteHeader(http.StatusNoContent)
}

// readSession exchanges the cookie for the token it holds. An invalid cookie
// is cleared and answers 204, not 401 — the console falls back to its gate.
func readSession(token string) http.HandlerFunc {
	want := []byte(token)
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if subtle.ConstantTimeCompare([]byte(cookie.Value), want) != 1 {
			http.SetCookie(w, clearedSessionCookie(r))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Token string `json:"token"`
		}{cookie.Value})
	}
}

func deleteSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, clearedSessionCookie(r))
	w.WriteHeader(http.StatusNoContent)
}

func bearerAuth(next http.Handler, token string) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(provided), want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) createInvestigation(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Question   string `json:"question"`
		Trigger    string `json:"trigger"`
		FollowUpTo string `json:"follow_up_to"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(request.Question) == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}
	if request.Trigger == "" {
		request.Trigger = "api"
	}
	var summary store.Summary
	var err error
	if request.FollowUpTo != "" {
		summary, err = s.CreateFollowUp(r.Context(), request.Question, request.Trigger, request.FollowUpTo)
	} else {
		summary, err = s.CreateInvestigation(r.Context(), request.Question, request.Trigger)
	}
	if errors.Is(err, ErrInvestigationNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, summary)
}

func (s *Service) listInvestigations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ListInvestigations())
}

func (s *Service) getInvestigation(w http.ResponseWriter, r *http.Request) {
	summary, events, err := s.GetInvestigation(r.PathValue("id"))
	if err != nil {
		writeInvestigationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Summary store.Summary `json:"summary"`
		Events  []store.Event `json:"events"`
	}{summary, events})
}

func (s *Service) saveInvestigationToKB(w http.ResponseWriter, r *http.Request) {
	slug, err := s.SaveInvestigationToKB(r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvestigationNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrNotCurateable), errors.Is(err, ErrArticleExists):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Slug string `json:"slug"`
	}{slug})
}

func (s *Service) followInvestigation(w http.ResponseWriter, r *http.Request) {
	snapshot, live, cancel, err := s.FollowInvestigation(r.PathValue("id"))
	if err != nil {
		writeInvestigationError(w, err)
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	index := 0
	for _, event := range snapshot {
		if writeSSE(w, controller, index, event) != nil {
			return
		}
		index++
		if terminal(event) {
			return
		}
	}
	if live == nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-live:
			if !ok || writeSSE(w, controller, index, event) != nil {
				return
			}
			index++
			if terminal(event) {
				return
			}
		}
	}
}

// ingestAlerts decodes leniently: the Alertmanager webhook payload is an
// external contract carrying fields this endpoint has no use for.
func (s *Service) ingestAlerts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Alerts []Alert `json:"alerts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, skipped, err := s.IngestAlerts(r.Context(), request.Alerts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		Created int `json:"created"`
		Skipped int `json:"skipped"`
	}{created, skipped})
}

func (s *Service) listApprovals(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ListPendingApprovals())
}

func (s *Service) followApprovals(w http.ResponseWriter, r *http.Request) {
	events, cancel := s.SubscribeApprovals()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-events:
			if !ok || writeApprovalSSE(w, controller, event) != nil {
				return
			}
		}
	}
}

func (s *Service) decideApproval(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Approved *bool `json:"approved"`
	}
	if err := decodeJSON(r, &request); err != nil || request.Approved == nil {
		if err == nil {
			err = errors.New("approved is required")
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.DecideApproval(r.PathValue("id"), *request.Approved, "api"); err != nil {
		switch {
		case errors.Is(err, ErrApprovalNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrApprovalAlreadyDecided):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Approved bool `json:"approved"`
	}{*request.Approved})
}

func (s *Service) getAudit(w http.ResponseWriter, _ *http.Request) {
	entries, err := s.AuditTrail()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Service) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ConfigView())
}

func (s *Service) getHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Health())
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("body must contain one JSON value")
		}
		return err
	}
	return nil
}

func writeSSE(w http.ResponseWriter, controller *http.ResponseController, index int, event store.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("id: " + strconv.Itoa(index) + "\nevent: " + event.Type + "\ndata: " + string(data) + "\n\n")); err != nil {
		return err
	}
	return controller.Flush()
}

func writeApprovalSSE(w http.ResponseWriter, controller *http.ResponseController, event ApprovalEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("event: " + event.Type + "\ndata: " + string(data) + "\n\n")); err != nil {
		return err
	}
	return controller.Flush()
}

func terminal(event store.Event) bool {
	return event.Type == store.EventCompleted || event.Type == store.EventFailed
}

func writeInvestigationError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrInvalidID) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "investigation not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}

func (s *Service) getInventory(w http.ResponseWriter, _ *http.Request) {
	view, err := s.InventoryView()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Service) listKB(w http.ResponseWriter, r *http.Request) {
	articles, err := s.KBList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, articles)
}

func (s *Service) getKB(w http.ResponseWriter, r *http.Request) {
	raw, err := s.KBGet(r.PathValue("slug"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "article not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
