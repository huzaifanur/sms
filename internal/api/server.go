// Package api is the HTTP interface: send one SMS, or a bulk job with one
// body for every number or a body per number. Everything is queued and sent
// by the Worker at the engine's pace; clients poll the job for progress.
// It also serves the sender page (/), the OpenAPI spec (/openapi.json) and
// Swagger UI (/docs) from web/.
package api

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"sms/internal/gateway"
	"sms/internal/msg"
	"sms/internal/store"
)

//go:embed web
var web embed.FS

// MaxBulk is the most messages one request may queue.
const MaxBulk = 1000

// Server serves the API.
type Server struct {
	W              *Worker
	User, Password string // HTTP Basic auth for everything but /healthz
}

// Handler returns the routes. Everything but /healthz needs the username and
// password, so the browser asks once for the pages and their API calls.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST /api/send", s.send)
	api.HandleFunc("POST /api/bulk", s.bulk)
	api.HandleFunc("GET /api/jobs", s.listJobs)
	api.HandleFunc("GET /api/jobs/{id}", s.getJob)
	api.HandleFunc("POST /api/jobs/{id}/cancel", s.cancelJob)
	api.HandleFunc("POST /api/jobs/{id}/resume", s.resumeJob)
	api.HandleFunc("GET /api/status", s.status)

	api.Handle("GET /{$}", page("web/index.html", "text/html; charset=utf-8"))
	api.Handle("GET /docs", page("web/docs.html", "text/html; charset=utf-8"))
	api.Handle("GET /openapi.json", page("web/openapi.json", "application/json"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("/", s.auth(api))
	return mux
}

func page(name, contentType string) http.Handler {
	data, err := web.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write(data)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		// Compare both even when the first differs, so timing reveals neither.
		u := subtle.ConstantTimeCompare([]byte(user), []byte(s.User))
		p := subtle.ConstantTimeCompare([]byte(pass), []byte(s.Password))
		if u&p != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="SMS", charset="UTF-8"`)
			writeError(w, http.StatusUnauthorized, "wrong or missing username and password")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// item is one recipient and text, as sent by the client.
type item struct {
	To   string `json:"to"`
	Body string `json:"body"`
}

type bulkRequest struct {
	To          []string `json:"to"`       // with Body: one text for every number
	Body        string   `json:"body"`     //
	Messages    []item   `json:"messages"` // or: a text per number
	SkipInvalid bool     `json:"skip_invalid"`
}

// problem is a rejected item. Index is its 1-based position in the request.
type problem struct {
	Index int    `json:"index"`
	To    string `json:"to"`
	Error string `json:"error"`
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req item
	if !decode(w, r, &req) {
		return
	}
	s.create(w, r, store.KindSend, []item{req}, false)
}

func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	var req bulkRequest
	if !decode(w, r, &req) {
		return
	}
	var items []item
	switch {
	case len(req.Messages) > 0 && (len(req.To) > 0 || req.Body != ""):
		writeError(w, http.StatusBadRequest, `send either "to" with "body", or "messages", not both`)
		return
	case len(req.Messages) > 0:
		items = req.Messages
	case len(req.To) > 0:
		for _, to := range req.To {
			items = append(items, item{to, req.Body})
		}
	default:
		writeError(w, http.StatusBadRequest, `send "to" (a list of numbers) with "body", or "messages" (a list of {"to", "body"})`)
		return
	}
	if len(items) > MaxBulk {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%d messages; at most %d per request", len(items), MaxBulk))
		return
	}
	s.create(w, r, store.KindBulk, items, req.SkipInvalid)
}

// create validates items and queues them as one job. Invalid items reject
// the whole request unless skipInvalid; repeats of the same number and text
// are always skipped.
func (s *Server) create(w http.ResponseWriter, r *http.Request, kind string, items []item, skipInvalid bool) {
	e := s.W.E
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		if prev, err := e.Store.FindJobByKey(key); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		} else if prev != nil {
			s.writeJob(w, http.StatusOK, prev, true, nil)
			return
		}
	}

	var msgs []store.Message
	var invalid, skipped []problem
	seen := map[string]int{}
	for i, it := range items {
		bad := func(format string, a ...any) {
			invalid = append(invalid, problem{i + 1, it.To, fmt.Sprintf(format, a...)})
		}
		phone, err := msg.Normalize(it.To, e.S.CountryCode)
		if err != nil {
			bad("%v", err)
			continue
		}
		if strings.TrimSpace(it.Body) == "" {
			bad("empty body")
			continue
		}
		enc := msg.Analyze(it.Body)
		if enc.Segments > e.S.MaxSegments {
			bad("too long: needs %d SMS (%s); the limit is %d", enc.Segments, enc.Describe(), e.S.MaxSegments)
			continue
		}
		if first, dup := seen[phone+"\x00"+it.Body]; dup {
			skipped = append(skipped, problem{i + 1, it.To, fmt.Sprintf("same number and text as message %d", first)})
			continue
		}
		seen[phone+"\x00"+it.Body] = i + 1
		msgs = append(msgs, store.Message{RowNo: i + 1, Phone: phone, Text: it.Body, Segments: enc.Segments, Encoding: enc.Name, State: store.MsgQueued})
	}
	if len(invalid) > 0 && (!skipInvalid || len(msgs) == 0) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":   fmt.Sprintf("%d invalid message(s); nothing was queued", len(invalid)),
			"invalid": invalid,
		})
		return
	}

	job := &store.Job{Kind: kind, IdemKey: key, SIM: e.S.SIM}
	if err := e.Store.CreateJob(job, msgs, e.Clock.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.W.Wake()
	s.writeJob(w, http.StatusAccepted, job, true, append(invalid, skipped...))
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.W.E.Store.ListJobs(50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		v, err := s.view(j, false)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.lookup(w, r)
	if ok {
		s.writeJob(w, http.StatusOK, job, true, nil)
	}
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, s.W.Cancel)
}

func (s *Server) resumeJob(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, s.W.Resume)
}

func (s *Server) act(w http.ResponseWriter, r *http.Request, do func(id string) error) {
	err := do(r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNoJob):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrFinished):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if job, ok := s.lookup(w, r); ok {
		s.writeJob(w, http.StatusOK, job, false, nil)
	}
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*store.Job, bool) {
	job, err := s.W.E.Store.GetJob(r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNoJob):
		writeError(w, http.StatusNotFound, err.Error())
		return nil, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return job, true
}

// status reports the caps, the sending window and whether SMSGate accepts
// our credentials.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	e := s.W.E
	q, err := e.Quota()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	gw := "ok"
	ctx, cancel := context.WithTimeout(r.Context(), e.S.GatewayTimeout)
	defer cancel()
	if err := e.GW.Health(ctx); err != nil {
		gw = err.Error()
	} else if _, err := e.GW.Status(ctx, "status-probe"); err != nil && !errors.Is(err, gateway.ErrNotFound) {
		gw = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"gateway":       gw,
		"sim":           e.S.SIM,
		"hours":         e.S.HoursLabel(),
		"in_hours":      q.InHours,
		"last_15m":      fmt.Sprintf("%d/%d", q.Window, q.WindowLimit),
		"today":         fmt.Sprintf("%d/%d", q.Day, q.DayLimit),
		"max_segments":  e.S.MaxSegments,
		"running_job":   s.W.Running(),
		"next_slot_utc": timeOrNil(q.NextSlot),
	})
}

type jobView struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	State     string         `json:"state"`
	Reason    string         `json:"reason,omitempty"`
	SIM       int            `json:"sim"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Counts    map[string]int `json:"counts"`
	Messages  []messageView  `json:"messages,omitempty"`
	Skipped   []problem      `json:"skipped,omitempty"`
}

type messageView struct {
	Index     int       `json:"index"`
	To        string    `json:"to"`
	State     string    `json:"state"`
	Segments  int       `json:"segments"`
	Error     string    `json:"error,omitempty"`
	Submitted time.Time `json:"submitted_at,omitzero"`
}

func (s *Server) view(j *store.Job, withMessages bool) (jobView, error) {
	st := s.W.E.Store
	c, err := st.Counts(j.ID)
	if err != nil {
		return jobView{}, err
	}
	v := jobView{ID: j.ID, Kind: j.Kind, State: j.State, Reason: j.StopReason, SIM: j.SIM, CreatedAt: j.CreatedAt.UTC(), UpdatedAt: j.UpdatedAt.UTC(), Counts: c}
	if !withMessages {
		return v, nil
	}
	msgs, err := st.Messages(j.ID)
	if err != nil {
		return jobView{}, err
	}
	for _, m := range msgs {
		v.Messages = append(v.Messages, messageView{Index: m.RowNo, To: m.Phone, State: m.State, Segments: m.Segments, Error: m.Error, Submitted: m.SubmittedAt.UTC()})
	}
	return v, nil
}

func (s *Server) writeJob(w http.ResponseWriter, code int, j *store.Job, withMessages bool, skipped []problem) {
	v, err := s.view(j, withMessages)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	v.Skipped = skipped
	writeJSON(w, code, v)
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
