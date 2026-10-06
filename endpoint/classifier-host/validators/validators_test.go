package validators_test

import (
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/validators"
)

func TestLookupRefusesNamesOutsideTheClosedSet(t *testing.T) {
	for _, name := range []string{"exec", "", "LUHN"} {
		if _, ok := validators.Lookup(name); ok {
			t.Errorf("Lookup accepted %q", name)
		}
	}
	for _, name := range validators.Names() {
		if c, ok := validators.Lookup(name); !ok || c == nil {
			t.Errorf("validator %q is listed but not usable", name)
		}
	}
}

func TestValidators(t *testing.T) {
	cases := []struct {
		validator string
		value     string
		want      bool
	}{
		{"luhn", "4111 1111 1111 1111", true},
		{"luhn", "4111-1111-1111-1111", true},
		{"luhn", "4111.1111.1111.1111", true},
		{"luhn", "4111 1111 1111 1112", false},
		{"luhn", "1234", false},
		{"luhn", "41111111111111111111", false},
		{"luhn", "4111 1111 1111 11a1", false},
		{"us_ssn", "123-45-6788", true},
		{"us_ssn", "123 45 6788", true},
		{"us_ssn", "123-45-6789", false},
		{"us_ssn", "000-12-3456", false},
		{"us_ssn", "666-12-3456", false},
		{"us_ssn", "900-12-3456", false},
		{"us_ssn", "123-00-6789", false},
		{"us_ssn", "123-45-0000", false},
		{"us_ssn", "078-05-1120", false},
		{"us_ssn", "219-09-9999", false},
		{"gb_nino", "AB123456C", true},
		{"gb_nino", "AB 12 34 56 C", true},
		{"gb_nino", "DA123456C", false},
		{"gb_nino", "AO123456C", false},
		{"gb_nino", "GB123456C", false},
		{"gb_nino", "AB123456E", false},
		{"gb_nino", "AB12345C", false},
	}
	for _, tc := range cases {
		check, ok := validators.Lookup(tc.validator)
		if !ok {
			t.Fatalf("validator %q is missing from the closed set", tc.validator)
		}
		if got := check(tc.value); got != tc.want {
			t.Errorf("%s(%q) = %v, want %v", tc.validator, tc.value, got, tc.want)
		}
	}
}
