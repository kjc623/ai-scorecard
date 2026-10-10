package graphsync

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/scim"
)

// fakeDirectory is a customer's Entra directory.
type fakeDirectory struct {
	users    []User
	members  map[string][]string
	names    map[string]string
	usersErr error
}

func (d *fakeDirectory) Users(_ context.Context, _ string, fn func(User) error) error {
	for _, u := range d.users {
		if err := fn(u); err != nil {
			return err
		}
	}
	return d.usersErr
}

func (d *fakeDirectory) GroupMembers(_ context.Context, _, id string) ([]string, error) {
	m, ok := d.members[id]
	if !ok {
		return nil, &GraphError{Status: http.StatusNotFound, Code: "Request_ResourceNotFound"}
	}
	return m, nil
}

func (d *fakeDirectory) Group(_ context.Context, _, id string) (Group, error) {
	return Group{ID: id, DisplayName: d.names[id]}, nil
}

// fakeProvisioner keeps users and groups as the SCIM service renders them, and records writes.
type fakeProvisioner struct {
	users  map[string]map[string]any
	groups map[string]map[string]any
	writes []string
	refuse map[string]bool
	next   int
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{users: map[string]map[string]any{}, groups: map[string]map[string]any{}, refuse: map[string]bool{}}
}

func (f *fakeProvisioner) id() string {
	f.next++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.next)
}

func (f *fakeProvisioner) ListUsers(_ context.Context, p scim.Principal, q scim.ListQuery) (*scim.ListResponse, *scim.Error) {
	if p.Actor != Actor {
		return nil, &scim.Error{Status: 401}
	}
	ids := make([]string, 0, len(f.users))
	for id := range f.users {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	start := q.StartIndex - 1
	end := min(start+q.Count, len(ids))
	var page []map[string]any
	if start < len(ids) {
		for _, id := range ids[start:end] {
			page = append(page, f.users[id])
		}
	}
	return &scim.ListResponse{TotalResults: len(ids), StartIndex: q.StartIndex, ItemsPerPage: len(page), Resources: page}, nil
}

func (f *fakeProvisioner) CreateUser(_ context.Context, _ scim.Principal, body map[string]any) (map[string]any, *scim.Error) {
	if f.refuse[body["userName"].(string)] {
		return nil, &scim.Error{Status: 409, Detail: "conflict"}
	}
	id := f.id()
	body["id"] = id
	f.users[id] = body
	f.writes = append(f.writes, "create "+body["userName"].(string))
	return body, nil
}

func (f *fakeProvisioner) ReplaceUser(_ context.Context, _ scim.Principal, id string, body map[string]any) (map[string]any, *scim.Error) {
	body["id"] = id
	f.users[id] = body
	f.writes = append(f.writes, "replace "+body["userName"].(string))
	return body, nil
}

func (f *fakeProvisioner) DeleteUser(_ context.Context, _ scim.Principal, id string) *scim.Error {
	f.users[id]["active"] = false
	f.writes = append(f.writes, "retire "+f.users[id]["userName"].(string))
	return nil
}

func (f *fakeProvisioner) GetGroup(_ context.Context, _ scim.Principal, id string, _ bool) (map[string]any, *scim.Error) {
	g, ok := f.groups[id]
	if !ok {
		return nil, &scim.Error{Status: 404}
	}
	return g, nil
}

func (f *fakeProvisioner) ReplaceGroup(_ context.Context, _ scim.Principal, id string, body map[string]any) (map[string]any, *scim.Error) {
	body["id"] = id
	f.groups[id] = body
	f.writes = append(f.writes, "group "+body["displayName"].(string))
	return body, nil
}

const (
	oidAda   = "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	oidGrace = "26118915-6090-4610-87a4-49d0767e9fde"
	oidGuest = "0f0f0f0f-0000-4000-8000-000000000001"
	oidGroup = "11111111-2222-4333-8444-555555555555"
	tenant   = "84beb829-b508-437c-9cd2-501f36e28b81"
)

func boolp(b bool) *bool { return &b }

func TestSyncCreatesUpdatesAndLeavesUnchangedPeopleAlone(t *testing.T) {
	dir := &fakeDirectory{users: []User{
		{ID: oidAda, UserPrincipalName: "ada@contoso.com", DisplayName: "Ada Lovelace", Department: "Engineering",
			AccountEnabled: boolp(true), UserType: "Member", OnPremisesDistinguishedName: "CN=Ada,OU=Eng,DC=contoso,DC=com"},
		{ID: oidGuest, UserPrincipalName: "guest_fabrikam.com#EXT#@contoso.com", UserType: "Guest"},
		{ID: "not-a-guid", UserPrincipalName: "broken@contoso.com"},
	}}
	prov := newFakeProvisioner()
	s := &Syncer{Directory: dir, Provisioner: prov}

	res := s.Sync(context.Background(), tenant, entraTenant)
	if res.Error != "" || res.Users != 1 || strings.Join(prov.writes, ";") != "create ada@contoso.com" {
		t.Fatalf("first pass = %+v, writes %v", res, prov.writes)
	}
	var ada map[string]any
	for _, u := range prov.users {
		ada = u
	}
	if ada["externalId"] != oidAda || ada["active"] != true || ada["displayName"] != "Ada Lovelace" {
		t.Fatalf("created = %v", ada)
	}
	if ext := ada[scim.SchemaEnterpriseUser].(map[string]any); ext["department"] != "Engineering" {
		t.Fatalf("enterprise extension = %v", ext)
	}
	if ext := ada[scim.SchemaDirectoryUser].(map[string]any); ext["orgUnit"] != "OU=Eng,DC=contoso,DC=com" {
		t.Fatalf("directory extension = %v", ext)
	}

	// A second pass over the same directory writes nothing, so it adds no audit rows.
	prov.writes = nil
	if res := s.Sync(context.Background(), tenant, entraTenant); res.Error != "" || len(prov.writes) != 0 {
		t.Fatalf("unchanged pass = %+v, writes %v", res, prov.writes)
	}

	// A change is written on top of what another provider set.
	ada["emails"] = []any{map[string]any{"value": "ada@contoso.com"}}
	dir.users[0].Department = "Research"
	dir.users[0].AccountEnabled = boolp(false)
	s.Sync(context.Background(), tenant, entraTenant)
	if strings.Join(prov.writes, ";") != "replace ada@contoso.com" {
		t.Fatalf("writes = %v", prov.writes)
	}
	for _, u := range prov.users {
		ada = u
	}
	if ada["active"] != false || ada["emails"] == nil || ada[scim.SchemaEnterpriseUser].(map[string]any)["department"] != "Research" {
		t.Fatalf("replaced = %v", ada)
	}
}

func TestSyncMatchesByUserNameAndRetiresTheDeparted(t *testing.T) {
	prov := newFakeProvisioner()
	// Grace was provisioned by SCIM without her object id; Bob has left the directory.
	prov.users["00000000-0000-4000-8000-000000000101"] = map[string]any{"id": "00000000-0000-4000-8000-000000000101", "userName": "Grace@contoso.com", "active": true}
	prov.users["00000000-0000-4000-8000-000000000102"] = map[string]any{"id": "00000000-0000-4000-8000-000000000102", "userName": "bob@contoso.com", "externalId": oidAda, "active": true}
	dir := &fakeDirectory{users: []User{{ID: oidGrace, UserPrincipalName: "grace@contoso.com", DisplayName: "Grace Hopper"}}}
	res := (&Syncer{Directory: dir, Provisioner: prov}).Sync(context.Background(), tenant, entraTenant)
	if res.Retired != 1 || strings.Join(prov.writes, ";") != "replace grace@contoso.com;retire bob@contoso.com" {
		t.Fatalf("res = %+v, writes %v", res, prov.writes)
	}
	if prov.users["00000000-0000-4000-8000-000000000101"]["externalId"] != oidGrace {
		t.Fatal("the matched person did not gain their object id")
	}
}

func TestSyncRetiresNobodyWhenTheListingFails(t *testing.T) {
	prov := newFakeProvisioner()
	prov.users["00000000-0000-4000-8000-000000000102"] = map[string]any{"id": "00000000-0000-4000-8000-000000000102", "userName": "bob@contoso.com", "externalId": oidAda, "active": true}
	dir := &fakeDirectory{usersErr: &GraphError{Status: http.StatusForbidden, Code: "Authorization_RequestDenied"}}
	res := (&Syncer{Directory: dir, Provisioner: prov}).Sync(context.Background(), tenant, entraTenant)
	if res.Error != ErrConsentMissing || len(prov.writes) != 0 {
		t.Fatalf("res = %+v, writes %v", res, prov.writes)
	}
	if res := (&Syncer{Directory: dir, Provisioner: prov}).Sync(context.Background(), tenant, ""); res.Error != ErrEntraNotConnected {
		t.Fatalf("no Entra = %+v", res)
	}
}

func TestSyncCountsRefusedPeopleAndCarriesOn(t *testing.T) {
	prov := newFakeProvisioner()
	prov.refuse["ada@contoso.com"] = true
	dir := &fakeDirectory{users: []User{
		{ID: oidAda, UserPrincipalName: "ada@contoso.com"},
		{ID: oidGrace, UserPrincipalName: "grace@contoso.com"},
	}}
	res := (&Syncer{Directory: dir, Provisioner: prov}).Sync(context.Background(), tenant, entraTenant)
	if res.Failed != 1 || res.Error != ErrPeopleNotSynced || strings.Join(prov.writes, ";") != "create grace@contoso.com" {
		t.Fatalf("res = %+v, writes %v", res, prov.writes)
	}
}

func TestSyncReplacesTheMembersOfImportedGroups(t *testing.T) {
	prov := newFakeProvisioner()
	groupID := "00000000-0000-4000-8000-000000000900"
	prov.groups[groupID] = map[string]any{"id": groupID, "displayName": "Sales", "externalId": oidGroup, "members": []any{}}
	dir := &fakeDirectory{
		users:   []User{{ID: oidAda, UserPrincipalName: "ada@contoso.com"}, {ID: oidGrace, UserPrincipalName: "grace@contoso.com"}},
		members: map[string][]string{oidGroup: {oidAda, oidGuest}},
		names:   map[string]string{oidGroup: "Sales EMEA"},
	}
	s := (&Syncer{Directory: dir, Provisioner: prov}).WithGroups(func(context.Context, string) ([]ImportedGroup, error) {
		return []ImportedGroup{{ID: groupID, ExternalID: oidGroup}, {ID: "00000000-0000-4000-8000-000000000901", ExternalID: oidGrace}}, nil
	})
	res := s.Sync(context.Background(), tenant, entraTenant)
	if res.Error != "" || res.Groups != 1 {
		t.Fatalf("res = %+v", res)
	}
	g := prov.groups[groupID]
	members := g["members"].([]any)
	if g["displayName"] != "Sales EMEA" || len(members) != 1 {
		t.Fatalf("group = %v", g)
	}
	if id := members[0].(map[string]any)["value"]; prov.users[id.(string)]["userName"] != "ada@contoso.com" {
		t.Fatalf("member = %v", id)
	}
	// Nothing changed: no group write.
	prov.writes = nil
	s.Sync(context.Background(), tenant, entraTenant)
	if len(prov.writes) != 0 {
		t.Fatalf("writes = %v", prov.writes)
	}
}
