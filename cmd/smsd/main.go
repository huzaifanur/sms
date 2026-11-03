// Command smsd is an HTTP API for sending SMS through one chosen SIM on your
// own Android phone running SMSGate, paced well under PTA and carrier limits.
//
// Settings come from the environment: SMS_AUTH_USER and SMS_AUTH_PASSWORD
// (required, HTTP Basic auth), SMS_LISTEN (default 127.0.0.1:8097),
// SMS_DATA_DIR (default ./data), and the engine settings in package config
// (SMS_GATEWAY_URL, SMS_SIM, ...).
package main

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sms/internal/api"
	"sms/internal/config"
	"sms/internal/engine"
	"sms/internal/gateway"
	"sms/internal/store"
)

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Fatal("smsd: ", err)
	}
}

func run() error {
	s, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if err := s.RequireSending(); err != nil {
		return err
	}
	user, pass := os.Getenv("SMS_AUTH_USER"), os.Getenv("SMS_AUTH_PASSWORD")
	if user == "" || len(pass) < 12 {
		return errors.New("SMS_AUTH_USER must be set and SMS_AUTH_PASSWORD must be at least 12 characters")
	}
	listen := envOr("SMS_LISTEN", "127.0.0.1:8097")
	st, err := store.Open(envOr("SMS_DATA_DIR", "data"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Recover(time.Now()); err != nil {
		return err
	}

	e := &engine.Engine{
		Store:    st,
		GW:       gateway.New(s.GatewayURL, s.Username, s.Password, s.GatewayTimeout),
		S:        s,
		Clock:    engine.RealClock{},
		Rand:     rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
		Progress: os.Stderr,
	}
	w := api.NewWorker(e)
	srv := &http.Server{
		Addr:              listen,
		Handler:           (&api.Server{W: w, User: user, Password: pass}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	worked := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(worked)
	}()
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		stop()
		<-worked
		return err
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Printf("smsd listening on %s, SIM %d via %s", listen, s.SIM, s.GatewayURL)

	select {
	case err = <-served:
		stop()
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = srv.Shutdown(shut)
	}
	// The worker lets the current message finish and checkpoints the job.
	<-worked
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
