// Package engine is the only path to the phone. Every message passes six
// gates in order (hours, daily cap, rolling window, spacing, backpressure,
// breaker) before it is submitted, so every limit is enforced in one place.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sort"
	"time"

	"sms/internal/config"
	"sms/internal/gateway"
	"sms/internal/msg"
	"sms/internal/store"
)

const (
	// Window is the rolling-limit window. It is fixed, not configurable.
	Window = 15 * time.Minute
	// BreakerWindow is how many resolved messages the failure rate looks at.
	BreakerWindow = 20
	// ResumeWarmup is the warm-up length when resuming a job that passed its gate.
	ResumeWarmup = 5
	// MaxGatewayErrors in a row stop the job: the phone is probably offline.
	MaxGatewayErrors = 3
	// DeliveryGrace is how long a finished job keeps polling for delivery reports.
	DeliveryGrace = 30 * time.Second
	// DeliveryTail is how long after its TTL a Sent message is polled for Delivered.
	DeliveryTail = 5 * time.Minute

	sentPollEvery = 30 * time.Second
	sentPollBatch = 5
)

// Clock is injectable so tests can run hours of pacing instantly.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Engine sends messages for jobs.
type Engine struct {
	Store    *store.Store
	GW       gateway.Gateway
	S        config.Settings
	Clock    Clock
	Rand     *rand.Rand
	Progress io.Writer // one line per event; nil for none
}

// Outcome is how a run ended.
type Outcome struct {
	JobID    string
	State    string // store.JobDone, JobPaused or JobStopped
	Reason   string
	Counts   map[string]int
	Failures []string // recent failure reasons
}

// halt ends a run as paused or stopped. It travels as an error so every
// gate can return it directly.
type halt struct {
	state, reason string
}

func (h *halt) Error() string { return h.state + ": " + h.reason }

func pause(format string, a ...any) error {
	return &halt{store.JobPaused, fmt.Sprintf(format, a...)}
}

func stop(format string, a ...any) error {
	return &halt{store.JobStopped, fmt.Sprintf(format, a...)}
}

var errInterrupted = &halt{store.JobPaused, "interrupted"}

type run struct {
	e          *Engine
	job        *store.Job
	start      time.Time
	prev       *store.Message
	gwErrs     int
	lastErr    error
	lastPoll   time.Time
	submitted  int
	warmTarget int
	warmRows   []int
	draining   bool
}

func (r *run) now() time.Time { return r.e.Clock.Now() }

func (r *run) logf(format string, a ...any) {
	if r.e.Progress != nil {
		fmt.Fprintf(r.e.Progress, "%s  %s\n", r.now().In(r.e.S.TZ).Format("15:04:05"), fmt.Sprintf(format, a...))
	}
}

// RunJob sends a job's remaining messages. It applies the resume rules first,
// which do nothing for a new job: messages left mid-submission are looked up,
// never blindly resent.
func (e *Engine) RunJob(ctx context.Context, jobID string) (*Outcome, error) {
	job, err := e.Store.GetJob(jobID)
	if err != nil {
		return nil, err
	}
	r := &run{e: e, job: job, start: e.Clock.Now()}
	if err := e.Store.SetJobState(job.ID, store.JobRunning, "", r.start); err != nil {
		return nil, err
	}
	err = r.execute(ctx)
	state, reason := store.JobDone, ""
	var h *halt
	switch {
	case errors.As(err, &h):
		state, reason = h.state, h.reason
	case err != nil:
		e.Store.SetJobState(job.ID, store.JobPaused, "internal error: "+err.Error(), e.Clock.Now())
		return nil, err
	}
	if err := e.Store.SetJobState(job.ID, state, reason, e.Clock.Now()); err != nil {
		return nil, err
	}
	return e.outcome(job.ID, state, reason)
}

func (e *Engine) outcome(jobID, state, reason string) (*Outcome, error) {
	counts, err := e.Store.Counts(jobID)
	if err != nil {
		return nil, err
	}
	failed, err := e.Store.Messages(jobID, store.MsgFailed, store.MsgExpired)
	if err != nil {
		return nil, err
	}
	o := &Outcome{JobID: jobID, State: state, Reason: reason, Counts: counts}
	sort.Slice(failed, func(i, j int) bool { return failed[i].ResolvedAt.After(failed[j].ResolvedAt) })
	for i, m := range failed {
		if i == 5 {
			break
		}
		o.Failures = append(o.Failures, fmt.Sprintf("row %d %s: %s", m.RowNo, m.Phone, orDash(m.Error)))
	}
	return o, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (r *run) execute(ctx context.Context) error {
	if err := r.reconcile(); err != nil {
		return err
	}
	prev, err := r.e.Store.LastSubmitted(r.job.ID)
	if err != nil {
		return err
	}
	r.prev = prev
	if r.job.Kind == store.KindBulk {
		r.warmTarget = r.e.S.WarmupCount
		if r.job.WarmupDone {
			r.warmTarget = min(ResumeWarmup, r.e.S.WarmupCount)
		}
	}
	warmChecked := r.warmTarget == 0
	if !warmChecked {
		r.logf("warm-up: first %d messages at %s before full pace", r.warmTarget, config.FormatDuration(r.e.S.WarmupDelay))
	}
	for {
		m, err := r.e.Store.NextToSend(r.job.ID)
		if err != nil {
			return err
		}
		if m == nil {
			break
		}
		warm := r.submitted < r.warmTarget
		if err := r.gates(ctx, m, warm); err != nil {
			return err
		}
		counted, err := r.submit(m)
		if err != nil {
			return err
		}
		if counted && warm {
			r.warmRows = append(r.warmRows, m.RowNo)
		}
		if !warmChecked && r.submitted >= r.warmTarget {
			warmChecked = true
			if err := r.warmupGate(ctx); err != nil {
				return err
			}
		}
	}
	if !warmChecked && len(r.warmRows) > 0 {
		if err := r.warmupGate(ctx); err != nil {
			return err
		}
	}
	return r.drain(ctx)
}

// gates blocks until m may be submitted, or returns a halt.
func (r *run) gates(ctx context.Context, m *store.Message, warm bool) error {
	s := r.e.S
	if m.Segments > s.Per15m || m.Segments > s.PerDay {
		return stop("row %d needs %d segments, more than limit.per_15m (%d) or limit.per_day (%d) allow", m.RowNo, m.Segments, s.Per15m, s.PerDay)
	}
	delay := s.WarmupDelay
	if !warm {
		delay = s.DelayMin + time.Duration(r.e.Rand.Int64N(int64(s.DelayMax-s.DelayMin)+1))
	}
	for {
		if ctx.Err() != nil {
			return errInterrupted
		}
		now := r.now()

		// 1. Hours.
		if !s.InHours(now) {
			return pause("outside sending hours (%s); next window opens %s", s.HoursLabel(), s.NextStart(now).Format("Mon 15:04"))
		}

		// 2. Daily cap.
		day, err := r.e.Store.SegmentsSince(s.DayStart(now))
		if err != nil {
			return err
		}
		if day+m.Segments > s.PerDay {
			return pause("daily cap reached: %d/%d segments today; resets at midnight %s", day, s.PerDay, s.TZ)
		}

		// 3. Rolling window.
		entries, err := r.e.Store.LedgerSince(now.Add(-Window))
		if err != nil {
			return err
		}
		if w := windowWait(entries, now, m.Segments, s.Per15m); w > 0 {
			if w >= 20*time.Second {
				r.logf("rolling cap: %d/%d in last 15m; waiting %s", sumSegments(entries), s.Per15m, w.Round(time.Second))
			}
			if err := r.wait(ctx, w); err != nil {
				return err
			}
			continue
		}

		// 4. Spacing.
		last, err := r.e.Store.LastLedger()
		if err != nil {
			return err
		}
		if gap := now.Sub(last); !last.IsZero() && gap < delay {
			if err := r.wait(ctx, delay-gap); err != nil {
				return err
			}
			continue
		}

		// 5. Backpressure.
		if r.prev != nil && r.prev.RowNo != m.RowNo {
			prev, err := r.e.Store.GetMessage(r.prev.JobID, r.prev.RowNo)
			if err != nil {
				return err
			}
			r.prev = prev
			if prev.State == store.MsgPending {
				age := now.Sub(prev.SubmittedAt)
				if age >= s.StuckAfter {
					return pause("%s", stuckReason(prev, age))
				}
				if err := r.wait(ctx, min(s.PollInterval, s.StuckAfter-age)); err != nil {
					return err
				}
				continue
			}
		}

		// 6. Breaker.
		return r.breaker()
	}
}

func stuckReason(m *store.Message, age time.Duration) string {
	return fmt.Sprintf("row %d has been Pending for %s; check the phone for Android's \"send many messages?\" confirmation or a killed SMSGate app, then resume",
		m.RowNo, age.Round(time.Second))
}

// windowWait is how long until seg more segments fit in the rolling window.
// entries are the ledger rows inside the window, oldest first.
func windowWait(entries []store.LedgerEntry, now time.Time, seg, limit int) time.Duration {
	used := 0
	for _, e := range entries {
		if e.SubmittedAt.After(now.Add(-Window)) {
			used += e.Segments
		}
	}
	excess := used + seg - limit
	if excess <= 0 {
		return 0
	}
	freed := 0
	for _, e := range entries {
		if !e.SubmittedAt.After(now.Add(-Window)) {
			continue
		}
		freed += e.Segments
		if freed >= excess {
			return e.SubmittedAt.Add(Window).Sub(now) + time.Millisecond
		}
	}
	return Window
}

func sumSegments(entries []store.LedgerEntry) int {
	n := 0
	for _, e := range entries {
		n += e.Segments
	}
	return n
}

// wait sleeps for d while polling statuses and watching the breaker.
func (r *run) wait(ctx context.Context, d time.Duration) error {
	end := r.now().Add(d)
	for {
		now := r.now()
		if !now.Before(end) {
			return nil
		}
		if err := r.e.Clock.Sleep(ctx, min(end.Sub(now), r.e.S.PollInterval)); err != nil {
			return errInterrupted
		}
		if err := r.pollDue(); err != nil {
			return err
		}
		if !r.draining {
			if err := r.breaker(); err != nil {
				return err
			}
		}
	}
}

func (r *run) pollDue() error {
	now := r.now()
	if now.Sub(r.lastPoll) < r.e.S.PollInterval {
		return nil
	}
	r.lastPoll = now
	return r.poll()
}

// poll refreshes in-flight messages every interval, and a few Sent ones
// awaiting a delivery report, least recently checked first.
func (r *run) poll() error {
	now := r.now()
	inflight, err := r.e.Store.Messages(r.job.ID, store.MsgPending, store.MsgProcessed)
	if err != nil {
		return err
	}
	sent, err := r.e.Store.Messages(r.job.ID, store.MsgSent)
	if err != nil {
		return err
	}
	sort.Slice(sent, func(i, j int) bool { return sent[i].CheckedAt.Before(sent[j].CheckedAt) })
	every := sentPollEvery
	if r.draining {
		// Nothing else is in flight: poll for delivery reports at full rate.
		every = r.e.S.PollInterval
	}
	batch := inflight
	for _, m := range sent {
		if len(batch)-len(inflight) == sentPollBatch {
			break
		}
		if now.Before(m.SubmittedAt.Add(r.e.S.TTL+DeliveryTail)) && now.Sub(m.CheckedAt) >= every {
			batch = append(batch, m)
		}
	}
	for _, m := range batch {
		st, err := r.e.GW.Status(context.Background(), m.MsgID)
		switch {
		case err == nil:
			r.gwErrs = 0
			if err := r.apply(m, st); err != nil {
				return err
			}
		case errors.Is(err, gateway.ErrNotFound):
			r.gwErrs = 0
			if err := r.e.Store.Touch(m, now); err != nil {
				return err
			}
		case errors.Is(err, gateway.ErrUnauthorized):
			return stop("%v", err)
		default:
			// Stop polling this round; the next tick tries again.
			return r.gatewayError(err)
		}
	}
	return nil
}

func (r *run) gatewayError(err error) error {
	r.gwErrs++
	r.lastErr = err
	if r.gwErrs >= MaxGatewayErrors {
		return stop("phone unreachable: %d gateway errors in a row (last: %v); check the phone is online and SMSGate is running", r.gwErrs, err)
	}
	return nil
}

func mapState(s gateway.State) string {
	switch s {
	case gateway.Pending:
		return store.MsgPending
	case gateway.Processed:
		return store.MsgProcessed
	case gateway.Sent:
		return store.MsgSent
	case gateway.Delivered:
		return store.MsgDelivered
	case gateway.Failed:
		return store.MsgFailed
	}
	return ""
}

// apply records the gateway's state for m.
func (r *run) apply(m *store.Message, st gateway.Status) error {
	now := r.now()
	state := mapState(st.State)
	if state == "" || state == m.State || store.IsFinal(m.State) {
		return r.e.Store.Touch(m, now)
	}
	if err := r.e.Store.UpdateState(m, state, st.Error, now); err != nil {
		return err
	}
	if state == store.MsgFailed {
		r.logf("row %-5d %s  failed: %s", m.RowNo, msg.Mask(m.Phone), orDash(st.Error))
	}
	return nil
}

// breaker returns a stop when the recent failure pattern suggests a block.
func (r *run) breaker() error {
	s := r.e.S
	recent, err := r.e.Store.ResolvedSince(r.job.ID, r.start, BreakerWindow)
	if err != nil {
		return err
	}
	consec, fails := 0, 0
	counting := true
	for _, m := range recent {
		if store.IsFailure(m.State) {
			fails++
			if counting {
				consec++
			}
		} else {
			counting = false
		}
	}
	if consec >= s.BreakerConsec {
		return stop("circuit breaker: %d consecutive failures", consec)
	}
	if fails*100 >= s.BreakerRate*BreakerWindow {
		return stop("circuit breaker: %d failures in the last %d resolved messages (limit %d%% of %d)", fails, len(recent), s.BreakerRate, BreakerWindow)
	}
	return nil
}

// submit sends m. counted reports whether a new submission was made.
func (r *run) submit(m *store.Message) (counted bool, err error) {
	s := r.e.S
	if m.State == store.MsgSubmitting {
		// A previous POST failed with an unknown outcome: look before resending.
		st, err := r.e.GW.Status(context.Background(), m.MsgID)
		switch {
		case err == nil:
			r.gwErrs = 0
			r.prev = m
			r.logf("row %-5d %s  already on the phone (%s), not resent", m.RowNo, msg.Mask(m.Phone), st.State)
			if mapState(st.State) == "" {
				st.State = gateway.Pending
			}
			return false, r.e.Store.UpdateState(m, mapState(st.State), st.Error, r.now())
		case errors.Is(err, gateway.ErrNotFound):
			r.gwErrs = 0
		case errors.Is(err, gateway.ErrUnauthorized):
			return false, stop("%v", err)
		case gateway.IsUnavailable(err):
			return false, r.gatewayError(err)
		default:
			return false, stop("cannot confirm whether row %d was sent (%v); check the phone, then resume", m.RowNo, err)
		}
	}

	now := r.now()
	if err := r.e.Store.BeginSubmit(m, r.job.SIM, now); err != nil {
		return false, err
	}
	st, err := r.e.GW.Send(context.Background(), gateway.OutMessage{
		ID: m.MsgID, Phone: m.Phone, Text: m.Text, SIM: r.job.SIM, TTL: s.TTL,
	})
	var rejected *gateway.RejectedError
	switch {
	case err == nil:
		r.gwErrs = 0
		state := mapState(st.State)
		if state == "" {
			state = store.MsgPending
		}
		if err := r.e.Store.UpdateState(m, state, st.Error, r.now()); err != nil {
			return false, err
		}
	case errors.Is(err, gateway.ErrDuplicate):
		st, lerr := r.e.GW.Status(context.Background(), m.MsgID)
		if lerr != nil {
			// Leave it submitting; the next attempt looks it up again.
			if errors.Is(lerr, gateway.ErrUnauthorized) {
				return false, stop("%v", lerr)
			}
			return false, r.gatewayError(fmt.Errorf("duplicate id but status lookup failed: %w", lerr))
		}
		r.gwErrs = 0
		if mapState(st.State) == "" {
			st.State = gateway.Pending
		}
		if err := r.e.Store.UpdateState(m, mapState(st.State), st.Error, r.now()); err != nil {
			return false, err
		}
	case errors.Is(err, gateway.ErrUnauthorized):
		r.e.Store.UpdateState(m, store.MsgQueued, "", r.now())
		return false, stop("%v", err)
	case errors.As(err, &rejected):
		r.gwErrs = 0
		if err := r.e.Store.UpdateState(m, store.MsgFailed, err.Error(), r.now()); err != nil {
			return false, err
		}
	case gateway.IsUnavailable(err):
		r.logf("row %-5d %s  gateway error, will look it up before retrying: %v", m.RowNo, msg.Mask(m.Phone), err)
		return false, r.gatewayError(err)
	default:
		return false, stop("sending row %d: %v", m.RowNo, err)
	}
	r.prev = m
	r.submitted++
	return true, r.progress(m)
}

func (r *run) progress(m *store.Message) error {
	s := r.e.S
	now := r.now()
	w, err := r.e.Store.SegmentsSince(now.Add(-Window))
	if err != nil {
		return err
	}
	d, err := r.e.Store.SegmentsSince(s.DayStart(now))
	if err != nil {
		return err
	}
	tag := ""
	if r.submitted <= r.warmTarget {
		tag = fmt.Sprintf("  [warm-up %d/%d]", r.submitted, r.warmTarget)
	}
	line := fmt.Sprintf("row %-5d %s  %-9s 15m %d/%d  today %d/%d%s", m.RowNo, msg.Mask(m.Phone), m.State, w, s.Per15m, d, s.PerDay, tag)
	if m.State == store.MsgFailed {
		line += "  " + m.Error
	}
	r.logf("%s", line)
	return nil
}

// warmupGate waits for every warm-up message to resolve. Any failure, or
// anything unresolved at the timeout, stops the job.
func (r *run) warmupGate(ctx context.Context) error {
	s := r.e.S
	deadline := r.now().Add(s.WarmupGate)
	r.logf("warm-up: waiting up to %s for %d messages to resolve", config.FormatDuration(s.WarmupGate), len(r.warmRows))
	for {
		unresolved := 0
		for _, row := range r.warmRows {
			m, err := r.e.Store.GetMessage(r.job.ID, row)
			if err != nil {
				return err
			}
			if store.IsFailure(m.State) {
				return stop("warm-up failed: row %d %s ended %s (%s); check the SIM and phone before resuming", m.RowNo, m.Phone, m.State, orDash(m.Error))
			}
			if !store.IsResolved(m.State) {
				unresolved++
			}
		}
		if unresolved == 0 {
			r.logf("warm-up passed: %d/%d sent cleanly; continuing at full pace", len(r.warmRows), len(r.warmRows))
			return r.e.Store.SetWarmupDone(r.job.ID)
		}
		now := r.now()
		if !now.Before(deadline) {
			return stop("warm-up: %d of %d messages still unresolved after %s; check the phone", unresolved, len(r.warmRows), config.FormatDuration(s.WarmupGate))
		}
		if err := r.wait(ctx, min(s.PollInterval, deadline.Sub(now))); err != nil {
			return err
		}
	}
}

// drain waits for in-flight messages to leave Pending, then briefly for
// delivery reports. Nothing is submitted here.
func (r *run) drain(ctx context.Context) error {
	s := r.e.S
	r.draining = true
	var graceEnd time.Time
	for {
		now := r.now()
		inflight, err := r.e.Store.Messages(r.job.ID, store.MsgPending, store.MsgProcessed)
		if err != nil {
			return err
		}
		waiting := 0
		for _, m := range inflight {
			age := now.Sub(m.SubmittedAt)
			if m.State == store.MsgPending && age >= s.StuckAfter {
				return pause("%s", stuckReason(m, age))
			}
			if age < s.TTL+DeliveryTail {
				waiting++
			}
		}
		if waiting > 0 {
			if err := r.wait(ctx, s.PollInterval); err != nil {
				return err
			}
			continue
		}
		if graceEnd.IsZero() {
			graceEnd = now.Add(DeliveryGrace)
		}
		sent, err := r.e.Store.Messages(r.job.ID, store.MsgSent)
		if err != nil {
			return err
		}
		if len(sent) == 0 || !now.Before(graceEnd) || ctx.Err() != nil {
			return nil
		}
		if err := r.wait(ctx, min(s.PollInterval, graceEnd.Sub(now))); err != nil {
			var h *halt
			if errors.As(err, &h) {
				return nil // everything was sent; a late hiccup doesn't change that
			}
			return err
		}
	}
}

// reconcile applies the resume rules before any new submission.
func (r *run) reconcile() error {
	now := r.now()
	subs, err := r.e.Store.Messages(r.job.ID, store.MsgSubmitting)
	if err != nil {
		return err
	}
	for _, m := range subs {
		st, err := r.e.GW.Status(context.Background(), m.MsgID)
		switch {
		case err == nil:
			if mapState(st.State) == "" {
				st.State = gateway.Pending
			}
			r.logf("row %-5d %s  found on the phone (%s), not resent", m.RowNo, msg.Mask(m.Phone), st.State)
			if err := r.e.Store.UpdateState(m, mapState(st.State), st.Error, now); err != nil {
				return err
			}
		case errors.Is(err, gateway.ErrNotFound):
			if err := r.e.Store.UpdateState(m, store.MsgQueued, "", now); err != nil {
				return err
			}
		case errors.Is(err, gateway.ErrUnauthorized):
			return stop("%v", err)
		default:
			return stop("cannot confirm whether row %d was sent (%v); the outcome is unknown, so nothing was resent", m.RowNo, err)
		}
	}
	pending, err := r.e.Store.Messages(r.job.ID, store.MsgPending)
	if err != nil {
		return err
	}
	for _, m := range pending {
		if now.Before(m.SubmittedAt.Add(r.e.S.TTL)) {
			continue
		}
		st, err := r.e.GW.Status(context.Background(), m.MsgID)
		switch {
		case err == nil && mapState(st.State) != "" && mapState(st.State) != store.MsgPending:
			if err := r.e.Store.UpdateState(m, mapState(st.State), st.Error, now); err != nil {
				return err
			}
		case err == nil || errors.Is(err, gateway.ErrNotFound):
			if err := r.e.Store.UpdateState(m, store.MsgExpired, "still Pending after its TTL", now); err != nil {
				return err
			}
		case errors.Is(err, gateway.ErrUnauthorized):
			return stop("%v", err)
		default:
			return stop("cannot check row %d on the phone: %v", m.RowNo, err)
		}
	}
	return nil
}

// Blocked reports why a message of seg segments could not be sent right now
// because of the hours window or the daily cap, or "" if it could.
func (e *Engine) Blocked(seg int) (string, error) {
	s := e.S
	now := e.Clock.Now()
	if !s.InHours(now) {
		return fmt.Sprintf("outside sending hours (%s); next window opens %s", s.HoursLabel(), s.NextStart(now).Format("Mon 15:04")), nil
	}
	day, err := e.Store.SegmentsSince(s.DayStart(now))
	if err != nil {
		return "", err
	}
	if day+seg > s.PerDay {
		return fmt.Sprintf("daily cap reached: %d/%d segments today; resets at midnight %s", day, s.PerDay, s.TZ), nil
	}
	return "", nil
}

// Quota is current usage against both caps.
type Quota struct {
	Window, WindowLimit int
	Day, DayLimit       int
	NextSlot            time.Time // when one more segment fits the window; zero if now
	InHours             bool
	NextStart           time.Time
}

// Quota reports usage from the ledger.
func (e *Engine) Quota() (Quota, error) {
	s := e.S
	now := e.Clock.Now()
	entries, err := e.Store.LedgerSince(now.Add(-Window))
	if err != nil {
		return Quota{}, err
	}
	day, err := e.Store.SegmentsSince(s.DayStart(now))
	if err != nil {
		return Quota{}, err
	}
	q := Quota{Window: sumSegments(entries), WindowLimit: s.Per15m, Day: day, DayLimit: s.PerDay, InHours: s.InHours(now), NextStart: s.NextStart(now)}
	if w := windowWait(entries, now, 1, s.Per15m); w > 0 {
		q.NextSlot = now.Add(w)
	}
	return q, nil
}
