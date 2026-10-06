// Package validators is the closed set of checks a rule can require before a pattern match
// becomes a label: a card-shaped number is a card only if its check digit holds, and an
// identifier-shaped string only if its structure does.
//
// A rule that names a validator outside this set is rejected when the rules are compiled. Each
// validator is a pure function of the matched text, so a rule that reaches one still cannot read
// a file or open a socket.
package validators

import (
	"sort"
	"strings"
)

// Check reports whether the matched candidate text passes the validator.
type Check func(candidate string) bool

var all = map[string]Check{
	"gb_nino": checkGBNINO,
	"luhn":    checkLuhn,
	"us_ssn":  checkUSSSN,
}

// Lookup returns the validator with this name, and false for any name outside the closed set.
func Lookup(name string) (Check, bool) {
	c, ok := all[name]
	return c, ok
}

// Names lists the closed set, sorted.
func Names() []string {
	out := make([]string, 0, len(all))
	for name := range all {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// digitsOnly strips the separators people and formatters put between digits, and refuses
// anything else.
func digitsOnly(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.':
		default:
			return "", false
		}
	}
	return b.String(), true
}

// checkLuhn accepts twelve to nineteen digits whose Luhn check digit holds: the payment card
// number lengths.
func checkLuhn(candidate string) bool {
	d, ok := digitsOnly(candidate)
	if !ok || len(d) < 12 || len(d) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// ssnDenylist holds never-issued numbers that are widely printed as examples.
var ssnDenylist = map[string]bool{
	"078051120": true, // printed on sample cards sold with wallets
	"219099999": true, // used in a Social Security Administration advertisement
	"123456789": true, // the canonical placeholder
}

// checkUSSSN accepts nine digits outside the ranges the Social Security Administration never
// issues: area 000, 666 or 9xx, group 00, serial 0000.
func checkUSSSN(candidate string) bool {
	d, ok := digitsOnly(candidate)
	if !ok || len(d) != 9 {
		return false
	}
	area, group, serial := d[:3], d[3:5], d[5:]
	if area == "000" || area == "666" || area[0] == '9' || group == "00" || serial == "0000" {
		return false
	}
	return !ssnDenylist[d]
}

// ninoUnallocated are the prefixes HMRC does not allocate.
var ninoUnallocated = map[string]bool{"BG": true, "GB": true, "KN": true, "NK": true, "NT": true, "TN": true, "ZZ": true}

// checkGBNINO accepts a UK National Insurance number: two prefix letters (neither D, F, I, Q, U
// or V, the second not O, and not an unallocated pair), six digits and a suffix A to D. Spaces
// between the groups are allowed.
func checkGBNINO(candidate string) bool {
	s := strings.ToUpper(strings.ReplaceAll(candidate, " ", ""))
	if len(s) != 9 {
		return false
	}
	for i := 0; i < 2; i++ {
		if s[i] < 'A' || s[i] > 'Z' || strings.IndexByte("DFIQUV", s[i]) >= 0 {
			return false
		}
	}
	if s[1] == 'O' || ninoUnallocated[s[:2]] {
		return false
	}
	for i := 2; i < 8; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s[8] >= 'A' && s[8] <= 'D'
}
