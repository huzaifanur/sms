// Package config holds the sender's settings: compiled-in defaults and
// allowed ranges, overridden by environment variables (SMS_PACE_DELAY_MIN for
// pace.delay_min, and so on).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kind is how a key's value is parsed and validated.
type Kind int

const (
	KindString Kind = iota
	KindURL
	KindInt
	KindDigits
	KindDuration
	KindPercent
	KindClock
	KindZone
)

// Key describes one setting.
type Key struct {
	Name    string
	Kind    Kind
	Default string // "" means no default
	Min     int64  // ints and percents: value; durations: nanoseconds; 0 = no bound
	Max     int64
	Range   string // human-readable allowed range
}

// Keys is every setting, in display order.
var Keys = []Key{
	{Name: "gateway.url", Kind: KindURL, Range: "http(s) URL"},
	{Name: "gateway.username", Kind: KindString, Range: "text"},
	{Name: "gateway.password", Kind: KindString, Range: "text"},
	{Name: "gateway.timeout", Kind: KindDuration, Default: "10s", Min: int64(2 * time.Second), Max: int64(60 * time.Second), Range: "2s-60s"},
	{Name: "sim", Kind: KindInt, Min: 1, Max: 2, Range: "1 or 2"},
	{Name: "country_code", Kind: KindDigits, Default: "92", Range: "digits"},
	{Name: "pace.delay_min", Kind: KindDuration, Default: "6s", Min: int64(3 * time.Second), Range: "3s and up"},
	{Name: "pace.delay_max", Kind: KindDuration, Default: "9s", Min: int64(3 * time.Second), Range: ">= pace.delay_min"},
	{Name: "limit.per_15m", Kind: KindInt, Default: "150", Min: 1, Max: 180, Range: "1-180"},
	{Name: "limit.per_day", Kind: KindInt, Default: "450", Min: 1, Max: 500, Range: "1-500"},
	{Name: "hours.start", Kind: KindClock, Default: "10:00", Range: "HH:MM, before hours.end"},
	{Name: "hours.end", Kind: KindClock, Default: "20:00", Range: "HH:MM or 24:00, after hours.start"},
	{Name: "hours.tz", Kind: KindZone, Default: "Asia/Karachi", Range: "IANA zone"},
	{Name: "warmup.count", Kind: KindInt, Default: "15", Min: 5, Max: 50, Range: "5-50"},
	{Name: "warmup.delay", Kind: KindDuration, Default: "15s", Range: ">= pace.delay_max"},
	{Name: "warmup.gate_timeout", Kind: KindDuration, Default: "3m", Min: int64(time.Minute), Max: int64(10 * time.Minute), Range: "1m-10m"},
	{Name: "breaker.consecutive", Kind: KindInt, Default: "3", Min: 1, Max: 10, Range: "1-10"},
	{Name: "breaker.rate", Kind: KindPercent, Default: "20%", Min: 5, Max: 50, Range: "5%-50%"},
	{Name: "breaker.stuck_after", Kind: KindDuration, Default: "60s", Min: int64(30 * time.Second), Max: int64(300 * time.Second), Range: "30s-300s"},
	{Name: "message.max_segments", Kind: KindInt, Default: "1", Min: 1, Max: 3, Range: "1-3"},
	{Name: "message.ttl", Kind: KindDuration, Default: "10m", Min: int64(time.Minute), Max: int64(60 * time.Minute), Range: "1m-60m"},
	{Name: "poll.interval", Kind: KindDuration, Default: "5s", Min: int64(2 * time.Second), Max: int64(30 * time.Second), Range: "2s-30s"},
}

// Env is the environment variable that sets k: hours.start is SMS_HOURS_START.
func (k Key) Env() string {
	return "SMS_" + strings.ToUpper(strings.NewReplacer(".", "_").Replace(k.Name))
}

// Normalize parses raw for key k, checks its own range and returns the
// canonical string form. Cross-key rules are checked by Settings.
func (k Key) Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%s: empty value", k.Name)
	}
	bad := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("%s (%s): %s (allowed: %s)", k.Name, k.Env(), fmt.Sprintf(format, a...), k.Range)
	}
	switch k.Kind {
	case KindString:
		return raw, nil
	case KindURL:
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return bad("%q is not an http(s) URL", raw)
		}
		return strings.TrimRight(raw, "/"), nil
	case KindInt, KindPercent:
		s := raw
		if k.Kind == KindPercent {
			s = strings.TrimSuffix(s, "%")
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return bad("%q is not a whole number", raw)
		}
		if (k.Min != 0 && n < k.Min) || (k.Max != 0 && n > k.Max) {
			return bad("%d is out of range", n)
		}
		if k.Kind == KindPercent {
			return fmt.Sprintf("%d%%", n), nil
		}
		return strconv.FormatInt(n, 10), nil
	case KindDigits:
		s := strings.TrimPrefix(raw, "+")
		if s == "" || strings.Trim(s, "0123456789") != "" || s[0] == '0' || len(s) > 3 {
			return bad("%q is not a country calling code", raw)
		}
		return s, nil
	case KindDuration:
		d, err := ParseDuration(raw)
		if err != nil {
			return bad("%v", err)
		}
		if (k.Min != 0 && int64(d) < k.Min) || (k.Max != 0 && int64(d) > k.Max) {
			return bad("%s is out of range", FormatDuration(d))
		}
		return FormatDuration(d), nil
	case KindClock:
		m, err := parseClock(raw)
		if err != nil {
			return bad("%v", err)
		}
		return fmt.Sprintf("%02d:%02d", m/60, m%60), nil
	case KindZone:
		if _, err := time.LoadLocation(raw); err != nil {
			return bad("unknown time zone %q", raw)
		}
		return raw, nil
	}
	return raw, nil
}

// ParseDuration accepts Go durations ("6s", "3m", "1m30s") or a bare number
// of seconds ("6").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration like 6s or 3m", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return d, nil
}

// FormatDuration prints a duration compactly: 6s, 3m, 1m30s, 1h.
func FormatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		if s == "24:00" { // end of day, so 00:00-24:00 means always open
			return 24 * 60, nil
		}
		return 0, fmt.Errorf("%q is not a time like 10:00", s)
	}
	return hh*60 + mm, nil
}

// Load resolves every key from getenv (normally os.Getenv), falling back to
// the defaults, and checks the cross-key rules.
func Load(getenv func(string) string) (Settings, error) {
	vals := map[string]string{}
	var errs []error
	for _, k := range Keys {
		v := k.Default
		if raw := getenv(k.Env()); raw != "" {
			norm, err := k.Normalize(raw)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			v = norm
		}
		vals[k.Name] = v
	}
	if len(errs) > 0 {
		return Settings{}, errors.Join(errs...)
	}
	num := func(name string) int {
		n, _ := strconv.Atoi(strings.TrimSuffix(vals[name], "%"))
		return n
	}
	dur := func(name string) time.Duration {
		d, _ := ParseDuration(vals[name])
		return d
	}
	clock := func(name string) int {
		m, _ := parseClock(vals[name])
		return m
	}
	s := Settings{
		GatewayURL:     vals["gateway.url"],
		Username:       vals["gateway.username"],
		Password:       vals["gateway.password"],
		GatewayTimeout: dur("gateway.timeout"),
		SIM:            num("sim"),
		CountryCode:    vals["country_code"],
		DelayMin:       dur("pace.delay_min"),
		DelayMax:       dur("pace.delay_max"),
		Per15m:         num("limit.per_15m"),
		PerDay:         num("limit.per_day"),
		HoursStart:     clock("hours.start"),
		HoursEnd:       clock("hours.end"),
		WarmupCount:    num("warmup.count"),
		WarmupDelay:    dur("warmup.delay"),
		WarmupGate:     dur("warmup.gate_timeout"),
		BreakerConsec:  num("breaker.consecutive"),
		BreakerRate:    num("breaker.rate"),
		StuckAfter:     dur("breaker.stuck_after"),
		MaxSegments:    num("message.max_segments"),
		TTL:            dur("message.ttl"),
		PollInterval:   dur("poll.interval"),
	}
	s.TZ, _ = time.LoadLocation(vals["hours.tz"])

	if s.DelayMax < s.DelayMin {
		return s, fmt.Errorf("pace.delay_max (%s) must be >= pace.delay_min (%s)", FormatDuration(s.DelayMax), FormatDuration(s.DelayMin))
	}
	if s.WarmupDelay < s.DelayMax {
		return s, fmt.Errorf("warmup.delay (%s) must be >= pace.delay_max (%s)", FormatDuration(s.WarmupDelay), FormatDuration(s.DelayMax))
	}
	if s.HoursStart >= s.HoursEnd {
		return s, fmt.Errorf("hours.start (%s) must be before hours.end (%s); overnight windows are not allowed", vals["hours.start"], vals["hours.end"])
	}
	return s, nil
}

// Settings is the resolved, typed configuration.
type Settings struct {
	GatewayURL     string
	Username       string
	Password       string
	GatewayTimeout time.Duration
	SIM            int // 0 when unset
	CountryCode    string
	DelayMin       time.Duration
	DelayMax       time.Duration
	Per15m         int
	PerDay         int
	HoursStart     int // minutes after midnight
	HoursEnd       int
	TZ             *time.Location
	WarmupCount    int
	WarmupDelay    time.Duration
	WarmupGate     time.Duration
	BreakerConsec  int
	BreakerRate    int // percent
	StuckAfter     time.Duration
	MaxSegments    int
	TTL            time.Duration
	PollInterval   time.Duration
}

// RequireSending reports the settings that must be set before anything is sent.
func (s Settings) RequireSending() error {
	var missing []string
	if s.GatewayURL == "" {
		missing = append(missing, "SMS_GATEWAY_URL")
	}
	if s.SIM == 0 {
		missing = append(missing, "SMS_SIM")
	}
	if len(missing) > 0 {
		return fmt.Errorf("not configured: %s", strings.Join(missing, ", "))
	}
	return nil
}

// HoursLabel renders the sending window, e.g. "10:00-20:00 Asia/Karachi".
func (s Settings) HoursLabel() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d %s", s.HoursStart/60, s.HoursStart%60, s.HoursEnd/60, s.HoursEnd%60, s.TZ)
}

// InHours reports whether t falls inside the sending window.
func (s Settings) InHours(t time.Time) bool {
	t = t.In(s.TZ)
	m := t.Hour()*60 + t.Minute()
	return m >= s.HoursStart && m < s.HoursEnd
}

// NextStart is the next time the sending window opens at or after t.
func (s Settings) NextStart(t time.Time) time.Time {
	t = t.In(s.TZ)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, s.TZ)
	start := day.Add(time.Duration(s.HoursStart) * time.Minute)
	if !t.Before(start) {
		start = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, s.TZ).Add(time.Duration(s.HoursStart) * time.Minute)
	}
	return start
}

// DayStart is midnight of t's calendar day in hours.tz.
func (s Settings) DayStart(t time.Time) time.Time {
	t = t.In(s.TZ)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, s.TZ)
}
