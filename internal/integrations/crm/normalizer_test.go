package crm

import "testing"

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"+34 873 94 07 02", "34873940702"},
		{"+34 (873) 940-702", "34873940702"},
		{"34873940702", "34873940702"},
		{"0034873940702", "34873940702"},
		{"+1-555-123-4567", "15551234567"},
		{"   34   873  940 702  ", "34873940702"},
		{"", ""},
		{"++++34 873", "34873"},
		// Spanish landline format with parens & dashes
		{"+34 (93) 123-45-67", "34931234567"},
		// Just digits, no plus
		{"600123456", "600123456"},
		// Letters get stripped
		{"call34637000111now", "34637000111"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := NormalizePhone(c.in)
			if got != c.want {
				t.Errorf("NormalizePhone(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
