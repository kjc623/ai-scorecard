// Package validators is the closed validator set of docs/01-collectors.md §9.2 and §9.5.
//
// §9.2: "Validators turn a pattern match into a defensible verdict: a payment-card-shaped
// string is a payment card only if the checksum passes, a government-identifier-shaped string
// only if its structure and, where applicable, its check digit hold — the difference between a
// rule set a customer trusts and one that flags every 16-digit number."
//
// §9.5: a rule may reference "a closed set of validators that ship in the binary". This
// package *is* that closed set. A rule naming a validator that is not here is rejected at
// load time, whole — it can never fall through to a permissive default.
//
// The package deliberately imports nothing but the standard library's string/unicode packages:
// no os, no net, no time. A validator is a pure function of the candidate string, so a rule
// that reaches one still cannot read a file, open a socket, or look at the spool.
package validators

import (
	"strings"
	"unicode"
)

// Kind separates a check-digit verdict from a structural one, so a report can say which kind
// of evidence a label rests on.
type Kind string

const (
	// KindChecksum is a check-digit or modular-arithmetic verdict.
	KindChecksum Kind = "checksum"
	// KindStructure is a format verdict with no check digit.
	KindStructure Kind = "structure"
)

// Validator is one named, pure predicate over the matched candidate text.
type Validator struct {
	Name        string
	Kind        Kind
	Description string
	Check       func(candidate string) bool
}

// Lookup returns the validator with this name. The second result is false for anything not in
// the closed set, which is what makes a rule naming an unknown validator a load-time rejection
// rather than a silently dropped signal.
func Lookup(name string) (Validator, bool) {
	for _, v := range all {
		if v.Name == name {
			return v, true
		}
	}
	return Validator{}, false
}

// Names is the closed set, sorted, for validation messages and for the release manifest
// binding (§9.5: "classifier_version identifies a (rules, validators, model, shape-predicate)
// tuple").
func Names() []string {
	out := make([]string, 0, len(all))
	for _, v := range all {
		out = append(out, v.Name)
	}
	return out
}

// all is the closed set. Order is by name so Names() is stable.
var all = []Validator{
	{Name: "ca_sin", Kind: KindChecksum, Description: "Canadian SIN: nine digits, Luhn check digit", Check: checkSIN},
	{Name: "ean13", Kind: KindChecksum, Description: "EAN-13: thirteen digits, GS1 mod-10 check digit", Check: checkEAN13},
	{Name: "gb_nino", Kind: KindStructure, Description: "UK National Insurance number: two prefix letters, six digits, suffix A-D", Check: checkGBNINO},
	{Name: "iban", Kind: KindChecksum, Description: "IBAN: country-length structure and ISO 7064 mod-97 check", Check: checkIBAN},
	{Name: "luhn", Kind: KindChecksum, Description: "Luhn check digit over twelve to nineteen digits (payment cards)", Check: checkLuhn},
	{Name: "npi", Kind: KindChecksum, Description: "US NPI: ten digits beginning 1 or 2, Luhn over the 80840 prefix", Check: checkNPI},
	{Name: "upc_a", Kind: KindChecksum, Description: "UPC-A: twelve digits, mod-10 check digit", Check: checkUPCA},
	{Name: "us_ssn", Kind: KindStructure, Description: "US SSN structure with the reserved ranges and published invalid numbers excluded", Check: checkUSSSN},
}

// digitsOnly strips the separators a human or a formatter inserts between digits. Refusing
// separators would make a rule blind to "4111 1111 1111 1111", which is how cards are written.
func digitsOnly(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '\u00a0' || r == '\u2011' || r == '\u2013':
			// separator: skipped
		default:
			return "", false
		}
	}
	return b.String(), true
}

func checkLuhn(candidate string) bool {
	d, ok := digitsOnly(candidate)
	if !ok || len(d) < 12 || len(d) > 19 {
		return false
	}
	return luhn(d)
}

// luhn is the Luhn mod-10 checksum over an ASCII digit string.
func luhn(d string) bool {
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

func checkEAN13(candidate string) bool {
	d, ok := digitsOnly(candidate)
	if !ok || len(d) != 13 {
		return false
	}
	return mod10Weighted(d, 13, []int{1, 3})
}

func checkUPCA(candidate string) bool {
	d, ok := digitsOnly(candidate)
	if !ok || len(d) != 12 {
		return false
	}
	return mod10Weighted(d, 12, []int{3, 1})
}

// mod10Weighted checks the GS1-style mod-10 check digit: weights alternate from the left across
// the payload digits, and the final digit is the check digit.
func mod10Weighted(d string, length int, weights []int) bool {
	if len(d) != length {
		return false
	}
	sum := 0
	for i := 0; i < length-1; i++ {
		sum += int(d[i]-'0') * weights[i%len(weights)]
	}
	return (10-sum%10)%10 == int(d[length-1]-'0')
}

func checkSIN(candidate string) bool {
	d, ok := digitsOnly(candidate)
	if !ok || len(d) != 9 {
		return false
	}
	// 0 is not a valid SIN, and the first digit may not be 0 or 8 in practice; keep the
	// structural check minimal and defensible: all-zero is not a number.
	if d == "000000000" {
		return false
	}
	return luhn(d)
}

func checkNPI(candidate string) bool {
	d, ok := digitsOnly(candidate)
	if !ok || len(d) != 10 {
		return false
	}
	if d[0] != '1' && d[0] != '2' {
		return false
	}
	// The NPI check digit is Luhn over the constant prefix 80840 followed by the first nine
	// digits, which is equivalent to Luhn over "80840"+d[:9].
	return luhn("80840" + d[:9] + d[9:])
}

// ibanLengths is the ISO 13616 register for the countries this build ships. A country outside
// the table falls back to the 15..34 length range; an unknown country is not a checksum pass,
// it is a structural one.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22,
	"BH": 22, "BR": 29, "BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24, "DE": 22,
	"DK": 18, "DO": 28, "EE": 20, "EG": 29, "ES": 24, "FI": 18, "FO": 18, "FR": 27,
	"GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28, "HR": 21, "HU": 28,
	"IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20,
	"LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "MC": 27, "MD": 24,
	"ME": 22, "MK": 19, "MR": 27, "MT": 31, "MU": 30, "NL": 18, "NO": 15, "PK": 24,
	"PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22, "SA": 24, "SC": 31,
	"SE": 24, "SI": 19, "SK": 24, "SM": 27, "ST": 25, "SV": 28, "TL": 23, "TN": 24,
	"TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20,
}

func checkIBAN(candidate string) bool {
	s := strings.ToUpper(strings.Join(strings.Fields(candidate), ""))
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	if s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' {
		return false
	}
	if s[2] < '0' || s[2] > '9' || s[3] < '0' || s[3] > '9' {
		return false
	}
	if want, ok := ibanLengths[s[:2]]; ok && len(s) != want {
		return false
	}
	// ISO 7064 mod 97-10: move the first four characters to the end, map letters to 10..35,
	// and require remainder 1.
	rearranged := s[4:] + s[:4]
	rem := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		if c >= '0' && c <= '9' {
			rem = (rem*10 + int(c-'0')) % 97
			continue
		}
		v := int(c-'A') + 10
		rem = (rem*100 + v) % 97
	}
	return rem == 1
}

// ssnDenylist is the small set of published, never-issued SSNs that appear in documentation
// and test data. A validator that flagged them would make the shipped test corpus a false
// positive, which is exactly what §9.2's "defensible verdict" is about.
var ssnDenylist = map[string]bool{
	"078051120": true, // the SSN printed on sample cards since 1938
	"219099999": true, // Woolworth wallet SSN
	"457555462": true, // published as belonging to a real person
}

func checkUSSSN(candidate string) bool {
	d, ok := digitsOnly(candidate)
	if !ok || len(d) != 9 {
		return false
	}
	area := d[:3]
	group := d[3:5]
	serial := d[5:]
	if area == "000" || area == "666" || area[0] == '9' {
		return false
	}
	if group == "00" || serial == "0000" {
		return false
	}
	return !ssnDenylist[d]
}

func checkGBNINO(candidate string) bool {
	s := strings.ToUpper(strings.Join(strings.Fields(candidate), ""))
	if len(s) != 9 {
		return false
	}
	bad := "DFIQUV"
	for i := 0; i < 2; i++ {
		c := s[i]
		if c < 'A' || c > 'Z' || strings.ContainsRune(bad, rune(c)) {
			return false
		}
	}
	for i := 2; i < 8; i++ {
		if !unicode.IsDigit(rune(s[i])) {
			return false
		}
	}
	c := s[8]
	return c >= 'A' && c <= 'D'
}
