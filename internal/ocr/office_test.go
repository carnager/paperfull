package ocr

import "testing"

func TestExcelDate(t *testing.T) {
	cases := map[string]string{"46027": "2026-01-05", "45292.5": "2024-01-01 12:00", "abc": "abc", "0": "0"}
	for in, want := range cases {
		if got := excelDate(in); got != want {
			t.Errorf("excelDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDateFormat(t *testing.T) {
	cases := []struct {
		id   int
		code string
		want bool
	}{
		{14, "", true}, {164, `yyyy\-mm\-dd`, true}, {165, `dd.mm.yyyy`, true},
		{166, `#,##0.00 "EUR"`, false}, {167, `[Red]0.00`, false}, {168, `0.00 "days"`, false}, {2, "", false},
	}
	for _, c := range cases {
		if got := isDateFormat(c.id, c.code); got != c.want {
			t.Errorf("isDateFormat(%d, %q) = %v, want %v", c.id, c.code, got, c.want)
		}
	}
}

func TestColIndex(t *testing.T) {
	for ref, want := range map[string]int{"A1": 0, "B7": 1, "Z3": 25, "AA10": 26, "AB1": 27} {
		if got := colIndex(ref); got != want {
			t.Errorf("colIndex(%q) = %d, want %d", ref, got, want)
		}
	}
}
