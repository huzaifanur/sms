package msg

import (
	"fmt"
	"unicode/utf16"
)

const (
	gsmBasic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
		"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	gsmExtension = "\f^{}\\[~]|€"
)

var gsmWidth = func() map[rune]int {
	m := map[rune]int{}
	for _, r := range gsmBasic {
		m[r] = 1
	}
	for _, r := range gsmExtension {
		m[r] = 2
	}
	return m
}()

// Encoding describes how a text goes over the air.
type Encoding struct {
	Name      string // "GSM-7" or "UCS-2"
	Units     int    // septets for GSM-7, UTF-16 code units for UCS-2
	Segments  int
	Trigger   rune // first character that forced UCS-2; 0 for GSM-7
	TriggerAt int  // 1-based character position of Trigger
}

// Analyze picks the encoding for text and counts its carrier segments.
//
// GSM-7 fits 160 septets in one SMS and 153 per part when split; extension
// characters take two septets and are never split across parts. Anything
// outside GSM-7 switches the whole message to UCS-2: 70 units, 67 per part.
func Analyze(text string) Encoding {
	var widths []int
	enc := Encoding{Name: "GSM-7"}
	pos := 0
	for _, r := range text {
		pos++
		w, ok := gsmWidth[r]
		if !ok {
			enc.Name, enc.Trigger, enc.TriggerAt = "UCS-2", r, pos
			break
		}
		widths = append(widths, w)
	}
	single, part := 160, 153
	if enc.Name == "UCS-2" {
		single, part = 70, 67
		widths = widths[:0]
		for _, r := range text {
			widths = append(widths, len(utf16.Encode([]rune{r})))
		}
	}
	for _, w := range widths {
		enc.Units += w
	}
	switch {
	case enc.Units == 0:
		enc.Segments = 0
	case enc.Units <= single:
		enc.Segments = 1
	default:
		enc.Segments = 1
		cur := 0
		for _, w := range widths {
			if cur+w > part {
				enc.Segments++
				cur = 0
			}
			cur += w
		}
	}
	return enc
}

// Describe explains the encoding in one phrase for error messages.
func (e Encoding) Describe() string {
	if e.Name == "GSM-7" {
		return fmt.Sprintf("GSM-7, %d chars", e.Units)
	}
	return fmt.Sprintf("UCS-2 because of %s at char %d, %d chars", runeLabel(e.Trigger), e.TriggerAt, e.Units)
}

// runeLabel renders a character with its code point: '’' (U+2019).
func runeLabel(r rune) string {
	return fmt.Sprintf("%q (U+%04X)", r, r)
}
