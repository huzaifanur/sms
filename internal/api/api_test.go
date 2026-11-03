package api

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sms/internal/config"
	"sms/internal/engine"
	"sms/internal/gateway"
	"sms/internal/store"
)

const (
	user     = "sms"
	password = "test-password-123"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.t = c.t.Add(d)
	return nil
}

type harness struct {
	t   *testing.T
	srv *httptest.Server
	w   *Worker
	gw  *gateway.Fake
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := config.Load(func(k string) string {
		return map[string]string{"SMS_SIM": "1", "SMS_GATEWAY_URL": "http://phone", "SMS_WARMUP_COUNT": "5"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gw := &gateway.Fake{}
	clock := &fakeClock{t: time.Date(2026, 10, 5, 11, 0, 0, 0, s.TZ)}
	w := NewWorker(&engine.Engine{Store: st, GW: gw, S: s, Clock: clock, Rand: rand.New(rand.NewPCG(1, 2))})
	srv := httptest.NewServer((&Server{W: w, User: user, Password: password}).Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, w: w, gw: gw}
}

// do sends a request and decodes the JSON response into a map.
func (h *harness) do(method, path, body string, hdr ...string) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.SetBasicAuth(user, password)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (h *harness) runAll() {
	h.t.Helper()
	for {
		ran, err := h.w.Step(context.Background())
		if err != nil {
			h.t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t)
	get := func(path, user, pass string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for _, path := range []string{"/", "/docs", "/openapi.json", "/api/jobs", "/nope"} {
		for _, login := range [][2]string{{"", ""}, {user, "wrong"}, {"wrong", password}} {
			resp := get(path, login[0], login[1])
			if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
				t.Errorf("%s as %q/%q: %d, want 401 with a Basic challenge", path, login[0], login[1], resp.StatusCode)
			}
		}
	}
	for path, ctype := range map[string]string{"/": "text/html", "/docs": "text/html", "/openapi.json": "application/json", "/api/jobs": "application/json"} {
		resp := get(path, user, password)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s, want 200 %s", path, resp.StatusCode, resp.Header.Get("Content-Type"), ctype)
		}
	}
	if resp := get("/healthz", "", ""); resp.StatusCode != 200 {
		t.Errorf("/healthz needs no login, got %d", resp.StatusCode)
	}
	if resp := get("/nope", user, password); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/nope: %d", resp.StatusCode)
	}
}

// TestOpenAPIMatchesRoutes keeps the spec honest: every path it documents
// exists, and every API route is documented.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	data, err := web.ReadFile("web/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	want := map[string]bool{
		"POST /api/send": true, "POST /api/bulk": true, "GET /api/jobs": true, "GET /api/jobs/{id}": true,
		"POST /api/jobs/{id}/cancel": true, "POST /api/jobs/{id}/resume": true, "GET /api/status": true, "GET /healthz": true,
	}
	for path, ops := range spec.Paths {
		for method := range ops {
			route := strings.ToUpper(method) + " " + path
			if !want[route] {
				t.Errorf("spec documents %s, which the server doesn't serve", route)
			}
			delete(want, route)
		}
	}
	for route := range want {
		t.Errorf("%s is not in openapi.json", route)
	}
}

func TestSendOne(t *testing.T) {
	h := newHarness(t)
	code, job := h.do("POST", "/api/send", `{"to":"0300 1234567","body":"Salam"}`)
	if code != http.StatusAccepted || job["state"] != store.JobQueued || job["kind"] != store.KindSend {
		t.Fatalf("%d %v", code, job)
	}
	h.runAll()
	code, job = h.do("GET", "/api/jobs/"+job["id"].(string), "")
	if code != 200 || job["state"] != store.JobDone {
		t.Fatalf("%d %v", code, job)
	}
	m := job["messages"].([]any)[0].(map[string]any)
	if m["to"] != "+923001234567" || (m["state"] != store.MsgSent && m["state"] != store.MsgDelivered) {
		t.Fatalf("message %v", m)
	}
	if len(h.gw.Accepted) != 1 || h.gw.Accepted[0].Text != "Salam" || h.gw.Accepted[0].SIM != 1 {
		t.Fatalf("gateway got %+v", h.gw.Accepted)
	}
}

func TestBulkSameBody(t *testing.T) {
	h := newHarness(t)
	code, job := h.do("POST", "/api/bulk", `{"to":["03001234567","03001234568","+923001234567","03001234569"],"body":"Meeting at 5"}`)
	if code != http.StatusAccepted {
		t.Fatalf("%d %v", code, job)
	}
	if n := job["counts"].(map[string]any)["queued"]; n != 3.0 {
		t.Fatalf("queued %v, want 3 (one repeat skipped)", n)
	}
	if sk := job["skipped"].([]any); len(sk) != 1 || sk[0].(map[string]any)["index"] != 3.0 {
		t.Fatalf("skipped %v", sk)
	}
	h.runAll()
	if len(h.gw.Accepted) != 3 {
		t.Fatalf("sent %d, want 3", len(h.gw.Accepted))
	}
	for _, m := range h.gw.Accepted {
		if m.Text != "Meeting at 5" {
			t.Fatalf("text %q", m.Text)
		}
	}
}

func TestBulkPerNumber(t *testing.T) {
	h := newHarness(t)
	code, job := h.do("POST", "/api/bulk", `{"messages":[{"to":"03001234567","body":"Salam Ali"},{"to":"03001234568","body":"Salam Sara"}]}`)
	if code != http.StatusAccepted {
		t.Fatalf("%d %v", code, job)
	}
	h.runAll()
	got := map[string]string{}
	for _, m := range h.gw.Accepted {
		got[m.Phone] = m.Text
	}
	if got["+923001234567"] != "Salam Ali" || got["+923001234568"] != "Salam Sara" {
		t.Fatalf("sent %v", got)
	}
}

func TestBulkInvalid(t *testing.T) {
	h := newHarness(t)
	body := `{"messages":[{"to":"03001234567","body":"ok"},{"to":"04235761234","body":"landline"},{"to":"03001234568","body":" "},{"to":"03001234569","body":"` + strings.Repeat("a", 161) + `"}]}`
	code, out := h.do("POST", "/api/bulk", body)
	if code != http.StatusUnprocessableEntity || len(out["invalid"].([]any)) != 3 {
		t.Fatalf("%d %v", code, out)
	}
	if code, jobs := h.do("GET", "/api/jobs", ""); code != 200 || len(jobs["jobs"].([]any)) != 0 {
		t.Fatalf("a rejected request queued a job: %v", jobs)
	}
	code, job := h.do("POST", "/api/bulk", strings.Replace(body, `{"messages"`, `{"skip_invalid":true,"messages"`, 1))
	if code != http.StatusAccepted || job["counts"].(map[string]any)["queued"] != 1.0 || len(job["skipped"].([]any)) != 3 {
		t.Fatalf("skip_invalid: %d %v", code, job)
	}
	for _, bad := range []string{
		`{"to":["03001234567"],"body":"x","messages":[{"to":"03001234567","body":"x"}]}`,
		`{}`,
		`{"to":"03001234567","body":"x"}`,
		`{"to":["03001234567"],"text":"x"}`,
	} {
		if code, out := h.do("POST", "/api/bulk", bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
}

func TestIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	body := `{"to":["03001234567","03001234568"],"body":"once"}`
	code, a := h.do("POST", "/api/bulk", body, "Idempotency-Key", "k1")
	if code != http.StatusAccepted {
		t.Fatalf("%d %v", code, a)
	}
	code, b := h.do("POST", "/api/bulk", body, "Idempotency-Key", "k1")
	if code != http.StatusOK || b["id"] != a["id"] {
		t.Fatalf("retry made a new job: %d %v", code, b)
	}
	h.runAll()
	if len(h.gw.Accepted) != 2 {
		t.Fatalf("sent %d, want 2", len(h.gw.Accepted))
	}
}

func TestCancelAndResume(t *testing.T) {
	h := newHarness(t)
	_, job := h.do("POST", "/api/bulk", `{"to":["03001234567","03001234568"],"body":"x"}`)
	id := job["id"].(string)
	if code, out := h.do("POST", "/api/jobs/"+id+"/cancel", ""); code != 200 || out["state"] != store.JobCancelled {
		t.Fatalf("cancel: %d %v", code, out)
	}
	h.runAll()
	if len(h.gw.Accepted) != 0 {
		t.Fatalf("cancelled job sent %d", len(h.gw.Accepted))
	}
	if code, _ := h.do("POST", "/api/jobs/"+id+"/resume", ""); code != http.StatusConflict {
		t.Fatalf("resume of a cancelled job: %d", code)
	}
	if code, _ := h.do("POST", "/api/jobs/nope/cancel", ""); code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", code)
	}

	// A stopped job resumes on request.
	h.gw.SendErrs = []error{gateway.ErrUnauthorized}
	_, job = h.do("POST", "/api/send", `{"to":"03001234567","body":"retry me"}`)
	id = job["id"].(string)
	h.runAll()
	if _, out := h.do("GET", "/api/jobs/"+id, ""); out["state"] != store.JobStopped {
		t.Fatalf("want stopped, got %v", out)
	}
	if code, out := h.do("POST", "/api/jobs/"+id+"/resume", ""); code != 200 || out["state"] != store.JobQueued {
		t.Fatalf("resume: %d %v", code, out)
	}
	h.runAll()
	if _, out := h.do("GET", "/api/jobs/"+id, ""); out["state"] != store.JobDone || len(h.gw.Accepted) != 1 {
		t.Fatalf("after resume: %v, sent %d", out, len(h.gw.Accepted))
	}
}

func TestCancelWhileRunning(t *testing.T) {
	h := newHarness(t)
	_, job := h.do("POST", "/api/bulk", `{"to":["03001234567","03001234568","03001234569","03001234560"],"body":"x"}`)
	id := job["id"].(string)
	h.gw.Outcome = func(m gateway.OutMessage) []gateway.Status {
		if len(h.gw.Accepted) == 1 {
			h.w.Cancel(id)
		}
		return []gateway.Status{{State: gateway.Sent}}
	}
	h.runAll()
	_, out := h.do("GET", "/api/jobs/"+id, "")
	if out["state"] != store.JobCancelled || len(h.gw.Accepted) >= 4 {
		t.Fatalf("state %v after %d sends", out["state"], len(h.gw.Accepted))
	}
}

func TestStatus(t *testing.T) {
	h := newHarness(t)
	code, out := h.do("GET", "/api/status", "")
	if code != 200 || out["gateway"] != "ok" || out["today"] != "0/450" || out["sim"] != 1.0 {
		t.Fatalf("%d %v", code, out)
	}
}
