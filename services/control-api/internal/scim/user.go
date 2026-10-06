package scim

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/session"
)

// A user's core attributes a provider may send but the stored resource does not take from it: the
// server's own id and meta, the read-only group list, and a password, which is never stored.
var userPatcher = patcher{ignore: map[string]bool{"id": true, "meta": true, "schemas": true, "password": true, "groups": true}}

const departmentPath = SchemaEnterpriseUser + ":department"

// normalizeUser turns a provider's body into the resource that is sealed and served back.
func normalizeUser(body map[string]any) (map[string]any, *Error) {
	res := deepCopy(body)
	for _, k := range []string{"id", "meta", "password", "groups"} {
		deleteKey(res, k)
	}
	if v, ok := getKey(res, "active"); ok {
		b, ok := boolValue(v)
		if !ok {
			return nil, badRequest("invalidValue", "active must be a boolean")
		}
		setKey(res, "active", b)
	}
	if v, ok := getKey(res, "userName"); ok && v != nil {
		if _, isString := v.(string); !isString {
			return nil, badRequest("invalidValue", "userName must be a string")
		}
	}
	// Entra may send a value's primary flag as a string too.
	for _, k := range sortedKeys(res) {
		if arr, ok := res[k].([]any); ok {
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok {
					if v, ok := getKey(m, "primary"); ok {
						if b, ok := boolValue(v); ok {
							setKey(m, "primary", b)
						}
					}
				}
			}
		}
	}
	addSchema(res, SchemaUser)
	if _, ok := getKey(res, SchemaEnterpriseUser); ok {
		addSchema(res, SchemaEnterpriseUser)
	}
	return res, nil
}

// userFacts is what the product reads out of a user resource.
type userFacts struct {
	userName    string
	externalID  string
	active      bool
	displayName string
	department  string
}

func (s *Service) facts(res map[string]any) userFacts {
	f := userFacts{
		userName:   firstString(res, "userName"),
		externalID: firstString(res, "externalId"),
		active:     true,
		department: firstString(res, departmentPath),
	}
	if v, ok := getKey(res, "active"); ok {
		if b, ok := boolValue(v); ok {
			f.active = b
		}
	}
	f.displayName = firstString(res, "displayName")
	if f.displayName == "" {
		f.displayName = firstString(res, "name.formatted")
	}
	if f.displayName == "" {
		f.displayName = strings.TrimSpace(firstString(res, "name.givenName") + " " + firstString(res, "name.familyName"))
	}
	return f
}

var guidPattern = regexp.MustCompile(`^\{?([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\}?$`)

// objectID returns the externalId as a bare GUID when it is one: Entra admins map objectId to
// externalId, and a device that cannot resolve a UPN derives the person's ref from that object id.
func objectID(externalID string) (string, bool) {
	m := guidPattern.FindStringSubmatch(strings.TrimSpace(externalID))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// canonicalRef is a new user's user_ref: their userName's upn-ref, unless that ref already names a
// person (a userName reused after its earlier holder was renamed). Then the newcomer gets a ref of
// their own, derived from the SCIM id under the `acct` kind with a value no device produces, and the
// upn-ref is moved to them as an alias, so the devices of whoever holds the userName now resolve to
// whoever holds it now, and the earlier holder's history keeps its own ref.
func canonicalRef(tx Tx, key []byte, userName, id string) (string, error) {
	cand, err := protocol.DeriveUserRef(key, protocol.UserRefUPN, userName)
	if err != nil {
		return "", err
	}
	owner, err := tx.UserRefOwner(cand)
	if err != nil {
		return "", err
	}
	if owner == "" || owner == id {
		return cand, nil
	}
	return protocol.DeriveUserRef(key, protocol.UserRefAccount, "scim:"+id)
}

// aliases are the device-derived refs that resolve to this person now: their current userName's
// upn-ref and, when externalId is a GUID, its oid-ref. Earlier userNames' refs were written when
// those were current and stay until another person takes the name.
func aliases(key []byte, f userFacts) ([]string, error) {
	upn, err := protocol.DeriveUserRef(key, protocol.UserRefUPN, f.userName)
	if err != nil {
		return nil, err
	}
	out := []string{upn}
	if oid, ok := objectID(f.externalID); ok {
		ref, err := protocol.DeriveUserRef(key, protocol.UserRefOID, oid)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, nil
}

// writeIdentity writes the person's aliases and their ops.user_dim row.
func (s *Service) writeIdentity(tx Tx, tenantID, identity string, key []byte, row UserRow, f userFacts, at time.Time) error {
	refs, err := aliases(key, f)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := tx.PutAlias(ref, row.UserRef, at); err != nil {
			return err
		}
	}
	directoryID := f.externalID
	if directoryID == "" {
		directoryID = f.userName
	}
	sealed, err := s.cipher.Seal(tenantID, directoryID)
	if err != nil {
		return err
	}
	dim := UserDimRow{
		UserRef:              row.UserRef,
		DirectoryObjectIDEnc: sealed,
		Department:           nullable(f.department),
		Status:               DimInactive,
		SyncedAt:             at,
	}
	if f.active {
		dim.Status = DimActive
	}
	// A tenant that chose hashed device identities stores no clear name from the directory either.
	if identity == "clear" {
		dim.DisplayName = nullable(f.displayName)
	}
	return tx.UpsertUserDim(dim)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Service) renderUser(row UserRow, res map[string]any) map[string]any {
	out := deepCopy(res)
	out["id"] = row.ID
	out["meta"] = meta("User", row.CreatedAt, row.UpdatedAt, s.location("Users", row.ID))
	return out
}

// CreateUser is POST /Users.
func (s *Service) CreateUser(ctx context.Context, p Principal, body map[string]any) (map[string]any, *Error) {
	res, e := normalizeUser(body)
	if e != nil {
		return nil, e
	}
	if _, ok := getKey(res, "active"); !ok {
		setKey(res, "active", true)
	}
	f := s.facts(res)
	if f.userName == "" {
		return nil, badRequest("invalidValue", "userName is required")
	}
	key, err := s.keys.Key(ctx, p.TenantID)
	if err != nil {
		return nil, internal(err)
	}
	id, err := session.NewUUID()
	if err != nil {
		return nil, internal(err)
	}
	now := s.now()
	var row UserRow
	err = s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		identity, err := tx.DeviceIdentity()
		if err != nil {
			return err
		}
		unh := LookupHash(key, f.userName)
		existing, err := tx.UsersByUserNameHash(unh)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			return ErrConflict
		}
		canonical, err := canonicalRef(tx, key, f.userName, id)
		if err != nil {
			return err
		}
		sealed, err := s.sealResource(p.TenantID, res)
		if err != nil {
			return err
		}
		row = UserRow{
			ID: id, UserNameHash: unh, ExternalIDHash: optionalHash(key, f.externalID),
			ResourceEnc: sealed, UserRef: canonical, Active: f.active, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.InsertUser(row); err != nil {
			return err
		}
		if err := s.writeIdentity(tx, p.TenantID, identity, key, row, f, now); err != nil {
			return err
		}
		return tx.Audit(s.auditEntry(p, "scim.user.create", "scim_user", id, canonical,
			map[string]any{"active": f.active, "has_external_id": f.externalID != ""}, now))
	})
	if e := txError(err, "a user with this userName already exists"); e != nil {
		return nil, e
	}
	return s.renderUser(row, res), nil
}

// GetUser is GET /Users/{id}.
func (s *Service) GetUser(ctx context.Context, p Principal, id string) (map[string]any, *Error) {
	if !isUUID(id) {
		return nil, notFound("user")
	}
	var out map[string]any
	err := s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		row, err := tx.User(strings.ToLower(id), false)
		if errors.Is(err, ErrNotFound) {
			return errAbort{notFound("user")}
		}
		if err != nil {
			return err
		}
		res, err := s.openResource(p.TenantID, row.ResourceEnc)
		if err != nil {
			return err
		}
		out = s.renderUser(row, res)
		return nil
	})
	if e := txError(err, ""); e != nil {
		return nil, e
	}
	return out, nil
}

// ListUsers is GET /Users, with or without a filter.
func (s *Service) ListUsers(ctx context.Context, p Principal, q ListQuery) (*ListResponse, *Error) {
	start, count := q.page()
	var f expr
	if strings.TrimSpace(q.Filter) != "" {
		var err error
		if f, err = parseFilter(q.Filter); err != nil {
			return nil, badRequest("invalidFilter", "the filter is not a SCIM filter expression")
		}
	}
	var key []byte
	attr, value, indexed := "", "", false
	if f != nil {
		if attr, value, indexed = indexTerm(f, "userName", "externalId", "id"); !indexed {
			return nil, badRequest("invalidFilter", "a user filter must include userName eq, externalId eq or id eq")
		}
		if attr != "id" {
			var err error
			if key, err = s.keys.Key(ctx, p.TenantID); err != nil {
				return nil, internal(err)
			}
		}
	}
	var resp *ListResponse
	err := s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		if f == nil {
			total, err := tx.CountUsers()
			if err != nil {
				return err
			}
			var page []map[string]any
			if count > 0 {
				rows, err := tx.ListUsers(start-1, count)
				if err != nil {
					return err
				}
				for _, row := range rows {
					res, err := s.openResource(p.TenantID, row.ResourceEnc)
					if err != nil {
						return err
					}
					page = append(page, s.renderUser(row, res))
				}
			}
			resp = listResponse(total, start, page)
			return nil
		}
		var rows []UserRow
		var err error
		switch attr {
		case "userName":
			rows, err = tx.UsersByUserNameHash(LookupHash(key, value))
		case "externalId":
			rows, err = tx.UsersByExternalIDHash(LookupHash(key, value))
		case "id":
			if isUUID(value) {
				var row UserRow
				row, err = tx.User(strings.ToLower(value), false)
				if errors.Is(err, ErrNotFound) {
					err = nil
				} else if err == nil {
					rows = []UserRow{row}
				}
			}
		}
		if err != nil {
			return err
		}
		var matched []map[string]any
		for _, row := range rows {
			res, err := s.openResource(p.TenantID, row.ResourceEnc)
			if err != nil {
				return err
			}
			rendered := s.renderUser(row, res)
			if f.match(rendered) {
				matched = append(matched, rendered)
			}
		}
		resp = listResponse(len(matched), start, pageOf(matched, start, count))
		return nil
	})
	if e := txError(err, ""); e != nil {
		return nil, e
	}
	return resp, nil
}

// ReplaceUser is PUT /Users/{id}. An `active` the body leaves out keeps its value: a provider that
// omits it has not asked to reactivate or deactivate anyone.
func (s *Service) ReplaceUser(ctx context.Context, p Principal, id string, body map[string]any) (map[string]any, *Error) {
	next, e := normalizeUser(body)
	if e != nil {
		return nil, e
	}
	return s.mutateUser(ctx, p, id, "", func(old map[string]any) (map[string]any, *Error) {
		if _, ok := getKey(next, "active"); !ok {
			if v, ok := getKey(old, "active"); ok {
				setKey(next, "active", v)
			}
		}
		return next, nil
	})
}

// PatchUser is PATCH /Users/{id}.
func (s *Service) PatchUser(ctx context.Context, p Principal, id string, body map[string]any) (map[string]any, *Error) {
	ops, e := parsePatch(body)
	if e != nil {
		return nil, e
	}
	return s.mutateUser(ctx, p, id, "", func(old map[string]any) (map[string]any, *Error) {
		cur := deepCopy(old)
		if e := userPatcher.applyAll(cur, ops); e != nil {
			return nil, e
		}
		return normalizeUser(cur)
	})
}

// DeleteUser is DELETE /Users/{id}: the person is retired, never deleted (see the package comment).
func (s *Service) DeleteUser(ctx context.Context, p Principal, id string) *Error {
	_, e := s.mutateUser(ctx, p, id, "scim.user.retire", func(old map[string]any) (map[string]any, *Error) {
		cur := deepCopy(old)
		setKey(cur, "active", false)
		return cur, nil
	})
	return e
}

// mutateUser is the one write path for an existing user: lock the row, change the resource, re-key
// the lookups, write the aliases and the ops.user_dim row, audit. The canonical user_ref never
// changes here; a new userName only adds an alias.
func (s *Service) mutateUser(ctx context.Context, p Principal, id, action string, change func(old map[string]any) (map[string]any, *Error)) (map[string]any, *Error) {
	if !isUUID(id) {
		return nil, notFound("user")
	}
	id = strings.ToLower(id)
	key, err := s.keys.Key(ctx, p.TenantID)
	if err != nil {
		return nil, internal(err)
	}
	now := s.now()
	var out map[string]any
	err = s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		identity, err := tx.DeviceIdentity()
		if err != nil {
			return err
		}
		cur, err := tx.User(id, true)
		if errors.Is(err, ErrNotFound) {
			return errAbort{notFound("user")}
		}
		if err != nil {
			return err
		}
		old, err := s.openResource(p.TenantID, cur.ResourceEnc)
		if err != nil {
			return err
		}
		next, e := change(old)
		if e != nil {
			return errAbort{e}
		}
		f := s.facts(next)
		if f.userName == "" {
			return errAbort{badRequest("invalidValue", "userName is required")}
		}
		unh := LookupHash(key, f.userName)
		renamed := !bytes.Equal(unh, cur.UserNameHash)
		if renamed {
			others, err := tx.UsersByUserNameHash(unh)
			if err != nil {
				return err
			}
			for _, o := range others {
				if o.ID != id {
					return ErrConflict
				}
			}
		}
		sealed, err := s.sealResource(p.TenantID, next)
		if err != nil {
			return err
		}
		row := cur
		row.UserNameHash = unh
		row.ExternalIDHash = optionalHash(key, f.externalID)
		row.ResourceEnc = sealed
		row.Active = f.active
		row.UpdatedAt = now
		if err := tx.UpdateUser(row); err != nil {
			return err
		}
		if err := s.writeIdentity(tx, p.TenantID, identity, key, row, f, now); err != nil {
			return err
		}
		act := action
		if act == "" {
			act = "scim.user.update"
			switch {
			case cur.Active && !f.active:
				act = "scim.user.deactivate"
			case !cur.Active && f.active:
				act = "scim.user.reactivate"
			}
		}
		detail := map[string]any{"active": f.active, "changed": changedAttributes(old, next)}
		if renamed {
			detail["user_name_changed"] = true
		}
		if err := tx.Audit(s.auditEntry(p, act, "scim_user", id, row.UserRef, detail, now)); err != nil {
			return err
		}
		out = s.renderUser(row, next)
		return nil
	})
	if e := txError(err, "a user with this userName already exists"); e != nil {
		return nil, e
	}
	return out, nil
}

// changedAttributes names, never values, the attributes a write changed, for the audit row. An
// extension's attributes are named with their URN.
func changedAttributes(old, next map[string]any) []string {
	flat := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			if strings.EqualFold(k, "schemas") {
				continue
			}
			if obj, ok := v.(map[string]any); ok && hasPrefixFold(k, "urn:") {
				for sk, sv := range obj {
					out[strings.ToLower(k+":"+sk)] = sv
				}
				continue
			}
			out[strings.ToLower(k)] = v
		}
		return out
	}
	a, b := flat(old), flat(next)
	seen := map[string]bool{}
	var changed []string
	for k, v := range a {
		if bv, ok := b[k]; !ok || !reflect.DeepEqual(v, bv) {
			changed = append(changed, k)
		}
		seen[k] = true
	}
	for k := range b {
		if !seen[k] {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	if changed == nil {
		changed = []string{}
	}
	return changed
}

// Person is what the identity service needs about a signed-in person SCIM knows: their canonical
// user_ref (ops.auth_session.user_ref) and whether the identity provider still has them active.
type Person struct {
	UserRef string
	Active  bool
}

// Person looks a person up by userName (the UPN or login the provider sends), then by externalId
// (Entra's object id when the admin mapped it). found is false for someone SCIM never provisioned,
// who is then governed by their sign-in alone.
func (s *Service) Person(ctx context.Context, tenantID, userName, externalID string) (Person, bool, error) {
	key, err := s.keys.Key(ctx, tenantID)
	if err != nil {
		return Person{}, false, err
	}
	var out Person
	var found bool
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var rows []UserRow
		var err error
		if strings.TrimSpace(userName) != "" {
			if rows, err = tx.UsersByUserNameHash(LookupHash(key, userName)); err != nil {
				return err
			}
		}
		if len(rows) == 0 && strings.TrimSpace(externalID) != "" {
			if rows, err = tx.UsersByExternalIDHash(LookupHash(key, externalID)); err != nil {
				return err
			}
		}
		if len(rows) > 0 {
			out, found = Person{UserRef: rows[0].UserRef, Active: rows[0].Active}, true
		}
		return nil
	})
	return out, found, err
}
