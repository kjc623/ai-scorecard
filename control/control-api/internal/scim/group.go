package scim

import (
	"context"
	"errors"
	"strings"

	"github.com/shadow-ai-capture/control-api/internal/session"
)

// Groups are stored so an identity provider that pushes them is answered faithfully; no product
// decision reads them. A group is stored as its name, its externalId and its members (ops.scim_group,
// ops.scim_group_member), not as a sealed resource: a group name is the customer's organisation, not a
// person, and its membership is a list of this provider's own user ids.

var groupPatcher = patcher{ignore: map[string]bool{"id": true, "meta": true, "schemas": true}}

// groupResource is the representation a PATCH is applied to: the stored columns as a SCIM object,
// so a group PATCH runs through the same engine as a user's, Entra's members-with-a-value removal and
// all.
func groupResource(g GroupRow, members []string) map[string]any {
	res := map[string]any{"schemas": []any{SchemaGroup}, "displayName": g.DisplayName}
	if g.ExternalID != "" {
		res["externalId"] = g.ExternalID
	}
	list := make([]any, 0, len(members))
	for _, m := range members {
		list = append(list, map[string]any{"value": m})
	}
	res["members"] = list
	return res
}

func (s *Service) renderGroup(g GroupRow, members []string, excludeMembers bool) map[string]any {
	res := map[string]any{
		"schemas":     []any{SchemaGroup},
		"id":          g.ID,
		"displayName": g.DisplayName,
		"meta":        meta("Group", g.CreatedAt, g.UpdatedAt, s.location("Groups", g.ID)),
	}
	if g.ExternalID != "" {
		res["externalId"] = g.ExternalID
	}
	if !excludeMembers {
		list := make([]any, 0, len(members))
		for _, m := range members {
			entry := map[string]any{"value": m, "type": "User"}
			if loc := s.location("Users", m); loc != "" {
				entry["$ref"] = loc
			}
			list = append(list, entry)
		}
		res["members"] = list
	}
	return res
}

// memberIDs reads the user ids out of a members attribute. A value that is not one of this
// provider's ids cannot name a member, so it is dropped here rather than failing the whole request.
func memberIDs(res map[string]any) []string {
	raw, _ := getKey(res, "members")
	list, _ := raw.([]any)
	seen := map[string]bool{}
	var out []string
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		v, _ := getKey(m, "value")
		id, _ := v.(string)
		id = strings.ToLower(strings.TrimSpace(id))
		if !session.IsUUID(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func groupFields(res map[string]any) (displayName, externalID string, e *Error) {
	v, _ := getKey(res, "displayName")
	name, _ := v.(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", badRequest("invalidValue", "displayName is required")
	}
	return name, firstString(res, "externalId"), nil
}

// CreateGroup is POST /Groups.
func (s *Service) CreateGroup(ctx context.Context, p Principal, body map[string]any) (map[string]any, *Error) {
	name, externalID, e := groupFields(body)
	if e != nil {
		return nil, e
	}
	id, err := session.NewUUID()
	if err != nil {
		return nil, internal(err)
	}
	now := s.now()
	g := GroupRow{ID: id, DisplayName: name, ExternalID: externalID, CreatedAt: now, UpdatedAt: now}
	var members []string
	err = s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		if err := tx.InsertGroup(g); err != nil {
			return err
		}
		requested := memberIDs(body)
		for _, uid := range requested {
			added, err := tx.AddMember(id, uid)
			if err != nil {
				return err
			}
			if added {
				members = append(members, uid)
			}
		}
		return tx.Audit(s.auditEntry(p, "scim.group.create", "scim_group", id, "",
			map[string]any{"members_added": len(members), "members_unknown": len(requested) - len(members)}, now))
	})
	if e := txError(err, "the group could not be created"); e != nil {
		return nil, e
	}
	return s.renderGroup(g, members, false), nil
}

// GetGroup is GET /Groups/{id}.
func (s *Service) GetGroup(ctx context.Context, p Principal, id string, excludeMembers bool) (map[string]any, *Error) {
	if !isUUID(id) {
		return nil, notFound("group")
	}
	var out map[string]any
	err := s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		g, err := tx.Group(strings.ToLower(id), false)
		if errors.Is(err, ErrNotFound) {
			return errAbort{notFound("group")}
		}
		if err != nil {
			return err
		}
		var members []string
		if !excludeMembers {
			if members, err = tx.Members(g.ID); err != nil {
				return err
			}
		}
		out = s.renderGroup(g, members, excludeMembers)
		return nil
	})
	if e := txError(err, ""); e != nil {
		return nil, e
	}
	return out, nil
}

// ListGroups is GET /Groups, with or without a filter.
func (s *Service) ListGroups(ctx context.Context, p Principal, q ListQuery) (*ListResponse, *Error) {
	start, count := q.page()
	var f expr
	if strings.TrimSpace(q.Filter) != "" {
		var err error
		if f, err = parseFilter(q.Filter); err != nil {
			return nil, badRequest("invalidFilter", "the filter is not a SCIM filter expression")
		}
	}
	attr, value, indexed := "", "", false
	if f != nil {
		if attr, value, indexed = indexTerm(f, "displayName", "externalId", "id"); !indexed {
			return nil, badRequest("invalidFilter", "a group filter must include displayName eq, externalId eq or id eq")
		}
	}
	var resp *ListResponse
	err := s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		render := func(g GroupRow) (map[string]any, error) {
			var members []string
			if !q.ExcludeMembers {
				var err error
				if members, err = tx.Members(g.ID); err != nil {
					return nil, err
				}
			}
			return s.renderGroup(g, members, q.ExcludeMembers), nil
		}
		if f == nil {
			total, err := tx.CountGroups()
			if err != nil {
				return err
			}
			var page []map[string]any
			if count > 0 {
				rows, err := tx.ListGroups(start-1, count)
				if err != nil {
					return err
				}
				for _, g := range rows {
					r, err := render(g)
					if err != nil {
						return err
					}
					page = append(page, r)
				}
			}
			resp = listResponse(total, start, page)
			return nil
		}
		var rows []GroupRow
		var err error
		switch attr {
		case "displayName":
			rows, err = tx.GroupsByDisplayName(value)
		case "externalId":
			rows, err = tx.GroupsByExternalID(value)
		case "id":
			if isUUID(value) {
				var g GroupRow
				g, err = tx.Group(strings.ToLower(value), false)
				if errors.Is(err, ErrNotFound) {
					err = nil
				} else if err == nil {
					rows = []GroupRow{g}
				}
			}
		}
		if err != nil {
			return err
		}
		var matched []map[string]any
		for _, g := range rows {
			// The filter is evaluated on the full representation, members included, then the
			// members are dropped if the provider asked: a filter may name a member.
			full, err := tx.Members(g.ID)
			if err != nil {
				return err
			}
			if !f.match(s.renderGroup(g, full, false)) {
				continue
			}
			matched = append(matched, s.renderGroup(g, full, q.ExcludeMembers))
		}
		resp = listResponse(len(matched), start, pageOf(matched, start, count))
		return nil
	})
	if e := txError(err, ""); e != nil {
		return nil, e
	}
	return resp, nil
}

// PatchGroup is PATCH /Groups/{id}: a rename, an externalId, and member adds and removes in any of
// the shapes Entra and Okta send.
func (s *Service) PatchGroup(ctx context.Context, p Principal, id string, body map[string]any) *Error {
	ops, e := parsePatch(body)
	if e != nil {
		return e
	}
	_, e = s.mutateGroup(ctx, p, id, func(cur map[string]any) (map[string]any, *Error) {
		if e := groupPatcher.applyAll(cur, ops); e != nil {
			return nil, e
		}
		return cur, nil
	})
	return e
}

// ReplaceGroup is PUT /Groups/{id}.
func (s *Service) ReplaceGroup(ctx context.Context, p Principal, id string, body map[string]any) (map[string]any, *Error) {
	return s.mutateGroup(ctx, p, id, func(map[string]any) (map[string]any, *Error) {
		return deepCopy(body), nil
	})
}

func (s *Service) mutateGroup(ctx context.Context, p Principal, id string, change func(cur map[string]any) (map[string]any, *Error)) (map[string]any, *Error) {
	if !isUUID(id) {
		return nil, notFound("group")
	}
	id = strings.ToLower(id)
	now := s.now()
	var out map[string]any
	err := s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		g, err := tx.Group(id, true)
		if errors.Is(err, ErrNotFound) {
			return errAbort{notFound("group")}
		}
		if err != nil {
			return err
		}
		before, err := tx.Members(id)
		if err != nil {
			return err
		}
		next, e := change(groupResource(g, before))
		if e != nil {
			return errAbort{e}
		}
		name, externalID, e := groupFields(next)
		if e != nil {
			return errAbort{e}
		}
		want := memberIDs(next)
		wanted, had := map[string]bool{}, map[string]bool{}
		for _, m := range want {
			wanted[m] = true
		}
		for _, m := range before {
			had[m] = true
		}
		added, removed := 0, 0
		for _, m := range before {
			if !wanted[m] {
				ok, err := tx.RemoveMember(id, m)
				if err != nil {
					return err
				}
				if ok {
					removed++
				}
			}
		}
		for _, m := range want {
			if !had[m] {
				ok, err := tx.AddMember(id, m)
				if err != nil {
					return err
				}
				if ok {
					added++
				}
			}
		}
		renamed := name != g.DisplayName
		g.DisplayName, g.ExternalID, g.UpdatedAt = name, externalID, now
		if err := tx.UpdateGroup(g); err != nil {
			return err
		}
		if err := tx.Audit(s.auditEntry(p, "scim.group.update", "scim_group", id, "",
			map[string]any{"renamed": renamed, "members_added": added, "members_removed": removed}, now)); err != nil {
			return err
		}
		members, err := tx.Members(id)
		if err != nil {
			return err
		}
		out = s.renderGroup(g, members, false)
		return nil
	})
	if e := txError(err, ""); e != nil {
		return nil, e
	}
	return out, nil
}

// DeleteGroup is DELETE /Groups/{id}. Unlike a person, a group is deleted: it holds no history of its
// own, and a group a role mapping still names should stop granting the moment the customer deletes it.
func (s *Service) DeleteGroup(ctx context.Context, p Principal, id string) *Error {
	if !isUUID(id) {
		return notFound("group")
	}
	id = strings.ToLower(id)
	err := s.store.InTenant(ctx, p.TenantID, func(tx Tx) error {
		if _, err := tx.Group(id, true); errors.Is(err, ErrNotFound) {
			return errAbort{notFound("group")}
		} else if err != nil {
			return err
		}
		members, err := tx.Members(id)
		if err != nil {
			return err
		}
		if err := tx.DeleteGroup(id); err != nil {
			return err
		}
		return tx.Audit(s.auditEntry(p, "scim.group.delete", "scim_group", id, "",
			map[string]any{"members_removed": len(members)}, s.now()))
	})
	return txError(err, "")
}
