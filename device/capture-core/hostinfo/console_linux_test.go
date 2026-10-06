package hostinfo

import (
	"errors"
	"testing"

	"github.com/godbus/dbus/v5"
)

type fakeLogind struct {
	uid    uint32
	err    error
	seat   string
	closed bool
}

func (f *fakeLogind) activeSessionUID(seat string) (uint32, error) {
	f.seat = seat
	return f.uid, f.err
}

func (f *fakeLogind) close() { f.closed = true }

func openFake(f *fakeLogind, err error) func() (logindSession, error) {
	return func() (logindSession, error) {
		if err != nil {
			return nil, err
		}
		return f, nil
	}
}

func TestConsoleUserFromSelectsTheActiveSessionUser(t *testing.T) {
	session := &fakeLogind{uid: 1000}
	want := User{SID: "1000", Account: "alice"}
	got, err := consoleUserFrom(openFake(session, nil), func(uid uint32) (User, error) {
		if uid != 1000 {
			t.Fatalf("lookup uid = %d, want 1000", uid)
		}
		return want, nil
	})
	if err != nil {
		t.Fatalf("consoleUserFrom: %v", err)
	}
	if got != want {
		t.Fatalf("user = %+v, want %+v", got, want)
	}
	if session.seat != "seat0" {
		t.Fatalf("queried seat %q, want seat0", session.seat)
	}
	if !session.closed {
		t.Fatal("the logind connection was not closed")
	}
}

func TestConsoleUserFromNoActiveSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session *fakeLogind
		want    error
	}{
		{"no active session on the seat", &fakeLogind{err: errNoActiveSession}, ErrNoConsoleUser},
		{"session owned by root", &fakeLogind{uid: 0}, ErrNoConsoleUser},
		{"logind answers but with a D-Bus failure", &fakeLogind{uid: 1000, err: errors.New("dbus failure")}, ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := consoleUserFrom(openFake(tc.session, nil), func(uint32) (User, error) {
				t.Fatal("a uid that is not a person must not be looked up")
				return User{}, nil
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !tc.session.closed {
				t.Fatal("the logind connection was not closed")
			}
		})
	}
}

func TestConsoleUserFromNoLogindIsUnsupported(t *testing.T) {
	_, err := consoleUserFrom(openFake(nil, errors.New("no system bus")), func(uint32) (User, error) {
		t.Fatal("nothing should be looked up without logind")
		return User{}, nil
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestClassifyLogindError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"no such seat", dbus.Error{Name: "org.freedesktop.login1.NoSuchSeat"}, errNoActiveSession},
		{"no such session", dbus.Error{Name: "org.freedesktop.login1.NoSuchSession"}, errNoActiveSession},
		{"no service", dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}, dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}},
		{"plain error", errors.New("transport"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLogindError(tc.err)
			if tc.want == nil {
				if got != tc.err {
					t.Fatalf("got %v, want the original error", got)
				}
				return
			}
			if !errors.Is(got, tc.want) && got.Error() != tc.want.Error() {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
