package msg

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	ok := map[string]string{
		"0300 1234567":     "+923001234567",
		"0300-1234567":     "+923001234567",
		"(0300) 123 4567":  "+923001234567",
		"+923211234567":    "+923211234567",
		"923211234567":     "+923211234567",
		"00923211234567":   "+923211234567",
		"3001234567":       "+923001234567",
		" +92 321 1234567": "+923211234567",
	}
	for in, want := range ok {
		got, err := Normalize(in, "92")
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "abc", "0300123", "04235761234", "+447911123456", "030012345678"}
	for _, in := range bad {
		if got, err := Normalize(in, "92"); err == nil {
			t.Errorf("Normalize(%q) = %q, want error", in, got)
		}
	}
}

func TestMask(t *testing.T) {
	if got := Mask("+923001234567"); got != "+92300****567" {
		t.Errorf("Mask = %q", got)
	}
}

func TestAnalyze(t *testing.T) {
	cases := []struct {
		text     string
		enc      string
		units    int
		segments int
	}{
		{"hello", "GSM-7", 5, 1},
		{strings.Repeat("a", 160), "GSM-7", 160, 1},
		{strings.Repeat("a", 161), "GSM-7", 161, 2},
		{strings.Repeat("a", 159) + "{", "GSM-7", 161, 2},
		{strings.Repeat("a", 306), "GSM-7", 306, 2},
		{strings.Repeat("a", 307), "GSM-7", 307, 3},
		{"€", "GSM-7", 2, 1},
		{"it’s", "UCS-2", 4, 1},
		{strings.Repeat("ب", 70), "UCS-2", 70, 1},
		{strings.Repeat("ب", 71), "UCS-2", 71, 2},
		{"😀", "UCS-2", 2, 1},
	}
	for _, c := range cases {
		e := Analyze(c.text)
		if e.Name != c.enc || e.Units != c.units || e.Segments != c.segments {
			t.Errorf("Analyze(%.20q) = %s/%d/%d; want %s/%d/%d", c.text, e.Name, e.Units, e.Segments, c.enc, c.units, c.segments)
		}
	}
	// An extension char straddling the 153 boundary moves to the next part.
	e := Analyze(strings.Repeat("a", 152) + "€" + strings.Repeat("a", 152))
	if e.Segments != 3 {
		t.Errorf("straddling escape: %d segments, want 3", e.Segments)
	}
	if e := Analyze("it’s"); e.Trigger != '’' || e.TriggerAt != 3 {
		t.Errorf("trigger %q at %d", e.Trigger, e.TriggerAt)
	}
}
