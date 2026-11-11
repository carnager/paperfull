package classify

import "testing"

func TestFindDate(t *testing.T) {
	cases := []struct {
		text     string
		dayFirst bool
		want     string
	}{
		{"Rechnungsdatum: 14.03.2025", true, "2025-03-14"},
		{"Musterstadt, den 21. August 2024", true, "2024-08-21"},
		{"Datum: 02. Januar 2026", true, "2026-01-02"},
		{"am 3. März 2023 erhalten", true, "2023-03-03"},
		{"Invoice date: March 5, 2024", true, "2024-03-05"},
		{"Date: 2023-11-30", true, "2023-11-30"},
		{"03/04/2024", true, "2024-04-03"},
		{"03/04/2024", false, "2024-03-04"},
		{"13/04/2024", false, "2024-04-13"}, // impossible month swaps back
		{"Kunde 4711, Tel 0171-555 1234", true, ""},
		{"gültig bis 31.12.2999, ausgestellt 01.02.2020", true, "2020-02-01"}, // future dates skipped
		{"31.02.2024 is not a date, 28.02.2024 is", true, "2024-02-28"},
		{"Stand 01.05.22", true, "2022-05-01"},
	}
	for _, c := range cases {
		if got := FindDate(c.text, c.dayFirst); got != c.want {
			t.Errorf("FindDate(%q, %v) = %q, want %q", c.text, c.dayFirst, got, c.want)
		}
	}
}
