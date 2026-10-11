package directoryadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/deploy"
)

const testTenant = "6f1c2a52-58f4-4a51-9b1e-1f6d2f0a0001"

// teamStore answers the calls a team creation makes; anything else panics on the nil Store.
type teamStore struct {
	Store
	created []Team
}

func (s *teamStore) CreateTeam(_ context.Context, _ string, t Team, _ []string, _ Audit) error {
	s.created = append(s.created, t)
	return nil
}

type kicks struct{ tenants []string }

func (k *kicks) Trigger(tenantID string) bool {
	k.tenants = append(k.tenants, tenantID)
	return true
}

func TestCreatingAGroupTeamAsksForARead(t *testing.T) {
	st := &teamStore{}
	sync := &kicks{}
	auth := func(*http.Request) (deploy.Principal, error) {
		return deploy.Principal{Tenant: testTenant, Actor: "admin@contoso.com", Roles: []string{deploy.RoleAdmin}}, nil
	}
	h, err := NewHandler(st, auth, Config{Sync: sync})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	create := func(body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/teams", strings.NewReader(body)))
		return rec.Code
	}

	// A group already held, as one an earlier attempt imported is, still needs its members read.
	if code := create(`{"name":"Sales","source":"group","group_id":"7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0009"}`); code != http.StatusCreated {
		t.Fatalf("create group team = %d", code)
	}
	if len(sync.tenants) != 1 || sync.tenants[0] != testTenant {
		t.Fatalf("reads asked for = %v", sync.tenants)
	}
	if code := create(`{"name":"Engineering","source":"department","match_value":"Engineering"}`); code != http.StatusCreated {
		t.Fatalf("create department team = %d", code)
	}
	if len(sync.tenants) != 1 {
		t.Fatalf("a department team asked for a read: %v", sync.tenants)
	}
}
