// Package store is the SQLite state: jobs, per-message state and the
// append-only send ledger that drives every rate limit.
package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Job states. Paused jobs resume on their own; stopped ones need a person.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobPaused    = "paused"
	JobStopped   = "stopped"
	JobDone      = "done"
	JobCancelled = "cancelled"
)

// Job kinds.
const (
	KindBulk = "bulk"
	KindSend = "send"
)

// Message states. Pending, Processed, Sent, Delivered and Failed mirror
// SMSGate; the rest are our own.
const (
	MsgQueued     = "queued"
	MsgSubmitting = "submitting"
	MsgPending    = "pending"
	MsgProcessed  = "processed"
	MsgSent       = "sent"
	MsgDelivered  = "delivered"
	MsgFailed     = "failed"
	MsgExpired    = "expired"
	MsgCancelled  = "cancelled"
)

// LedgerRetention is how long ledger rows are kept.
const LedgerRetention = 7 * 24 * time.Hour

// IsFinal reports whether no further state change is expected.
func IsFinal(state string) bool {
	switch state {
	case MsgDelivered, MsgFailed, MsgExpired, MsgCancelled:
		return true
	}
	return false
}

// IsResolved reports whether the outcome of a submission is known: Sent counts
// as success even though a delivery report may still arrive.
func IsResolved(state string) bool {
	return state == MsgSent || state == MsgDelivered || state == MsgFailed || state == MsgExpired
}

// IsFailure reports whether a state counts against the breaker.
func IsFailure(state string) bool { return state == MsgFailed || state == MsgExpired }

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id          TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,
	idem_key    TEXT NOT NULL DEFAULT '',
	sim         INTEGER NOT NULL,
	state       TEXT NOT NULL,
	stop_reason TEXT NOT NULL DEFAULT '',
	warmup_done INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_pick ON jobs(state, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_idem ON jobs(idem_key) WHERE idem_key != '';
CREATE TABLE IF NOT EXISTS messages (
	job_id       TEXT NOT NULL REFERENCES jobs(id),
	row_no       INTEGER NOT NULL,
	phone        TEXT NOT NULL,
	text         TEXT NOT NULL,
	segments     INTEGER NOT NULL,
	encoding     TEXT NOT NULL,
	msg_id       TEXT NOT NULL UNIQUE,
	state        TEXT NOT NULL,
	error        TEXT NOT NULL DEFAULT '',
	attempts     INTEGER NOT NULL DEFAULT 0,
	submitted_at INTEGER NOT NULL DEFAULT 0,
	resolved_at  INTEGER NOT NULL DEFAULT 0,
	checked_at   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (job_id, row_no)
);
CREATE INDEX IF NOT EXISTS messages_state ON messages(job_id, state);
CREATE TABLE IF NOT EXISTS ledger (
	msg_id       TEXT NOT NULL,
	sim          INTEGER NOT NULL,
	segments     INTEGER NOT NULL,
	submitted_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ledger_time ON ledger(submitted_at);
`

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "sms.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Recover runs once at startup, before anything sends: jobs left running by
// a crashed process become paused, and old ledger rows are pruned.
func (s *Store) Recover(now time.Time) error {
	if _, err := s.db.Exec(`UPDATE jobs SET state=?, stop_reason=?, updated_at=? WHERE state=?`,
		JobPaused, "interrupted: the server stopped without a checkpoint", ms(now), JobRunning); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM ledger WHERE submitted_at < ?`, ms(now.Add(-LedgerRetention)))
	return err
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// Job is one bulk run or single send.
type Job struct {
	ID         string
	Kind       string
	IdemKey    string // client's Idempotency-Key; "" if none
	SIM        int
	State      string
	StopReason string
	WarmupDone bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Message is one recipient of a job and its delivery state. RowNo is the
// 1-based position in the request.
type Message struct {
	JobID       string
	RowNo       int
	Phone       string
	Text        string
	Segments    int
	Encoding    string
	MsgID       string
	State       string
	Error       string
	Attempts    int
	SubmittedAt time.Time
	ResolvedAt  time.Time
	CheckedAt   time.Time
}

const idAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// MsgID is the deterministic gateway id for a row.
func MsgID(jobID string, rowNo int) string { return fmt.Sprintf("%s-%04d", jobID, rowNo) }

// CreateJob inserts a job and all its rows in one transaction, assigning the
// job id and every message id.
func (s *Store) CreateJob(j *Job, msgs []Message, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for {
		j.ID = newID()
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM jobs WHERE id=?`, j.ID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	j.State, j.CreatedAt, j.UpdatedAt = JobQueued, now, now
	if _, err := tx.Exec(`INSERT INTO jobs (id, kind, idem_key, sim, state, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`, j.ID, j.Kind, j.IdemKey, j.SIM, j.State, ms(now), ms(now)); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO messages (job_id, row_no, phone, text, segments, encoding, msg_id, state, error)
		VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := range msgs {
		m := &msgs[i]
		m.JobID, m.MsgID = j.ID, MsgID(j.ID, m.RowNo)
		if _, err := stmt.Exec(m.JobID, m.RowNo, m.Phone, m.Text, m.Segments, m.Encoding, m.MsgID, m.State, m.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const jobCols = `id, kind, idem_key, sim, state, stop_reason, warmup_done, created_at, updated_at`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var created, updated int64
	if err := row.Scan(&j.ID, &j.Kind, &j.IdemKey, &j.SIM, &j.State, &j.StopReason, &j.WarmupDone, &created, &updated); err != nil {
		return nil, err
	}
	j.CreatedAt, j.UpdatedAt = fromMS(created), fromMS(updated)
	return &j, nil
}

// ErrNoJob means no job has that id.
var ErrNoJob = errors.New("no such job")

// GetJob loads a job by id.
func (s *Store) GetJob(id string) (*Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoJob
	}
	return j, err
}

// FindJobByKey returns the job created with this idempotency key, or nil.
func (s *Store) FindJobByKey(key string) (*Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE idem_key=?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ListJobs returns up to limit jobs, newest first.
func (s *Store) ListJobs(limit int) ([]*Job, error) {
	return s.queryJobs(`1 ORDER BY created_at DESC LIMIT ?`, limit)
}

// NextJob picks the job to run next: queued jobs first, then (when
// withPaused) paused jobs last touched before pausedBefore. Single sends go
// ahead of bulk jobs, oldest first within each.
func (s *Store) NextJob(withPaused bool, pausedBefore time.Time) (*Job, error) {
	jobs, err := s.queryJobs(`state=? ORDER BY kind!=?, created_at LIMIT 1`, JobQueued, KindSend)
	if err != nil || len(jobs) > 0 || !withPaused {
		return first(jobs), err
	}
	jobs, err = s.queryJobs(`state=? AND updated_at<? ORDER BY kind!=?, created_at LIMIT 1`, JobPaused, ms(pausedBefore), KindSend)
	return first(jobs), err
}

func first(jobs []*Job) *Job {
	if len(jobs) == 0 {
		return nil
	}
	return jobs[0]
}

func (s *Store) queryJobs(where string, args ...any) ([]*Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetJobState records a job's state and the reason for it.
func (s *Store) SetJobState(id, state, reason string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET state=?, stop_reason=?, updated_at=? WHERE id=?`, state, reason, ms(now), id)
	return err
}

// Cancel marks a job cancelled and drops its unsent messages. Messages
// already on the phone are left to finish.
func (s *Store) Cancel(id string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE messages SET state=?, resolved_at=? WHERE job_id=? AND state=?`, MsgCancelled, ms(now), id, MsgQueued); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET state=?, stop_reason=?, updated_at=? WHERE id=?`, JobCancelled, "cancelled by request", ms(now), id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetWarmupDone marks a job's warm-up gate as passed.
func (s *Store) SetWarmupDone(id string) error {
	_, err := s.db.Exec(`UPDATE jobs SET warmup_done=1 WHERE id=?`, id)
	return err
}

const msgCols = `job_id, row_no, phone, text, segments, encoding, msg_id, state, error, attempts, submitted_at, resolved_at, checked_at`

func (s *Store) queryMessages(where string, args ...any) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		var m Message
		var sub, res, chk int64
		if err := rows.Scan(&m.JobID, &m.RowNo, &m.Phone, &m.Text, &m.Segments, &m.Encoding, &m.MsgID, &m.State, &m.Error, &m.Attempts, &sub, &res, &chk); err != nil {
			return nil, err
		}
		m.SubmittedAt, m.ResolvedAt, m.CheckedAt = fromMS(sub), fromMS(res), fromMS(chk)
		out = append(out, &m)
	}
	return out, rows.Err()
}

func inStates(states []string) (string, []any) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := make([]any, len(states))
	for i, s := range states {
		args[i] = s
	}
	return "state IN (" + ph + ")", args
}

// Messages returns a job's messages in row order, optionally only those in states.
func (s *Store) Messages(jobID string, states ...string) ([]*Message, error) {
	if len(states) == 0 {
		return s.queryMessages(`job_id=? ORDER BY row_no`, jobID)
	}
	cond, args := inStates(states)
	return s.queryMessages(`job_id=? AND `+cond+` ORDER BY row_no`, append([]any{jobID}, args...)...)
}

// NextToSend is the first row still queued or left mid-submission.
func (s *Store) NextToSend(jobID string) (*Message, error) {
	ms, err := s.queryMessages(`job_id=? AND state IN (?,?) ORDER BY row_no LIMIT 1`, jobID, MsgQueued, MsgSubmitting)
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	return ms[0], nil
}

// LastSubmitted is the job's most recently submitted row, or nil.
func (s *Store) LastSubmitted(jobID string) (*Message, error) {
	ms, err := s.queryMessages(`job_id=? AND submitted_at>0 ORDER BY submitted_at DESC, row_no DESC LIMIT 1`, jobID)
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	return ms[0], nil
}

// ResolvedSince returns the job's rows submitted at or after t whose outcome
// is known, newest submission first, up to limit.
func (s *Store) ResolvedSince(jobID string, t time.Time, limit int) ([]*Message, error) {
	return s.queryMessages(`job_id=? AND submitted_at>=? AND state IN (?,?,?,?) ORDER BY submitted_at DESC, row_no DESC LIMIT ?`,
		jobID, ms(t), MsgSent, MsgDelivered, MsgFailed, MsgExpired, limit)
}

// GetMessage reloads one row.
func (s *Store) GetMessage(jobID string, rowNo int) (*Message, error) {
	ms, err := s.queryMessages(`job_id=? AND row_no=?`, jobID, rowNo)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("job %s has no row %d", jobID, rowNo)
	}
	return ms[0], nil
}

// BeginSubmit marks a row as submitting and appends its ledger entry in one
// transaction, before the POST. A failed POST still counts toward the limits.
func (s *Store) BeginSubmit(m *Message, sim int, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE messages SET state=?, error='', attempts=attempts+1, submitted_at=?, checked_at=0 WHERE job_id=? AND row_no=?`,
		MsgSubmitting, ms(now), m.JobID, m.RowNo); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO ledger (msg_id, sim, segments, submitted_at) VALUES (?,?,?,?)`, m.MsgID, sim, m.Segments, ms(now)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.State, m.Error, m.SubmittedAt = MsgSubmitting, "", now
	m.Attempts++
	return nil
}

// UpdateState records a row's new state. errMsg replaces the stored error
// when non-empty.
func (s *Store) UpdateState(m *Message, state, errMsg string, now time.Time) error {
	resolved := m.ResolvedAt
	if IsResolved(state) && resolved.IsZero() {
		resolved = now
	}
	if errMsg == "" {
		errMsg = m.Error
	}
	_, err := s.db.Exec(`UPDATE messages SET state=?, error=?, resolved_at=?, checked_at=? WHERE job_id=? AND row_no=?`,
		state, errMsg, ms(resolved), ms(now), m.JobID, m.RowNo)
	if err == nil {
		m.State, m.Error, m.ResolvedAt, m.CheckedAt = state, errMsg, resolved, now
	}
	return err
}

// Touch records that a row's status was checked without a change.
func (s *Store) Touch(m *Message, now time.Time) error {
	_, err := s.db.Exec(`UPDATE messages SET checked_at=? WHERE job_id=? AND row_no=?`, ms(now), m.JobID, m.RowNo)
	if err == nil {
		m.CheckedAt = now
	}
	return err
}

// Counts returns the number of rows per state for a job.
func (s *Store) Counts(jobID string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT state, count(*) FROM messages WHERE job_id=? GROUP BY state`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// LedgerEntry is one submission.
type LedgerEntry struct {
	MsgID       string
	Segments    int
	SubmittedAt time.Time
}

// LedgerSince returns submissions after t, oldest first.
func (s *Store) LedgerSince(t time.Time) ([]LedgerEntry, error) {
	rows, err := s.db.Query(`SELECT msg_id, segments, submitted_at FROM ledger WHERE submitted_at > ? ORDER BY submitted_at`, ms(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var at int64
		if err := rows.Scan(&e.MsgID, &e.Segments, &at); err != nil {
			return nil, err
		}
		e.SubmittedAt = fromMS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SegmentsSince sums ledger segments submitted at or after t.
func (s *Store) SegmentsSince(t time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT coalesce(sum(segments),0) FROM ledger WHERE submitted_at >= ?`, ms(t)).Scan(&n)
	return n, err
}

// LastLedger is the time of the most recent submission from any job or send.
func (s *Store) LastLedger() (time.Time, error) {
	var at sql.NullInt64
	err := s.db.QueryRow(`SELECT max(submitted_at) FROM ledger`).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return fromMS(at.Int64), nil
}
