package service

import (
	"regexp"
	"strings"
)

// NormalizePhone reduces a phone number to WhatsApp's wa_id form: digits only,
// country code included, no separators.
//
// Kazakh numbers are written in many ways — "8 701 555 12 34", "+7 (701)
// 555-12-34", "7015551234" — and all of them must resolve to 77015551234 so a
// contact is never duplicated.
func NormalizePhone(raw string) (string, bool) {
	digits := digitsOnly(raw)
	if digits == "" {
		return "", false
	}

	switch {
	// Local Kazakh/Russian form: 8XXXXXXXXXX -> 7XXXXXXXXXX.
	case len(digits) == 11 && strings.HasPrefix(digits, "8"):
		digits = "7" + digits[1:]
	// Missing country code: 7015551234 -> 77015551234.
	case len(digits) == 10 && (strings.HasPrefix(digits, "7") || strings.HasPrefix(digits, "6")):
		digits = "7" + digits
	}

	// E.164 allows 8..15 digits including the country code.
	if len(digits) < 8 || len(digits) > 15 {
		return "", false
	}
	return digits, true
}

// FormatE164 renders a normalised number for display.
func FormatE164(digits string) string {
	if digits == "" {
		return ""
	}
	if strings.HasPrefix(digits, "+") {
		return digits
	}
	return "+" + digits
}

// phonePattern finds a candidate phone number inside free-form text.
var phonePattern = regexp.MustCompile(`\+?\d[\d\s\-()]{6,20}\d`)

// ExtractPhone pulls the first plausible phone number out of a message, so a
// user who simply types their number is understood without a form.
func ExtractPhone(text string) (string, bool) {
	for _, candidate := range phonePattern.FindAllString(text, -1) {
		if phone, ok := NormalizePhone(candidate); ok {
			return phone, true
		}
	}
	return "", false
}

func digitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
