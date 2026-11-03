package gateway

import (
	"context"
	"sync"
)

// Fake is a scriptable in-memory Gateway for tests.
type Fake struct {
	mu sync.Mutex

	// Outcome gives the states a message steps through on successive Status
	// calls; the last one sticks. Default: Sent, then Delivered.
	Outcome func(m OutMessage) []Status
	// SendErrs are returned by successive Send calls before any is accepted;
	// a nil entry accepts that call.
	SendErrs []error
	// StatusErr, when set, is returned by every Status call.
	StatusErr error
	// HealthErr is returned by Health.
	HealthErr error
	// AcceptOnError records the message even when a SendErrs entry fails the
	// call, like a POST that reached the phone but whose response was lost.
	AcceptOnError bool

	Accepted  []OutMessage
	SendCalls int
	msgs      map[string]*fakeMsg
}

type fakeMsg struct {
	out    OutMessage
	states []Status
	polls  int
}

func (f *Fake) Send(_ context.Context, m OutMessage) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.SendCalls++
	if f.msgs == nil {
		f.msgs = map[string]*fakeMsg{}
	}
	if _, dup := f.msgs[m.ID]; dup {
		return Status{}, ErrDuplicate
	}
	var err error
	if len(f.SendErrs) > 0 {
		err, f.SendErrs = f.SendErrs[0], f.SendErrs[1:]
	}
	if err != nil && !f.AcceptOnError {
		return Status{}, err
	}
	states := []Status{{State: Sent}, {State: Delivered}}
	if f.Outcome != nil {
		states = f.Outcome(m)
	}
	f.msgs[m.ID] = &fakeMsg{out: m, states: states}
	f.Accepted = append(f.Accepted, m)
	if err != nil {
		return Status{}, err
	}
	return Status{ID: m.ID, State: Pending}, nil
}

func (f *Fake) Status(_ context.Context, id string) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StatusErr != nil {
		return Status{}, f.StatusErr
	}
	m, ok := f.msgs[id]
	if !ok {
		return Status{}, ErrNotFound
	}
	i := min(m.polls, len(m.states)-1)
	m.polls++
	st := m.states[i]
	st.ID = id
	return st, nil
}

func (f *Fake) Health(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.HealthErr
}
