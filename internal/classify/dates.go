package classify

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var months = map[string]int{
	"jan": 1, "januar": 1, "january": 1, "jän": 1, "jänner": 1,
	"feb": 2, "februar": 2, "february": 2,
	"mär": 3, "mrz": 3, "märz": 3, "maerz": 3, "mar": 3, "march": 3,
	"apr": 4, "april": 4,
	"mai": 5, "may": 5,
	"jun": 6, "juni": 6, "june": 6,
	"jul": 7, "juli": 7, "july": 7,
	"aug": 8, "august": 8,
	"sep": 9, "sept": 9, "september": 9,
	"okt": 10, "oct": 10, "oktober": 10, "october": 10,
	"nov": 11, "november": 11,
	"dez": 12, "dec": 12, "dezember": 12, "december": 12,
}

const monthAlt = `jänner|januar|january|jän|jan|februar|february|feb|märz|maerz|march|mär|mrz|mar|april|apr|mai|may|juni|june|jun|juli|july|jul|august|aug|september|sept|sep|oktober|october|okt|oct|november|nov|dezember|december|dez|dec`

var (
	reNumeric = regexp.MustCompile(`(?:^|[^\d])(\d{1,2})[./-](\d{1,2})[./-](\d{4}|\d{2})(?:[^\d]|$)`)
	reISO     = regexp.MustCompile(`(?:^|[^\d])(\d{4})-(\d{2})-(\d{2})(?:[^\d]|$)`)
	reDayName = regexp.MustCompile(`(?:^|[^\pL\d])(\d{1,2})\.?\s*(` + monthAlt + `)\.?\s+(\d{4})(?:[^\d]|$)`)
	reNameDay = regexp.MustCompile(`(?:^|[^\pL])(` + monthAlt + `)\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})(?:[^\d]|$)`)
)

type found struct {
	pos  int
	date time.Time
}

// FindDate returns the first plausible date in the text as YYYY-MM-DD, or "".
// dayFirst decides how 03/04/2024 is read (true = 3 April).
func FindDate(text string, dayFirst bool) string {
	if len(text) > 20000 {
		text = text[:20000]
	}
	t := strings.ToLower(text)
	now := time.Now()
	var hits []found
	addHit := func(pos, y, m, d int) {
		if y < 100 {
			if y > now.Year()%100+1 {
				y += 1900
			} else {
				y += 2000
			}
		}
		if y < 1900 || m < 1 || m > 12 || d < 1 || d > 31 {
			return
		}
		dt := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.Local)
		if dt.Day() != d || dt.After(now.AddDate(0, 0, 1)) {
			return
		}
		hits = append(hits, found{pos, dt})
	}
	for _, m := range reNumeric.FindAllStringSubmatchIndex(t, -1) {
		a, b, y := atoi(t, m, 2), atoi(t, m, 4), atoi(t, m, 6)
		d, mo := a, b
		if !dayFirst {
			d, mo = b, a
		}
		if mo > 12 && d <= 12 {
			d, mo = mo, d
		}
		addHit(m[2], y, mo, d)
	}
	for _, m := range reISO.FindAllStringSubmatchIndex(t, -1) {
		addHit(m[2], atoi(t, m, 2), atoi(t, m, 4), atoi(t, m, 6))
	}
	for _, m := range reDayName.FindAllStringSubmatchIndex(t, -1) {
		addHit(m[2], atoi(t, m, 6), months[t[m[4]:m[5]]], atoi(t, m, 2))
	}
	for _, m := range reNameDay.FindAllStringSubmatchIndex(t, -1) {
		addHit(m[2], atoi(t, m, 6), months[t[m[2]:m[3]]], atoi(t, m, 4))
	}
	if len(hits) == 0 {
		return ""
	}
	best := hits[0]
	for _, h := range hits[1:] {
		if h.pos < best.pos {
			best = h
		}
	}
	return best.date.Format("2006-01-02")
}

func atoi(s string, m []int, i int) int {
	n, _ := strconv.Atoi(s[m[i]:m[i+1]])
	return n
}
