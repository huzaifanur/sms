package api

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"sms/internal/engine"
	"sms/internal/store"
)

// RetryPaused is how long a paused job waits before the worker resumes it.
const RetryPaused = 2 * time.Minute

// idle is the longest the worker sleeps between looks at the queue.
const idle = time.Minute

// ErrFinished means the job is already done or cancelled.
var ErrFinished = errors.New("job already finished")

// Worker runs jobs one at a time, so every send passes the engine's gates
// in order and two jobs can never double the rate.
type Worker struct {
	E *engine.Engine

	wake chan struct{}

	mu         sync.Mutex
	running    string // id of the job in RunJob, "" when idle
	stopRun    context.CancelFunc
	cancelling bool
}

// NewWorker returns a worker that sends through e.
func NewWorker(e *engine.Engine) *Worker {
	return &Worker{E: e, wake: make(chan struct{}, 1)}
}

// Wake tells the worker a job was queued.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes jobs until ctx is cancelled. A job interrupted by shutdown
// is left paused and resumes on the next start.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		ran, err := w.Step(ctx)
		if err != nil {
			log.Printf("worker: %v", err)
		}
		if ran && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-time.After(idle):
		}
	}
}

// Step runs the next due job to its end, if there is one.
func (w *Worker) Step(ctx context.Context) (ran bool, err error) {
	job, rctx, err := w.claim(ctx)
	if job == nil || err != nil {
		return false, err
	}
	o, err := w.E.RunJob(rctx, job.ID)

	w.mu.Lock()
	cancelled := w.cancelling
	w.stopRun()
	w.running, w.stopRun, w.cancelling = "", nil, false
	w.mu.Unlock()

	if cancelled {
		return true, w.E.Store.Cancel(job.ID, w.E.Clock.Now())
	}
	if err != nil {
		return true, err
	}
	if o.State != store.JobDone {
		log.Printf("job %s %s: %s", o.JobID, o.State, o.Reason)
	}
	return true, nil
}

// claim picks the next job and marks it running under the lock, so a cancel
// either finds it still waiting or finds it running.
func (w *Worker) claim(ctx context.Context) (*store.Job, context.Context, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.E.Clock.Now()
	blocked, err := w.E.Blocked(1)
	if err != nil {
		return nil, nil, err
	}
	job, err := w.E.Store.NextJob(blocked == "", now.Add(-RetryPaused))
	if job == nil || err != nil {
		return nil, nil, err
	}
	rctx, stop := context.WithCancel(ctx)
	w.running, w.stopRun = job.ID, stop
	return job, rctx, nil
}

// Cancel stops a job: it drops its unsent messages, and if it is running,
// lets the current message finish first.
func (w *Worker) Cancel(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running == id {
		w.cancelling = true
		w.stopRun()
		return nil
	}
	job, err := w.E.Store.GetJob(id)
	if err != nil {
		return err
	}
	if job.State == store.JobDone || job.State == store.JobCancelled {
		return ErrFinished
	}
	return w.E.Store.Cancel(id, w.E.Clock.Now())
}

// Resume queues a paused or stopped job to run again now.
func (w *Worker) Resume(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	job, err := w.E.Store.GetJob(id)
	if err != nil {
		return err
	}
	switch job.State {
	case store.JobDone, store.JobCancelled:
		return ErrFinished
	case store.JobPaused, store.JobStopped:
		if err := w.E.Store.SetJobState(id, store.JobQueued, "", w.E.Clock.Now()); err != nil {
			return err
		}
	}
	w.Wake()
	return nil
}

// Running is the id of the job being sent, or "".
func (w *Worker) Running() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}
