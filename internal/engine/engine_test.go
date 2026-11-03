package engine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"sms/internal/config"
	"sms/internal/gateway"
	"sms/internal/store"
)

type fakeClock struct {
	t       time.Time
	onSleep func()
}

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.t = c.t.Add(d)
	if c.onSleep != nil {
		c.onSleep()
	}
	return ctx.Err()
}

func defaults(t *testing.T) config.Settings {
	t.Helper()
	s, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	s.SIM = 1
	return s
}

// monday10 is Monday 2026-10-05 10:00 in Asia/Karachi.
func monday10(s config.Settings) time.Time {
	return time.Date(2026, 10, 5, 10, 0, 0, 0, s.TZ)
}

type harness struct {
	e     *Engine
	st    *store.Store
	gw    *gateway.Fake
	clock *fakeClock
	job   *store.Job
}

func newHarness(t *testing.T, s config.Settings, rows int, start time.Time) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock := &fakeClock{t: start}
	if err := st.Recover(clock.t); err != nil {
		t.Fatal(err)
	}
	gw := &gateway.Fake{}
	h := &harness{
		e:     &Engine{Store: st, GW: gw, S: s, Clock: clock, Rand: rand.New(rand.NewPCG(1, 2))},
		st:    st,
		gw:    gw,
		clock: clock,
	}
	if rows > 0 {
		h.job = h.addJob(t, rows)
	}
	return h
}

func (h *harness) addJob(t *testing.T, rows int) *store.Job {
	t.Helper()
	job := &store.Job{Kind: store.KindBulk, SIM: 1}
	var msgs []store.Message
	for i := 0; i < rows; i++ {
		msgs = append(msgs, store.Message{RowNo: i + 2, Phone: fmt.Sprintf("+92300%07d", i), Text: "hi", Segments: 1, Encoding: "GSM-7", State: store.MsgQueued})
	}
	if err := h.st.CreateJob(job, msgs, h.clock.t); err != nil {
		t.Fatal(err)
	}
	return job
}

func (h *harness) run(t *testing.T, ctx context.Context) *Outcome {
	t.Helper()
	o, err := h.e.RunJob(ctx, h.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (h *harness) ledger(t *testing.T) []store.LedgerEntry {
	t.Helper()
	entries, err := h.st.LedgerSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func assertUniqueSends(t *testing.T, gw *gateway.Fake) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range gw.Accepted {
		if seen[m.ID] {
			t.Fatalf("message %s sent twice", m.ID)
		}
		seen[m.ID] = true
		if m.SIM != 1 {
			t.Fatalf("message %s on SIM %d", m.ID, m.SIM)
		}
	}
}

func TestHappyPathPacing(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 40, monday10(s))
	o := h.run(t, context.Background())
	if o.State != store.JobDone {
		t.Fatalf("state %s: %s", o.State, o.Reason)
	}
	if len(h.gw.Accepted) != 40 {
		t.Fatalf("accepted %d, want 40", len(h.gw.Accepted))
	}
	assertUniqueSends(t, h.gw)
	led := h.ledger(t)
	for i := 1; i < len(led); i++ {
		gap := led[i].SubmittedAt.Sub(led[i-1].SubmittedAt)
		min := s.DelayMin
		if i < s.WarmupCount {
			min = s.WarmupDelay
		}
		if gap < min {
			t.Errorf("gap before send %d is %s, want >= %s", i, gap, min)
		}
	}
	job, _ := h.st.GetJob(h.job.ID)
	if !job.WarmupDone {
		t.Error("warm-up not marked done")
	}
	if o.Counts[store.MsgSent]+o.Counts[store.MsgDelivered] != 40 {
		t.Errorf("counts %v", o.Counts)
	}
}

func TestRollingWindowNeverExceeded(t *testing.T) {
	s := defaults(t)
	s.Per15m = 10
	s.DelayMin, s.DelayMax, s.WarmupDelay = 3*time.Second, 3*time.Second, 3*time.Second
	s.WarmupCount = 5
	h := newHarness(t, s, 35, monday10(s))
	o := h.run(t, context.Background())
	if o.State != store.JobDone {
		t.Fatalf("state %s: %s", o.State, o.Reason)
	}
	led := h.ledger(t)
	for i := range led {
		n := 0
		for j := range led {
			if !led[j].SubmittedAt.Before(led[i].SubmittedAt) && led[j].SubmittedAt.Before(led[i].SubmittedAt.Add(Window)) {
				n++
			}
		}
		if n > s.Per15m {
			t.Fatalf("%d sends in the 15 minutes from %s, cap %d", n, led[i].SubmittedAt, s.Per15m)
		}
	}
}

func TestLedgerCountsAcrossJobs(t *testing.T) {
	s := defaults(t)
	s.Per15m = 10
	s.WarmupCount = 5
	h := newHarness(t, s, 8, monday10(s))
	if o := h.run(t, context.Background()); o.State != store.JobDone {
		t.Fatalf("first job: %s %s", o.State, o.Reason)
	}
	h.job = h.addJob(t, 8)
	if o := h.run(t, context.Background()); o.State != store.JobDone {
		t.Fatalf("second job: %s %s", o.State, o.Reason)
	}
	led := h.ledger(t)
	if gap := led[10].SubmittedAt.Sub(led[0].SubmittedAt); gap < Window {
		t.Fatalf("11th send only %s after the first; the window spans jobs", gap)
	}
}

func TestHoursEndPauses(t *testing.T) {
	s := defaults(t)
	start := monday10(s).Add(9*time.Hour + 58*time.Minute) // 19:58
	h := newHarness(t, s, 30, start)
	o := h.run(t, context.Background())
	if o.State != store.JobPaused || !strings.Contains(o.Reason, "outside sending hours") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	for _, e := range h.ledger(t) {
		if !s.InHours(e.SubmittedAt) {
			t.Fatalf("sent at %s, outside hours", e.SubmittedAt)
		}
	}
}

func TestDailyCapPauses(t *testing.T) {
	s := defaults(t)
	s.PerDay = 7
	s.WarmupCount = 5
	h := newHarness(t, s, 20, monday10(s))
	o := h.run(t, context.Background())
	if o.State != store.JobPaused || !strings.Contains(o.Reason, "daily cap") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	if len(h.gw.Accepted) != 7 {
		t.Fatalf("sent %d, cap 7", len(h.gw.Accepted))
	}
}

func TestWarmupFailureStops(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 40, monday10(s))
	h.gw.Outcome = func(m gateway.OutMessage) []gateway.Status {
		if strings.HasSuffix(m.ID, "-0004") {
			return []gateway.Status{{State: gateway.Failed, Error: "RESULT_ERROR_GENERIC_FAILURE"}}
		}
		return []gateway.Status{{State: gateway.Sent}}
	}
	o := h.run(t, context.Background())
	if o.State != store.JobStopped || !strings.Contains(o.Reason, "warm-up failed") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	if len(h.gw.Accepted) != s.WarmupCount {
		t.Fatalf("sent %d before the gate decided, want %d", len(h.gw.Accepted), s.WarmupCount)
	}
}

func TestWarmupTimeoutStops(t *testing.T) {
	s := defaults(t)
	s.WarmupCount = 5
	h := newHarness(t, s, 20, monday10(s))
	h.gw.Outcome = func(m gateway.OutMessage) []gateway.Status {
		return []gateway.Status{{State: gateway.Processed}} // never resolves
	}
	o := h.run(t, context.Background())
	if o.State != store.JobStopped || !strings.Contains(o.Reason, "unresolved") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
}

func TestBreakerConsecutive(t *testing.T) {
	s := defaults(t)
	s.WarmupCount = 5
	h := newHarness(t, s, 40, monday10(s))
	h.gw.Outcome = func(m gateway.OutMessage) []gateway.Status {
		var row int
		fmt.Sscanf(m.ID[strings.LastIndex(m.ID, "-")+1:], "%d", &row)
		if row >= 12 {
			return []gateway.Status{{State: gateway.Failed, Error: "blocked"}}
		}
		return []gateway.Status{{State: gateway.Sent}}
	}
	o := h.run(t, context.Background())
	if o.State != store.JobStopped || !strings.Contains(o.Reason, "consecutive") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	if len(h.gw.Accepted) > 16 {
		t.Fatalf("kept sending after failures: %d sent", len(h.gw.Accepted))
	}
}

func TestStuckPendingPauses(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 10, monday10(s))
	h.gw.Outcome = func(m gateway.OutMessage) []gateway.Status {
		if strings.HasSuffix(m.ID, "-0003") {
			return []gateway.Status{{State: gateway.Pending}}
		}
		return []gateway.Status{{State: gateway.Sent}}
	}
	o := h.run(t, context.Background())
	if o.State != store.JobPaused || !strings.Contains(o.Reason, "Pending") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	if len(h.gw.Accepted) != 2 {
		t.Fatalf("sent %d; nothing should follow a stuck message", len(h.gw.Accepted))
	}
}

func TestUnreachablePhoneStops(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 10, monday10(s))
	down := &gateway.UnavailableError{Err: fmt.Errorf("connection refused")}
	h.gw.SendErrs = []error{down}
	h.gw.StatusErr = down
	o := h.run(t, context.Background())
	if o.State != store.JobStopped || !strings.Contains(o.Reason, "unreachable") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	if len(h.gw.Accepted) != 0 {
		t.Fatalf("accepted %d", len(h.gw.Accepted))
	}
}

func TestUnauthorizedStops(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 5, monday10(s))
	h.gw.SendErrs = []error{gateway.ErrUnauthorized}
	o := h.run(t, context.Background())
	if o.State != store.JobStopped || !strings.Contains(o.Reason, "HTTP 401") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	m, _ := h.st.GetMessage(h.job.ID, 2)
	if m.State != store.MsgQueued {
		t.Fatalf("row 2 is %s, want queued", m.State)
	}
}

func TestLostResponseIsNotResent(t *testing.T) {
	s := defaults(t)
	s.WarmupCount = 5
	h := newHarness(t, s, 6, monday10(s))
	// The phone queues row 2 but the response is lost.
	h.gw.SendErrs = []error{&gateway.UnavailableError{Err: fmt.Errorf("timeout")}}
	h.gw.AcceptOnError = true
	o := h.run(t, context.Background())
	if o.State != store.JobDone {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	assertUniqueSends(t, h.gw)
	if h.gw.SendCalls != 6 {
		t.Fatalf("send calls %d, want 6 (row 2 adopted, not resent)", h.gw.SendCalls)
	}
}

func TestResumeReconcilesSubmitting(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 4, monday10(s))
	now := h.clock.t
	// Row 2: crashed after the POST reached the phone. Row 3: crashed before the POST.
	m2, _ := h.st.GetMessage(h.job.ID, 2)
	m3, _ := h.st.GetMessage(h.job.ID, 3)
	h.st.BeginSubmit(m2, 1, now)
	h.gw.Send(context.Background(), gateway.OutMessage{ID: m2.MsgID, Phone: m2.Phone, SIM: 1})
	h.st.BeginSubmit(m3, 1, now)
	h.st.SetJobState(h.job.ID, store.JobPaused, "", now)
	h.clock.t = now.Add(time.Minute)

	o := h.run(t, context.Background())
	if o.State != store.JobDone {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	assertUniqueSends(t, h.gw)
	if len(h.gw.Accepted) != 4 {
		t.Fatalf("accepted %d, want 4", len(h.gw.Accepted))
	}
}

func TestResumeLookupErrorStops(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 3, monday10(s))
	m2, _ := h.st.GetMessage(h.job.ID, 2)
	h.st.BeginSubmit(m2, 1, h.clock.t)
	h.gw.StatusErr = &gateway.UnavailableError{Err: fmt.Errorf("no route to host")}
	o := h.run(t, context.Background())
	if o.State != store.JobStopped || !strings.Contains(o.Reason, "cannot confirm") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	if h.gw.SendCalls != 0 {
		t.Fatal("sent with an unknown outcome outstanding")
	}
}

func TestInterruptAndResume(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 25, monday10(s))
	ctx, cancel := context.WithCancel(context.Background())
	h.clock.onSleep = func() {
		if len(h.gw.Accepted) == 7 {
			cancel()
		}
	}
	o := h.run(t, ctx)
	if o.State != store.JobPaused || !strings.Contains(o.Reason, "interrupted") {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	if len(h.gw.Accepted) != 7 {
		t.Fatalf("accepted %d after interrupt, want 7", len(h.gw.Accepted))
	}
	h.clock.onSleep = nil
	o = h.run(t, context.Background())
	if o.State != store.JobDone {
		t.Fatalf("resume: %s: %s", o.State, o.Reason)
	}
	assertUniqueSends(t, h.gw)
	if len(h.gw.Accepted) != 25 {
		t.Fatalf("accepted %d, want 25", len(h.gw.Accepted))
	}
}

func TestResumeWarmupIsShort(t *testing.T) {
	s := defaults(t)
	h := newHarness(t, s, 30, monday10(s))
	h.st.SetWarmupDone(h.job.ID)
	o := h.run(t, context.Background())
	if o.State != store.JobDone {
		t.Fatalf("got %s: %s", o.State, o.Reason)
	}
	led := h.ledger(t)
	if gap := led[4].SubmittedAt.Sub(led[3].SubmittedAt); gap < s.WarmupDelay {
		t.Errorf("5th resume send gap %s, want warm-up %s", gap, s.WarmupDelay)
	}
	if gap := led[6].SubmittedAt.Sub(led[5].SubmittedAt); gap >= s.WarmupDelay {
		t.Errorf("7th resume send gap %s, want full pace", gap)
	}
}
