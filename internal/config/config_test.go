package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	s, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.Per15m != 150 || s.PerDay != 450 || s.DelayMin != 6*time.Second || s.DelayMax != 9*time.Second ||
		s.WarmupCount != 15 || s.BreakerRate != 20 || s.TTL != 10*time.Minute || s.SIM != 0 || s.CountryCode != "92" {
		t.Fatalf("unexpected defaults %+v", s)
	}
	if err := s.RequireSending(); err == nil {
		t.Fatal("sending must be refused until gateway and sim are set")
	}
}

func TestEnvOverrides(t *testing.T) {
	s, err := Load(env(map[string]string{
		"SMS_GATEWAY_URL":          "https://api.sms-gate.app/3rdparty/v1/",
		"SMS_SIM":                  "2",
		"SMS_PACE_DELAY_MIN":       "7",
		"SMS_BREAKER_RATE":         "10%",
		"SMS_MESSAGE_MAX_SEGMENTS": "3",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.GatewayURL != "https://api.sms-gate.app/3rdparty/v1" || s.SIM != 2 || s.DelayMin != 7*time.Second || s.BreakerRate != 10 || s.MaxSegments != 3 {
		t.Fatalf("overrides not applied: %+v", s)
	}
	if err := s.RequireSending(); err != nil {
		t.Fatal(err)
	}
}

func TestRangesEnforced(t *testing.T) {
	bad := map[string]string{
		"SMS_SIM":             "3",
		"SMS_LIMIT_PER_15M":   "181",
		"SMS_LIMIT_PER_DAY":   "501",
		"SMS_PACE_DELAY_MIN":  "2s",
		"SMS_GATEWAY_URL":     "ftp://x",
		"SMS_HOURS_START":     "25:00",
		"SMS_HOURS_TZ":        "Mars/Base",
		"SMS_BREAKER_RATE":    "60%",
		"SMS_COUNTRY_CODE":    "0092",
		"SMS_GATEWAY_TIMEOUT": "1s",
	}
	for k, v := range bad {
		if _, err := Load(env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: got %v, want an error naming the variable", k, v, err)
		}
	}
	cross := []map[string]string{
		{"SMS_PACE_DELAY_MIN": "10s"},                          // above delay_max
		{"SMS_WARMUP_DELAY": "8s"},                             // below delay_max
		{"SMS_HOURS_START": "21:00"},                           // after hours.end
		{"SMS_HOURS_START": "24:00", "SMS_HOURS_END": "24:00"}, // start can't be end of day
	}
	for _, m := range cross {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%v accepted", m)
		}
	}
}

func TestHours(t *testing.T) {
	s, _ := Load(env(nil))
	at := func(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, s.TZ) }
	if s.InHours(at(9, 59)) || !s.InHours(at(10, 0)) || !s.InHours(at(19, 59)) || s.InHours(at(20, 0)) {
		t.Fatal("hours window wrong")
	}
	if got := s.NextStart(at(21, 0)); !got.Equal(time.Date(2026, 10, 6, 10, 0, 0, 0, s.TZ)) {
		t.Fatalf("next start %s", got)
	}
	if got := s.NextStart(at(8, 0)); !got.Equal(at(10, 0)) {
		t.Fatalf("next start %s", got)
	}
}

func TestAlwaysOpen(t *testing.T) {
	s, err := Load(env(map[string]string{"SMS_HOURS_START": "00:00", "SMS_HOURS_END": "24:00"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, hm := range [][2]int{{0, 0}, {3, 30}, {23, 59}} {
		if !s.InHours(time.Date(2026, 10, 5, hm[0], hm[1], 0, 0, s.TZ)) {
			t.Fatalf("%02d:%02d outside an always-open window", hm[0], hm[1])
		}
	}
}
