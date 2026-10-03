package validators_test

import (
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/validators"
)

// TestClosedSetHasNoUnknownValidator is §9.5's "closed set of validators that ship in the binary":
// a rule naming anything else is a load-time rejection, which rules_test.go asserts from the other
// side.
func TestClosedSetHasNoUnknownValidator(t *testing.T) {
	if _, ok := validators.Lookup("exec"); ok {
		t.Fatal("Lookup accepted a validator that is not in the closed set")
	}
	if _, ok := validators.Lookup(""); ok {
		t.Fatal("Lookup accepted an empty validator name")
	}
	for _, name := range validators.Names() {
		v, ok := validators.Lookup(name)
		if !ok || v.Check == nil || v.Description == "" || v.Kind == "" {
			t.Errorf("validator %q is incomplete: %+v", name, v)
		}
	}
}

func TestValidators(t *testing.T) {
	cases := []struct {
		validator string
		value     string
		want      bool
	}{
		// §9.2: "a payment-card-shaped string is a payment card only if the checksum passes".
		{"luhn", "4111 1111 1111 1111", true},
		{"luhn", "4111-1111-1111-1111", true},
		{"luhn", "4111 1111 1111 1112", false},
		{"luhn", "1234", false},                 // shorter than a PAN
		{"luhn", "41111111111111111111", false}, // longer
		{"luhn", "4111 1111 1111 11a1", false},
		// IBAN: structure *and* ISO 7064 mod-97.
		{"iban", "GB82 WEST 1234 5698 7654 32", true},
		{"iban", "GB82WEST12345698765432", true},
		{"iban", "GB82WEST12345698765433", false},
		{"iban", "GB82WEST123456987654321", false}, // GB is 22 characters
		{"iban", "ZZ82WEST12345698765432", false},  // an unknown country still has to pass mod-97, and this does not
		// US SSN: structure, reserved ranges, published invalid numbers.
		{"us_ssn", "123-45-6789", true},
		{"us_ssn", "123456789", true},
		{"us_ssn", "000-12-3456", false},
		{"us_ssn", "666-12-3456", false},
		{"us_ssn", "900-12-3456", false},
		{"us_ssn", "123-00-6789", false},
		{"us_ssn", "123-45-0000", false},
		{"us_ssn", "078-05-1120", false}, // the sample card SSN
		{"us_ssn", "219-09-9999", false}, // the Woolworth SSN
		{"gb_nino", "AB123456C", true},
		{"gb_nino", "DA123456C", false}, // D is not a valid prefix letter
		{"gb_nino", "AB123456E", false}, // suffix outside A-D
		{"gb_nino", "AB12345C", false},
		{"ean13", "4006381333931", true},
		{"ean13", "4006381333932", false},
		{"upc_a", "036000291452", true},
		{"upc_a", "036000291453", false},
		{"ca_sin", "046454286", true},
		{"ca_sin", "046454287", false},
		{"npi", "1234567893", true},
		{"npi", "1234567894", false},
		{"npi", "3234567893", false}, // NPI begins 1 or 2
	}
	for _, tc := range cases {
		v, ok := validators.Lookup(tc.validator)
		if !ok {
			t.Fatalf("validator %q is missing from the closed set", tc.validator)
		}
		if got := v.Check(tc.value); got != tc.want {
			t.Errorf("%s(%q) = %v, want %v", tc.validator, tc.value, got, tc.want)
		}
	}
}

// TestValidatorIsPureAndRepeatable guards the property the validator set is used for: the same
// candidate must always give the same answer, since a label's defensibility rests on it.
func TestValidatorIsPureAndRepeatable(t *testing.T) {
	v, _ := validators.Lookup("luhn")
	for i := 0; i < 100; i++ {
		if !v.Check("4111111111111111") {
			t.Fatal("a valid card was rejected on a repeat call")
		}
		if v.Check("4111111111111112") {
			t.Fatal("an invalid card was accepted on a repeat call")
		}
	}
}
