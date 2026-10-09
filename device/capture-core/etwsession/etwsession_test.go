package etwsession

import "testing"

// A provider with event ids delivers those and no others, whatever the case of the GUID; one
// without delivers everything.
func TestEventFilterLimitsOnlyTheProvidersWithEventIDs(t *testing.T) {
	f := newEventFilter([]Provider{
		{GUID: "{22fb2cd6-0e7b-422b-a0c7-2fad1fd0e716}", EventIDs: []uint16{1, 2}},
		{GUID: "{1C95126E-7EEA-49A9-A3FE-A378B03DDB4D}"},
	})
	cases := []struct {
		provider string
		id       uint16
		want     bool
	}{
		{"{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}", 1, true},
		{"{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}", 2, true},
		{"{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}", 5, false},
		{"{1C95126E-7EEA-49A9-A3FE-A378B03DDB4D}", 1001, true},
		{"{1c95126e-7eea-49a9-a3fe-a378b03ddb4d}", 3008, true},
	}
	for _, c := range cases {
		if got := f.keeps(c.provider, c.id); got != c.want {
			t.Errorf("keeps(%s, %d) = %v, want %v", c.provider, c.id, got, c.want)
		}
	}
}
