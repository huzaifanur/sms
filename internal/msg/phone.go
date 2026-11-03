// Package msg checks one SMS before it is queued: the recipient's number and
// the text's encoding and segment count.
package msg

import (
	"errors"
	"fmt"
	"strings"
)

var phoneJunk = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "", "/", "", "\t", "", " ", "")

// Normalize turns a phone number as typed into E.164 (+923001234567).
//
// Local numbers (03001234567), numbers with the country code but no plus
// (923001234567) and the 00 international prefix (00923001234567) are all
// accepted. Numbers outside cc are rejected. For
// Pakistan (92) the national part must be a 10-digit mobile number starting
// with 3, since landlines cannot receive SMS.
func Normalize(raw, cc string) (string, error) {
	s := phoneJunk.Replace(strings.TrimSpace(raw))
	if s == "" {
		return "", errors.New("empty number")
	}
	var digits string
	switch {
	case strings.HasPrefix(s, "+"):
		digits = s[1:]
	case strings.HasPrefix(s, "00"):
		digits = s[2:]
	case strings.HasPrefix(s, "0"):
		digits = cc + s[1:]
	case strings.HasPrefix(s, cc) && len(s) > len(cc)+7:
		digits = s
	default:
		digits = cc + s
	}
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", fmt.Errorf("bad number %q", raw)
	}
	if digits[0] == '0' || len(digits) < 8 || len(digits) > 15 {
		return "", fmt.Errorf("bad number %q (not valid E.164)", raw)
	}
	if !strings.HasPrefix(digits, cc) {
		return "", fmt.Errorf("international number %q (only +%s is allowed)", raw, cc)
	}
	if cc == "92" {
		national := digits[len(cc):]
		if len(national) != 10 || national[0] != '3' {
			return "", fmt.Errorf("bad number %q (not a Pakistani mobile, expected 03XX XXXXXXX)", raw)
		}
	}
	return "+" + digits, nil
}

// Mask hides the middle of a number in logs: +92300****567.
func Mask(phone string) string {
	if len(phone) <= 9 {
		return phone
	}
	return phone[:6] + strings.Repeat("*", len(phone)-9) + phone[len(phone)-3:]
}
