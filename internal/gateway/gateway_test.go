package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendRequestShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "sms" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/message" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"id":"j7k2p-0042","state":"Pending","recipients":[{"phoneNumber":"+923001234567","state":"Pending"}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "sms", "pw", 2*time.Second)
	st, err := c.Send(context.Background(), OutMessage{ID: "j7k2p-0042", Phone: "+923001234567", Text: "Salam", SIM: 1, TTL: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != Pending || st.ID != "j7k2p-0042" {
		t.Fatalf("status %+v", st)
	}
	want := map[string]any{
		"id":                 "j7k2p-0042",
		"textMessage":        map[string]any{"text": "Salam"},
		"phoneNumbers":       []any{"+923001234567"},
		"simNumber":          float64(1),
		"ttl":                float64(600),
		"withDeliveryReport": true,
		"priority":           float64(0),
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("body\n got %s\nwant %s", gotJSON, wantJSON)
	}

	bad := New(srv.URL, "sms", "wrong", 2*time.Second)
	if _, err := bad.Send(context.Background(), OutMessage{ID: "x"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestErrorMapping(t *testing.T) {
	code, body := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	c := New(srv.URL, "u", "p", 2*time.Second)
	ctx := context.Background()

	code, body = http.StatusConflict, ""
	if _, err := c.Send(ctx, OutMessage{ID: "a"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("409: %v", err)
	}
	code, body = http.StatusBadRequest, `{"message":"message with id a already exists"}`
	if _, err := c.Send(ctx, OutMessage{ID: "a"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("400 duplicate: %v", err)
	}
	code, body = http.StatusBadRequest, `{"message":"invalid phone"}`
	var rej *RejectedError
	if _, err := c.Send(ctx, OutMessage{ID: "a"}); !errors.As(err, &rej) {
		t.Errorf("400: %v", err)
	}
	code = http.StatusInternalServerError
	if _, err := c.Send(ctx, OutMessage{ID: "a"}); !IsUnavailable(err) {
		t.Errorf("500: %v", err)
	}
	code = http.StatusNotFound
	if _, err := c.Status(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	code, body = http.StatusOK, `{"id":"a","state":"Processed","recipients":[{"phoneNumber":"+92","state":"Failed","error":"RESULT_ERROR_NO_SERVICE"}]}`
	st, err := c.Status(ctx, "a")
	if err != nil || st.State != Failed || st.Error != "RESULT_ERROR_NO_SERVICE" {
		t.Errorf("status: %+v %v", st, err)
	}

	srv.Close()
	if err := c.Health(ctx); !IsUnavailable(err) {
		t.Errorf("closed server: %v", err)
	}
}
