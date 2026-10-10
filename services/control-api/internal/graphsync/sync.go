// Package graphsync pulls a customer's people and groups from Microsoft Graph into the product's
// directory, for a tenant whose identity provider does not push them by SCIM.
//
// The pull is an in-process SCIM client: it writes through scim.Service exactly what Entra's own
// provisioning service would send (userName the UPN, externalId the object id, the department in the
// enterprise extension, the organisational unit in the product's extension), so a person's
// canonical ref, aliases, directory row and audit trail are the same whichever way they arrived, and
// a tenant can move between the two without splitting anyone's history. A person is matched by
// externalId, then by userName; a write starts from the stored resource, so attributes another
// provider set are kept, and an unchanged person is not written at all.
//
// A person Graph no longer lists is retired, as a SCIM DELETE retires them, but only when the
// listing completed and only for a person whose externalId is an object id. Groups are pulled only
// when a team follows them: the console imports a group by its object id, and each pass replaces
// that group's members with its transitive user members.
package graphsync

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/scim"
)

// Actor is how ops.audit names the pull.
const Actor = "directory-sync:graph"

// The closed set of ops.directory_sync.last_error codes.
const (
	ErrEntraNotConnected = "entra_not_connected"
	ErrConsentMissing    = "graph_consent_missing"
	ErrTokenRefused      = "graph_token_refused"
	ErrGraphUnavailable  = "graph_unavailable"
	ErrPeopleNotSynced   = "people_not_synced"
	ErrInternal          = "internal"
)

// Directory reads a customer's directory. *Client implements it.
type Directory interface {
	Users(ctx context.Context, entraTenantID string, fn func(User) error) error
	GroupMembers(ctx context.Context, entraTenantID, groupID string) ([]string, error)
	Group(ctx context.Context, entraTenantID, groupID string) (Group, error)
}

// Provisioner is the SCIM service the pull writes through. *scim.Service implements it.
type Provisioner interface {
	ListUsers(ctx context.Context, p scim.Principal, q scim.ListQuery) (*scim.ListResponse, *scim.Error)
	CreateUser(ctx context.Context, p scim.Principal, body map[string]any) (map[string]any, *scim.Error)
	ReplaceUser(ctx context.Context, p scim.Principal, id string, body map[string]any) (map[string]any, *scim.Error)
	DeleteUser(ctx context.Context, p scim.Principal, id string) *scim.Error
	GetGroup(ctx context.Context, p scim.Principal, id string, excludeMembers bool) (map[string]any, *scim.Error)
	ReplaceGroup(ctx context.Context, p scim.Principal, id string, body map[string]any) (map[string]any, *scim.Error)
}

// Result is one pass for one tenant.
type Result struct {
	Users   int
	Groups  int
	Retired int
	// Failed counts people the provisioner refused; the rest of the pass still ran.
	Failed int
	// Error is a closed code, empty when the pass succeeded.
	Error string
}

// Syncer runs one tenant's pass.
type Syncer struct {
	Directory   Directory
	Provisioner Provisioner
	Logger      *slog.Logger

	groups func(ctx context.Context, tenantID string) ([]ImportedGroup, error)
}

// Sync pulls one tenant's directory. entraTenantID is the customer's Entra directory.
func (s *Syncer) Sync(ctx context.Context, tenantID, entraTenantID string) Result {
	log := s.logger().With("tenant", tenantID)
	if entraTenantID == "" {
		return Result{Error: ErrEntraNotConnected}
	}
	p := scim.Principal{TenantID: tenantID, Actor: Actor}

	existing, err := s.existingUsers(ctx, p)
	if err != nil {
		log.Error("directory sync: read provisioned people", "error", err)
		return Result{Error: ErrInternal}
	}

	var res Result
	seen := map[string]bool{}
	byObjectID := map[string]string{}
	// SCIM ids this pass matched: a person whose object id changed is matched by userName and
	// must not then be retired under the old one.
	kept := map[string]bool{}
	err = s.Directory.Users(ctx, entraTenantID, func(u User) error {
		oid := strings.ToLower(strings.TrimSpace(u.ID))
		upn := strings.TrimSpace(u.UserPrincipalName)
		// A guest is someone else's employee, and an object with no UPN cannot be signed in to.
		if !isGUID(oid) || upn == "" || strings.EqualFold(u.UserType, "Guest") {
			return nil
		}
		seen[oid] = true
		res.Users++
		id, err := s.upsertUser(ctx, p, existing, u, oid, upn)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			res.Failed++
			log.Warn("directory sync: a person was not provisioned", "error", err)
			return nil
		}
		byObjectID[oid] = id
		kept[id] = true
		return nil
	})
	if err != nil {
		code := graphCode(err)
		log.Error("directory sync: list users", "error", err, "code", code)
		res.Error = code
		return res
	}

	for oid, r := range existing.byObjectID {
		if seen[oid] || kept[r.id] || !r.active {
			continue
		}
		if e := s.Provisioner.DeleteUser(ctx, p, r.id); e != nil {
			res.Failed++
			log.Warn("directory sync: a departed person was not retired", "error", e)
			continue
		}
		res.Retired++
	}

	groups, err := s.syncGroups(ctx, p, entraTenantID, byObjectID, existing)
	res.Groups = groups
	if err != nil {
		code := graphCode(err)
		log.Error("directory sync: groups", "error", err, "code", code)
		res.Error = code
		return res
	}
	if res.Failed > 0 {
		res.Error = ErrPeopleNotSynced
	}
	return res
}

func (s *Syncer) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// graphCode maps a failure of the pass to its closed code.
func graphCode(err error) string {
	var t *TokenError
	var p provisionError
	switch {
	case IsConsent(err):
		return ErrConsentMissing
	case errors.As(err, &t):
		return ErrTokenRefused
	case errors.As(err, &p):
		return ErrInternal
	}
	return ErrGraphUnavailable
}

// provisionError is the provisioner refusing a group write, which is the product's failure, not
// the directory's.
type provisionError struct{ err error }

func (e provisionError) Error() string { return "graphsync: provision group: " + e.err.Error() }
func (e provisionError) Unwrap() error { return e.err }

// provisioned is one stored person as the pull needs them.
type provisioned struct {
	id       string
	active   bool
	resource map[string]any
}

type userIndex struct {
	byObjectID map[string]provisioned
	byUserName map[string]provisioned
}

// existingUsers reads every provisioned person, a page at a time.
func (s *Syncer) existingUsers(ctx context.Context, p scim.Principal) (userIndex, error) {
	idx := userIndex{byObjectID: map[string]provisioned{}, byUserName: map[string]provisioned{}}
	for start := 1; ; {
		page, e := s.Provisioner.ListUsers(ctx, p, scim.ListQuery{StartIndex: start, Count: scim.MaxPageSize})
		if e != nil {
			return idx, e
		}
		for _, r := range page.Resources {
			id, _ := r["id"].(string)
			active, ok := r["active"].(bool)
			if !ok {
				active = true
			}
			entry := provisioned{id: id, active: active, resource: r}
			if ext, _ := r["externalId"].(string); isGUID(strings.ToLower(strings.Trim(ext, "{}"))) {
				idx.byObjectID[strings.ToLower(strings.Trim(ext, "{}"))] = entry
			}
			if un, _ := r["userName"].(string); un != "" {
				idx.byUserName[strings.ToLower(un)] = entry
			}
		}
		if len(page.Resources) == 0 || start+len(page.Resources) > page.TotalResults {
			return idx, nil
		}
		start += len(page.Resources)
	}
}

// upsertUser writes one person and returns their SCIM id.
func (s *Syncer) upsertUser(ctx context.Context, p scim.Principal, idx userIndex, u User, oid, upn string) (string, error) {
	cur, found := idx.byObjectID[oid]
	if !found {
		cur, found = idx.byUserName[strings.ToLower(upn)]
	}
	if !found {
		created, e := s.Provisioner.CreateUser(ctx, p, userResource(nil, u, oid, upn))
		if e != nil {
			return "", e
		}
		id, _ := created["id"].(string)
		return id, nil
	}
	next := userResource(cur.resource, u, oid, upn)
	if sameUser(cur.resource, next) {
		return cur.id, nil
	}
	if _, e := s.Provisioner.ReplaceUser(ctx, p, cur.id, next); e != nil {
		return "", e
	}
	return cur.id, nil
}

// userResource is the SCIM user for a Graph user, on top of the stored resource when there is one.
func userResource(base map[string]any, u User, oid, upn string) map[string]any {
	res := map[string]any{}
	for k, v := range base {
		switch k {
		case "id", "meta":
		default:
			res[k] = v
		}
	}
	res["userName"] = upn
	res["externalId"] = oid
	res["active"] = u.AccountEnabled == nil || *u.AccountEnabled
	if name := strings.TrimSpace(u.DisplayName); name != "" {
		res["displayName"] = name
	} else {
		delete(res, "displayName")
	}
	res[scim.SchemaEnterpriseUser] = withAttribute(res[scim.SchemaEnterpriseUser], "department", strings.TrimSpace(u.Department))
	res[scim.SchemaDirectoryUser] = withAttribute(res[scim.SchemaDirectoryUser], "orgUnit", OrgUnit(u.OnPremisesDistinguishedName))
	schemas := []any{scim.SchemaUser}
	for _, ext := range []string{scim.SchemaEnterpriseUser, scim.SchemaDirectoryUser} {
		if m, _ := res[ext].(map[string]any); len(m) > 0 {
			schemas = append(schemas, ext)
		} else {
			delete(res, ext)
		}
	}
	res["schemas"] = schemas
	return res
}

func withAttribute(ext any, name, value string) map[string]any {
	out := map[string]any{}
	if m, ok := ext.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	if value == "" {
		delete(out, name)
	} else {
		out[name] = value
	}
	return out
}

// sameUser compares what the pull writes, so an unchanged person costs no write and no audit row.
func sameUser(cur, next map[string]any) bool {
	for _, k := range []string{"userName", "externalId", "active", "displayName", scim.SchemaEnterpriseUser, scim.SchemaDirectoryUser} {
		if !reflect.DeepEqual(normal(cur[k]), normal(next[k])) {
			return false
		}
	}
	return true
}

func normal(v any) any {
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return nil
	}
	return v
}

// OrgUnit is the organisational unit of an on-premises distinguished name: the name with its
// first relative name removed. `CN=Ada\, Lovelace,OU=Sales,DC=contoso,DC=com` is
// `OU=Sales,DC=contoso,DC=com`. A name with no unit is "".
func OrgUnit(dn string) string {
	dn = strings.TrimSpace(dn)
	for i := 0; i < len(dn); i++ {
		switch dn[i] {
		case '\\':
			i++
		case ',':
			return strings.TrimSpace(dn[i+1:])
		}
	}
	return ""
}

// syncGroups replaces the members of every group a team follows that came from Graph.
func (s *Syncer) syncGroups(ctx context.Context, p scim.Principal, entraTenantID string, byObjectID map[string]string, idx userIndex) (int, error) {
	if s.groups == nil {
		return 0, nil
	}
	imported, err := s.groups(ctx, p.TenantID)
	if err != nil {
		return 0, provisionError{err}
	}
	n := 0
	for _, g := range imported {
		members, err := s.Directory.GroupMembers(ctx, entraTenantID, g.ExternalID)
		if IsNotFound(err) {
			// Deleted in Entra: the team keeps the members it had until an admin removes it.
			s.logger().Warn("directory sync: an imported group is gone from the directory", "tenant", p.TenantID, "group", g.ID)
			continue
		}
		if err != nil {
			return n, err
		}
		info, err := s.Directory.Group(ctx, entraTenantID, g.ExternalID)
		if err != nil && !IsNotFound(err) {
			return n, err
		}
		cur, e := s.Provisioner.GetGroup(ctx, p, g.ID, false)
		if e != nil {
			return n, provisionError{e}
		}
		name, _ := cur["displayName"].(string)
		if strings.TrimSpace(info.DisplayName) != "" {
			name = info.DisplayName
		}
		var list []any
		want := map[string]bool{}
		for _, oid := range members {
			id, ok := byObjectID[oid]
			if !ok {
				if r, found := idx.byObjectID[oid]; found {
					id, ok = r.id, true
				}
			}
			if ok && !want[id] {
				want[id] = true
				list = append(list, map[string]any{"value": id})
			}
		}
		if name == cur["displayName"] && sameMembers(cur["members"], want) {
			n++
			continue
		}
		body := map[string]any{"schemas": []any{scim.SchemaGroup}, "displayName": name, "externalId": g.ExternalID, "members": list}
		if _, e := s.Provisioner.ReplaceGroup(ctx, p, g.ID, body); e != nil {
			return n, provisionError{e}
		}
		n++
	}
	return n, nil
}

func sameMembers(cur any, want map[string]bool) bool {
	list, _ := cur.([]any)
	if len(list) != len(want) {
		return false
	}
	for _, e := range list {
		m, _ := e.(map[string]any)
		id, _ := m["value"].(string)
		if !want[strings.ToLower(id)] {
			return false
		}
	}
	return true
}

// ImportedGroup is a provisioned group a team follows and that came from Graph: its SCIM id and
// its Graph object id.
type ImportedGroup struct {
	ID         string
	ExternalID string
}

// WithGroups sets where the pull learns which groups teams follow.
func (s *Syncer) WithGroups(fn func(ctx context.Context, tenantID string) ([]ImportedGroup, error)) *Syncer {
	s.groups = fn
	return s
}

// Runner runs the pull for every tenant that has it on, on a schedule and on demand.
type Runner struct {
	Syncer   *Syncer
	Store    Store
	Interval time.Duration
	Logger   *slog.Logger
	Now      func() time.Time

	kick chan string
}

// Store is the pull's own state.
type Store interface {
	// Targets returns the tenants with the pull on.
	Targets(ctx context.Context) ([]Target, error)
	// Target returns one tenant's pull, ok false when it is off.
	Target(ctx context.Context, tenantID string) (Target, bool, error)
	// Lock holds the tenant's pull for this process; ok is false when another process holds it.
	Lock(ctx context.Context, tenantID string) (unlock func(), ok bool, err error)
	Started(ctx context.Context, tenantID string, at time.Time) error
	Finished(ctx context.Context, tenantID string, at time.Time, r Result) error
	ImportedGroups(ctx context.Context, tenantID string) ([]ImportedGroup, error)
}

// Target is one tenant with the pull on, and the Entra directory it pulls from ("" when the tenant
// has no active Entra connection).
type Target struct {
	TenantID      string
	EntraTenantID string
}

// DefaultInterval is how often every tenant is pulled.
const DefaultInterval = time.Hour

// NewRunner wires the scheduler.
func NewRunner(syncer *Syncer, store Store, logger *slog.Logger) (*Runner, error) {
	if syncer == nil || store == nil {
		return nil, errors.New("graphsync: a syncer and a store are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if syncer.groups == nil {
		syncer.WithGroups(store.ImportedGroups)
	}
	return &Runner{Syncer: syncer, Store: store, Interval: DefaultInterval, Logger: logger, Now: time.Now, kick: make(chan string, 16)}, nil
}

// Trigger asks for one tenant's pull now. It never blocks: a request while the queue is full is
// dropped, and the next scheduled pass covers it.
func (r *Runner) Trigger(tenantID string) bool {
	select {
	case r.kick <- tenantID:
		return true
	default:
		return false
	}
}

// Run pulls every tenant once a minute after it starts and then every Interval, and a tenant
// whenever Trigger names it, until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.kick:
			t, ok, err := r.Store.Target(ctx, id)
			if err != nil {
				r.Logger.Error("directory sync: read target", "tenant", id, "error", err)
				continue
			}
			if ok {
				r.RunTenant(ctx, t)
			}
		case <-timer.C:
			targets, err := r.Store.Targets(ctx)
			if err != nil {
				r.Logger.Error("directory sync: list targets", "error", err)
			}
			for _, t := range targets {
				if ctx.Err() != nil {
					return
				}
				r.RunTenant(ctx, t)
			}
			timer.Reset(r.Interval)
		}
	}
}

// RunTenant runs one tenant's pull under its lock and records the outcome.
func (r *Runner) RunTenant(ctx context.Context, t Target) {
	log := r.Logger.With("tenant", t.TenantID)
	unlock, ok, err := r.Store.Lock(ctx, t.TenantID)
	if err != nil {
		log.Error("directory sync: lock", "error", err)
		return
	}
	if !ok {
		return
	}
	defer unlock()
	if err := r.Store.Started(ctx, t.TenantID, r.Now().UTC()); err != nil {
		log.Error("directory sync: record start", "error", err)
		return
	}
	started := time.Now()
	res := r.Syncer.Sync(ctx, t.TenantID, t.EntraTenantID)
	if err := r.Store.Finished(context.WithoutCancel(ctx), t.TenantID, r.Now().UTC(), res); err != nil {
		log.Error("directory sync: record result", "error", err)
	}
	log.Info("directory sync: pass complete", "users", res.Users, "groups", res.Groups, "retired", res.Retired,
		"failed", res.Failed, "error", res.Error, "duration_ms", time.Since(started).Milliseconds())
}
