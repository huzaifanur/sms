// Package gateway talks to SMSGate (SMS Gateway for Android) with HTTP Basic
// Auth, either to the phone's Local Server or through the cloud relay
// (https://api.sms-gate.app/3rdparty/v1); both serve the same three calls
// used here: send, status and health.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Endpoints, relative to gateway.url.
const (
	PathSend   = "/message"
	PathStatus = "/message/" // + id
	PathHealth = "/health"
)

// State is SMSGate's message state.
type State string

const (
	Pending   State = "Pending"
	Processed State = "Processed"
	Sent      State = "Sent"
	Delivered State = "Delivered"
	Failed    State = "Failed"
)

// OutMessage is one SMS to one recipient.
type OutMessage struct {
	ID    string
	Phone string
	Text  string
	SIM   int
	TTL   time.Duration
}

// Status is the gateway's view of a message.
type Status struct {
	ID    string
	State State
	Error string
}

// Gateway is everything the engine needs from the phone.
type Gateway interface {
	Send(ctx context.Context, m OutMessage) (Status, error)
	Status(ctx context.Context, id string) (Status, error)
	Health(ctx context.Context) error
}

var (
	// ErrUnauthorized is HTTP 401: stop at once.
	ErrUnauthorized = errors.New("SMSGate refused the request (HTTP 401): set SMS_GATEWAY_USERNAME and SMS_GATEWAY_PASSWORD to the credentials shown in the app")
	// ErrNotFound means the gateway has no message with that id.
	ErrNotFound = errors.New("message not found on the gateway")
	// ErrDuplicate means the id was already submitted.
	ErrDuplicate = errors.New("gateway already has a message with this id")
)

// RejectedError is a 4xx the gateway gave for this payload.
type RejectedError struct {
	Code int
	Body string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("gateway rejected the message (HTTP %d): %s", e.Code, e.Body)
}

// UnavailableError means the phone or app could not be reached or failed
// (timeout, connection refused, 5xx). The outcome of a send is unknown.
type UnavailableError struct {
	Err error
}

func (e *UnavailableError) Error() string { return "gateway unreachable: " + e.Err.Error() }
func (e *UnavailableError) Unwrap() error { return e.Err }

// IsUnavailable reports whether err is a reachability problem.
func IsUnavailable(err error) bool {
	var u *UnavailableError
	return errors.As(err, &u)
}

// Client is the HTTP implementation of Gateway.
type Client struct {
	base       string
	user, pass string
	http       *http.Client
}

// New builds a client for baseURL, e.g. http://192.168.1.50:8080 or
// https://api.sms-gate.app/3rdparty/v1.
func New(baseURL, user, pass string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		user: user,
		pass: pass,
		http: &http.Client{Timeout: timeout},
	}
}

type sendBody struct {
	ID                 string   `json:"id"`
	TextMessage        textBody `json:"textMessage"`
	PhoneNumbers       []string `json:"phoneNumbers"`
	SimNumber          int      `json:"simNumber"`
	TTL                int64    `json:"ttl"`
	WithDeliveryReport bool     `json:"withDeliveryReport"`
	Priority           int      `json:"priority"`
}

type textBody struct {
	Text string `json:"text"`
}

type stateBody struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Recipients []struct {
		PhoneNumber string  `json:"phoneNumber"`
		State       string  `json:"state"`
		Error       *string `json:"error"`
	} `json:"recipients"`
}

func (b stateBody) status() Status {
	st := Status{ID: b.ID, State: State(b.State)}
	if len(b.Recipients) > 0 {
		r := b.Recipients[0]
		if r.State != "" {
			st.State = State(r.State)
		}
		if r.Error != nil {
			st.Error = *r.Error
		}
	}
	return st
}

// Send submits one message: exactly one recipient, priority 0 (so the
// app's own delays, limits and working hours still apply) and a TTL so a
// stalled phone never sends it late.
func (c *Client) Send(ctx context.Context, m OutMessage) (Status, error) {
	body, _ := json.Marshal(sendBody{
		ID:                 m.ID,
		TextMessage:        textBody{Text: m.Text},
		PhoneNumbers:       []string{m.Phone},
		SimNumber:          m.SIM,
		TTL:                int64(m.TTL / time.Second),
		WithDeliveryReport: true,
		Priority:           0,
	})
	code, resp, err := c.do(ctx, http.MethodPost, PathSend, body)
	if err != nil {
		return Status{}, err
	}
	switch {
	case code >= 200 && code < 300:
		var b stateBody
		if err := json.Unmarshal(resp, &b); err != nil || b.State == "" && len(b.Recipients) == 0 {
			return Status{ID: m.ID, State: Pending}, nil
		}
		st := b.status()
		if st.ID == "" {
			st.ID = m.ID
		}
		return st, nil
	case code == http.StatusConflict:
		return Status{}, ErrDuplicate
	case code == http.StatusBadRequest && looksDuplicate(resp):
		return Status{}, ErrDuplicate
	}
	return Status{}, classify(code, resp)
}

func looksDuplicate(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "exist") || strings.Contains(s, "duplicate") || strings.Contains(s, "unique")
}

// Status fetches a message's state by id.
func (c *Client) Status(ctx context.Context, id string) (Status, error) {
	code, resp, err := c.do(ctx, http.MethodGet, PathStatus+url.PathEscape(id), nil)
	if err != nil {
		return Status{}, err
	}
	if code == http.StatusOK {
		var b stateBody
		if err := json.Unmarshal(resp, &b); err != nil {
			return Status{}, &UnavailableError{Err: fmt.Errorf("unreadable status response: %w", err)}
		}
		st := b.status()
		if st.ID == "" {
			st.ID = id
		}
		return st, nil
	}
	if code == http.StatusNotFound {
		return Status{}, ErrNotFound
	}
	return Status{}, classify(code, resp)
}

// Health checks the app answers.
func (c *Client) Health(ctx context.Context) error {
	code, resp, err := c.do(ctx, http.MethodGet, PathHealth, nil)
	if err != nil {
		return err
	}
	if code >= 200 && code < 300 {
		return nil
	}
	return classify(code, resp)
}

func classify(code int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	switch {
	case code == http.StatusUnauthorized:
		return ErrUnauthorized
	case code == http.StatusTooManyRequests || code >= 500:
		return &UnavailableError{Err: fmt.Errorf("HTTP %d: %s", code, msg)}
	default:
		return &RejectedError{Code: code, Body: msg}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, &UnavailableError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, &UnavailableError{Err: err}
	}
	return resp.StatusCode, data, nil
}
