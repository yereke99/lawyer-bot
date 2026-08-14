package service

import "testing"

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"+7 (701) 555-12-34", "77015551234", true},
		{"8 701 555 12 34", "77015551234", true},
		{"87015551234", "77015551234", true},
		{"77015551234", "77015551234", true},
		{"7015551234", "77015551234", true},
		{"+1 415 555 0132", "14155550132", true},
		{"", "", false},
		{"не телефон", "", false},
		{"12345", "", false},
	}

	for _, tc := range cases {
		got, ok := NormalizePhone(tc.in)
		if ok != tc.valid {
			t.Errorf("NormalizePhone(%q) valid = %v, want %v", tc.in, ok, tc.valid)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractPhoneFromMessage(t *testing.T) {
	cases := []struct {
		text string
		want string
		ok   bool
	}{
		{"Мой номер +7 701 555 12 34, звоните", "77015551234", true},
		{"87015551234", "77015551234", true},
		{"Позвоните мне пожалуйста", "", false},
		{"Мне нужно 5 договоров", "", false},
	}

	for _, tc := range cases {
		got, ok := ExtractPhone(tc.text)
		if ok != tc.ok {
			t.Errorf("ExtractPhone(%q) ok = %v, want %v (got %q)", tc.text, ok, tc.ok, got)
			continue
		}
		if got != tc.want {
			t.Errorf("ExtractPhone(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestFormatE164(t *testing.T) {
	if got := FormatE164("77015551234"); got != "+77015551234" {
		t.Errorf("FormatE164 = %q, want +77015551234", got)
	}
	if got := FormatE164("+77015551234"); got != "+77015551234" {
		t.Errorf("FormatE164 must not double the plus, got %q", got)
	}
	if got := FormatE164(""); got != "" {
		t.Errorf("FormatE164(\"\") = %q, want empty", got)
	}
}
