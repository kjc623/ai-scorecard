// Package directoryadmin is the admin API for the people side of a tenant: the pull of its
// directory from Microsoft Graph, and its teams.
//
// A team is created in the console with members an admin picks, or follows the directory: a group
// (provisioned by SCIM, or imported from Graph by its object id and kept current by the pull), a
// department, or an organisational unit and the units below it. Usage is reported per team by the
// aggregate job from ops.v_team_member.
//
// Every route requires a product access token carrying the admin role; the tenant is the token's.
// Every write is audited with the admin as actor, in the write's own transaction.
package directoryadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deploy"
	"github.com/shadow-ai-capture/control-api/internal/graphsync"
	"github.com/shadow-ai-capture/control-api/internal/scim"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Team sources: exactly the values ops.team.source admits.
const (
	SourceConsole    = "console"
	SourceGroup      = "group"
	SourceDepartment = "department"
	SourceOrgUnit    = "org_unit"
)

// Store errors.
var (
	ErrUnknownTenant = errors.New("directoryadmin: tenant unknown")
	ErrNotFound      = errors.New("directoryadmin: not found")
	ErrNameTaken     = errors.New("directoryadmin: a team already has this name")
	ErrNotConsole    = errors.New("directoryadmin: the team's members follow the directory")
	ErrNoEntra       = errors.New("directoryadmin: no active Entra connection")
)

// SyncState is the tenant's directory pull.
type SyncState struct {
	EnabledBy       string     `json:"enabled_by"`
	EnabledAt       time.Time  `json:"enabled_at"`
	LastStartedAt   *time.Time `json:"last_started_at"`
	LastCompletedAt *time.Time `json:"last_completed_at"`
	LastStatus      string     `json:"last_status"`
	LastError       string     `json:"last_error"`
	UsersSynced     *int64     `json:"users_synced"`
	GroupsSynced    *int64     `json:"groups_synced"`
}

// Status is the directory as the admin page shows it.
type Status struct {
	EntraConnected bool
	People         int64
	Groups         int64
	Sync           *SyncState
}

// Team is one team as the admin page lists it.
type Team struct {
	ID              string
	Name            string
	Source          string
	GroupID         string
	GroupName       string
	GroupExternalID string
	MatchValue      string
	CreatedBy       string
	CreatedAt       time.Time
	Members         int64
}

// Member is one person in a team.
type Member struct {
	UserRef    string
	Name       string
	Department string
}

// Value is a department or an organisational unit, with how many active people it holds.
type Value struct {
	Value  string
	People int64
}

// ProvisionedGroup is a group the product holds.
type ProvisionedGroup struct {
	ID         string
	Name       string
	ExternalID string
	Members    int64
}

// Audit is one write's audit row.
type Audit struct {
	Actor      string
	Action     string
	ObjectType string
	ObjectID   string
	Detail     map[string]any
	At         time.Time
}

// Store is the persistence seam. Every method is tenant-scoped and returns ErrUnknownTenant for a
// tenant that does not exist.
type Store interface {
	Status(ctx context.Context, tenant string) (Status, error)
	EntraTenant(ctx context.Context, tenant string) (string, error)
	// SetSync turns the pull on or off; on needs an active Entra connection (ErrNoEntra).
	SetSync(ctx context.Context, tenant string, on bool, a Audit) error
	Teams(ctx context.Context, tenant string) ([]Team, error)
	// CreateTeam returns ErrNameTaken for a name in use, ErrNotFound for an unknown group.
	CreateTeam(ctx context.Context, tenant string, t Team, members []string, a Audit) error
	RenameTeam(ctx context.Context, tenant, id, name string, a Audit) error
	DeleteTeam(ctx context.Context, tenant, id string, a Audit) error
	// ChangeMembers returns ErrNotConsole for a team whose members follow the directory.
	ChangeMembers(ctx context.Context, tenant, id string, add, remove []string, a Audit) error
	TeamMembers(ctx context.Context, tenant, id string, limit int) ([]Member, error)
	DirectoryValues(ctx context.Context, tenant, kind string, limit int) ([]Value, error)
	Groups(ctx context.Context, tenant string, limit int) ([]ProvisionedGroup, error)
	GroupByExternalID(ctx context.Context, tenant, externalID string) (string, bool, error)
	Audit(ctx context.Context, tenant string, a Audit) error
}

// GroupDirectory finds groups in a customer's Entra directory. *graphsync.Client implements it.
type GroupDirectory interface {
	SearchGroups(ctx context.Context, entraTenantID, query string) ([]graphsync.Group, error)
	Group(ctx context.Context, entraTenantID, groupID string) (graphsync.Group, error)
}

// GroupProvisioner creates a provisioned group. *scim.Service implements it.
type GroupProvisioner interface {
	CreateGroup(ctx context.Context, p scim.Principal, body map[string]any) (map[string]any, *scim.Error)
}

// Trigger asks for one tenant's pull now. *graphsync.Runner implements it.
type Trigger interface {
	Trigger(tenantID string) bool
}

// Config is the handler's collaborators beyond the store. Directory, Groups and Sync are nil on a
// deployment with no Entra application; the pull's routes then answer 503.
type Config struct {
	Directory GroupDirectory
	Groups    GroupProvisioner
	Sync      Trigger
	Now       func() time.Time
	Logger    *slog.Logger
}

// Handler serves the routes.
type Handler struct {
	store Store
	auth  deploy.Authenticator
	cfg   Config
}

// NewHandler builds the handler. The store and authenticator are required.
func NewHandler(st Store, auth deploy.Authenticator, cfg Config) (*Handler, error) {
	if st == nil {
		return nil, errors.New("directoryadmin: store is required")
	}
	if auth == nil {
		return nil, errors.New("directoryadmin: an authenticator is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Handler{store: st, auth: auth, cfg: cfg}, nil
}

// Register adds the routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/v1/directory", h.handleStatus)
	mux.HandleFunc("PUT /admin/v1/directory/sync", h.handleSetSync)
	mux.HandleFunc("POST /admin/v1/directory/sync/run", h.handleRunSync)
	mux.HandleFunc("GET /admin/v1/directory/groups", h.handleGroups)
	mux.HandleFunc("GET /admin/v1/directory/values", h.handleValues)
	mux.HandleFunc("GET /admin/v1/teams", h.handleTeams)
	mux.HandleFunc("POST /admin/v1/teams", h.handleCreateTeam)
	mux.HandleFunc("PATCH /admin/v1/teams/{id}", h.handleRenameTeam)
	mux.HandleFunc("DELETE /admin/v1/teams/{id}", h.handleDeleteTeam)
	mux.HandleFunc("GET /admin/v1/teams/{id}/members", h.handleMembers)
	mux.HandleFunc("POST /admin/v1/teams/{id}/members", h.handleChangeMembers)
}

// Bounds on what one request names or returns.
const (
	maxBody         = 64 << 10
	maxNameLen      = 120
	maxMatchLen     = 1024
	maxMemberChange = 500
	listLimit       = 500
)

var userRefPattern = regexp.MustCompile(`^u_[0-9a-f]{32}$`)

// --- the directory pull ------------------------------------------------------------------------

type statusJSON struct {
	EntraConnected bool       `json:"entra_connected"`
	People         int64      `json:"people"`
	Groups         int64      `json:"groups"`
	GraphAvailable bool       `json:"graph_available"`
	Sync           *SyncState `json:"sync"`
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	st, err := h.store.Status(r.Context(), p.Tenant)
	if h.storeFailed(w, err, "directory status") {
		return
	}
	h.writeJSON(w, http.StatusOK, statusJSON{
		EntraConnected: st.EntraConnected, People: st.People, Groups: st.Groups,
		GraphAvailable: h.cfg.Directory != nil && h.cfg.Sync != nil, Sync: st.Sync,
	})
}

func (h *Handler) handleSetSync(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "enabled is required"))
		return
	}
	if *req.Enabled && h.cfg.Sync == nil {
		h.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable, "this deployment has no Microsoft Entra application, so it cannot read a directory"))
		return
	}
	action := "directory_sync.disable"
	if *req.Enabled {
		action = "directory_sync.enable"
	}
	now := h.cfg.Now().UTC()
	err := h.store.SetSync(r.Context(), p.Tenant, *req.Enabled, h.audit(p, action, "directory_sync", p.Tenant, now, nil))
	if errors.Is(err, ErrNoEntra) {
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeNoEntraConnection, "reading the directory needs an active Microsoft Entra connection for this tenant"))
		return
	}
	if h.storeFailed(w, err, "set directory sync") {
		return
	}
	if *req.Enabled {
		h.cfg.Sync.Trigger(p.Tenant)
	}
	h.cfg.Logger.Info("control: directory sync set", "tenant", p.Tenant, "enabled", *req.Enabled)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleRunSync(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	if h.cfg.Sync == nil {
		h.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable, "this deployment has no Microsoft Entra application, so it cannot read a directory"))
		return
	}
	st, err := h.store.Status(r.Context(), p.Tenant)
	if h.storeFailed(w, err, "directory status") {
		return
	}
	if st.Sync == nil {
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeDirectorySyncOff, "turn on reading the directory from Microsoft Entra first"))
		return
	}
	now := h.cfg.Now().UTC()
	if err := h.store.Audit(r.Context(), p.Tenant, h.audit(p, "directory_sync.run", "directory_sync", p.Tenant, now, nil)); h.storeFailed(w, err, "audit sync run") {
		return
	}
	queued := h.cfg.Sync.Trigger(p.Tenant)
	h.writeJSON(w, http.StatusAccepted, map[string]bool{"queued": queued})
}

type groupJSON struct {
	ID         string `json:"id,omitempty"`
	ObjectID   string `json:"object_id,omitempty"`
	Name       string `json:"name"`
	Members    *int64 `json:"members"`
	Imported   bool   `json:"imported"`
	Directory  string `json:"directory"`
	Descriptor string `json:"description,omitempty"`
}

// handleGroups lists the groups a team can follow: every provisioned group, and, with q, the
// matching groups in the customer's Entra directory.
func (h *Handler) handleGroups(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	provisioned, err := h.store.Groups(r.Context(), p.Tenant, listLimit)
	if h.storeFailed(w, err, "list groups") {
		return
	}
	out := struct {
		Provisioned []groupJSON `json:"provisioned"`
		Directory   []groupJSON `json:"directory"`
	}{Provisioned: []groupJSON{}, Directory: []groupJSON{}}
	known := map[string]bool{}
	for _, g := range provisioned {
		n := g.Members
		out.Provisioned = append(out.Provisioned, groupJSON{ID: g.ID, ObjectID: strings.ToLower(g.ExternalID), Name: g.Name, Members: &n, Imported: true, Directory: "provisioned"})
		if g.ExternalID != "" {
			known[strings.ToLower(g.ExternalID)] = true
		}
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q != "" {
		if len(q) > 120 {
			h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "the search is longer than 120 characters"))
			return
		}
		tid, ok := h.graphTenant(w, r, p)
		if !ok {
			return
		}
		found, err := h.cfg.Directory.SearchGroups(r.Context(), tid, q)
		if err != nil {
			h.graphFailed(w, err)
			return
		}
		for _, g := range found {
			oid := strings.ToLower(g.ID)
			out.Directory = append(out.Directory, groupJSON{ObjectID: oid, Name: g.DisplayName, Descriptor: g.Description, Imported: known[oid], Directory: "entra"})
		}
	}
	h.writeJSON(w, http.StatusOK, out)
}

func (h *Handler) handleValues(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != SourceDepartment && kind != SourceOrgUnit {
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest, "kind must be department or org_unit",
			map[string]any{"supported": []string{SourceDepartment, SourceOrgUnit}}))
		return
	}
	values, err := h.store.DirectoryValues(r.Context(), p.Tenant, kind, listLimit)
	if h.storeFailed(w, err, "directory values") {
		return
	}
	type valueJSON struct {
		Value  string `json:"value"`
		People int64  `json:"people"`
	}
	out := make([]valueJSON, 0, len(values))
	for _, v := range values {
		out = append(out, valueJSON(v))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "values": out})
}

// --- teams -------------------------------------------------------------------------------------

type teamJSON struct {
	ID         string    `json:"team_id"`
	Name       string    `json:"name"`
	Source     string    `json:"source"`
	GroupID    string    `json:"group_id,omitempty"`
	GroupName  string    `json:"group_name,omitempty"`
	ObjectID   string    `json:"group_object_id,omitempty"`
	MatchValue string    `json:"match_value,omitempty"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	Members    int64     `json:"members"`
}

func toJSON(t Team) teamJSON {
	return teamJSON{ID: t.ID, Name: t.Name, Source: t.Source, GroupID: t.GroupID, GroupName: t.GroupName,
		ObjectID: strings.ToLower(t.GroupExternalID), MatchValue: t.MatchValue, CreatedBy: t.CreatedBy,
		CreatedAt: t.CreatedAt, Members: t.Members}
}

func (h *Handler) handleTeams(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	teams, err := h.store.Teams(r.Context(), p.Tenant)
	if h.storeFailed(w, err, "list teams") {
		return
	}
	out := make([]teamJSON, 0, len(teams))
	for _, t := range teams {
		out = append(out, toJSON(t))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"teams": out})
}

type createTeamRequest struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// GroupID names a provisioned group; GroupObjectID a group in the customer's Entra directory,
	// which is imported first.
	GroupID       string   `json:"group_id"`
	GroupObjectID string   `json:"group_object_id"`
	MatchValue    string   `json:"match_value"`
	Members       []string `json:"members"`
}

func (h *Handler) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var req createTeamRequest
	if !h.decode(w, r, &req) {
		return
	}
	name, ok := h.teamName(w, req.Name)
	if !ok {
		return
	}
	team := Team{Name: name, Source: req.Source, CreatedBy: p.Actor, CreatedAt: h.cfg.Now().UTC()}
	if team.CreatedBy == "" {
		team.CreatedBy = p.Subject
	}
	var members []string
	switch req.Source {
	case SourceConsole:
		if members, ok = h.refs(w, req.Members); !ok {
			return
		}
	case SourceGroup:
		if team.GroupID, ok = h.teamGroup(w, r, p, req); !ok {
			return
		}
	case SourceDepartment, SourceOrgUnit:
		team.MatchValue = strings.TrimSpace(req.MatchValue)
		if team.MatchValue == "" || len(team.MatchValue) > maxMatchLen {
			h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, fmt.Sprintf("match_value is required and at most %d characters", maxMatchLen)))
			return
		}
	default:
		h.fail(w, apierr.Detailed(http.StatusBadRequest, apierr.CodeInvalidRequest, "source must be console, group, department or org_unit",
			map[string]any{"supported": []string{SourceConsole, SourceGroup, SourceDepartment, SourceOrgUnit}}))
		return
	}
	if req.Source != SourceConsole && len(req.Members) > 0 {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "members are chosen only for a console team; this team's members follow the directory"))
		return
	}
	id, err := store.NewUUID()
	if err != nil {
		h.fail(w, apierr.Internal(err))
		return
	}
	team.ID = id
	detail := map[string]any{"name": team.Name, "source": team.Source, "members_added": len(members)}
	switch team.Source {
	case SourceGroup:
		detail["group_id"] = team.GroupID
	case SourceDepartment, SourceOrgUnit:
		detail["match_value"] = team.MatchValue
	}
	err = h.store.CreateTeam(r.Context(), p.Tenant, team, members, h.audit(p, "team.create", "team", id, team.CreatedAt, detail))
	switch {
	case errors.Is(err, ErrNameTaken):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeTeamNameTaken, "a team already has this name"))
		return
	case errors.Is(err, ErrNotFound):
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such group"))
		return
	case h.storeFailed(w, err, "create team"):
		return
	}
	if team.Source == SourceGroup && h.cfg.Sync != nil {
		// A group imported from Entra, now or by an earlier attempt, has no members until the pull
		// reads them. A tenant with the pull off ignores the request.
		h.cfg.Sync.Trigger(p.Tenant)
	}
	h.cfg.Logger.Info("control: team created", "tenant", p.Tenant, "team_id", id, "source", team.Source)
	h.writeJSON(w, http.StatusCreated, toJSON(team))
}

// teamGroup resolves the group a new team follows, importing it from Entra when it is named by
// object id and not yet held.
func (h *Handler) teamGroup(w http.ResponseWriter, r *http.Request, p deploy.Principal, req createTeamRequest) (string, bool) {
	if gid := strings.ToLower(strings.TrimSpace(req.GroupID)); gid != "" {
		if !store.IsUUID(gid) {
			h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such group"))
			return "", false
		}
		return gid, true
	}
	oid := strings.ToLower(strings.TrimSpace(req.GroupObjectID))
	if !store.IsUUID(oid) {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "a group team needs group_id or group_object_id"))
		return "", false
	}
	if id, found, err := h.store.GroupByExternalID(r.Context(), p.Tenant, oid); err != nil {
		h.storeFailed(w, err, "find group")
		return "", false
	} else if found {
		return id, true
	}
	st, err := h.store.Status(r.Context(), p.Tenant)
	if h.storeFailed(w, err, "directory status") {
		return "", false
	}
	if st.Sync == nil || h.cfg.Groups == nil {
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeDirectorySyncOff, "importing a group from Microsoft Entra needs reading the directory turned on"))
		return "", false
	}
	tid, ok := h.graphTenant(w, r, p)
	if !ok {
		return "", false
	}
	g, err := h.cfg.Directory.Group(r.Context(), tid, oid)
	if graphsync.IsNotFound(err) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such group in the directory"))
		return "", false
	}
	if err != nil {
		h.graphFailed(w, err)
		return "", false
	}
	created, e := h.cfg.Groups.CreateGroup(r.Context(), scim.Principal{TenantID: p.Tenant, Actor: graphsync.Actor},
		map[string]any{"schemas": []any{scim.SchemaGroup}, "displayName": g.DisplayName, "externalId": oid})
	if e != nil {
		h.fail(w, apierr.Internal(fmt.Errorf("import group: %w", e)))
		return "", false
	}
	id, _ := created["id"].(string)
	return id, true
}

func (h *Handler) handleRenameTeam(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := h.teamID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	name, ok := h.teamName(w, req.Name)
	if !ok {
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.RenameTeam(r.Context(), p.Tenant, id, name, h.audit(p, "team.rename", "team", id, now, map[string]any{"name": name}))
	if h.teamFailed(w, err, "rename team") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := h.teamID(w, r)
	if !ok {
		return
	}
	now := h.cfg.Now().UTC()
	if h.teamFailed(w, h.store.DeleteTeam(r.Context(), p.Tenant, id, h.audit(p, "team.delete", "team", id, now, nil)), "delete team") {
		return
	}
	h.cfg.Logger.Info("control: team deleted", "tenant", p.Tenant, "team_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleMembers(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := h.teamID(w, r)
	if !ok {
		return
	}
	limit := listLimit
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v < listLimit {
		limit = v
	}
	members, err := h.store.TeamMembers(r.Context(), p.Tenant, id, limit)
	if h.teamFailed(w, err, "team members") {
		return
	}
	type memberJSON struct {
		UserRef    string `json:"user_ref"`
		Name       string `json:"name,omitempty"`
		Department string `json:"department,omitempty"`
	}
	out := make([]memberJSON, 0, len(members))
	for _, m := range members {
		out = append(out, memberJSON(m))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"members": out, "limit": limit})
}

func (h *Handler) handleChangeMembers(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := h.teamID(w, r)
	if !ok {
		return
	}
	var req struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	if len(req.Add)+len(req.Remove) == 0 {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "name at least one person to add or remove"))
		return
	}
	add, ok := h.refs(w, req.Add)
	if !ok {
		return
	}
	remove, ok := h.refs(w, req.Remove)
	if !ok {
		return
	}
	now := h.cfg.Now().UTC()
	err := h.store.ChangeMembers(r.Context(), p.Tenant, id, add, remove,
		h.audit(p, "team.members.change", "team", id, now, map[string]any{"added": len(add), "removed": len(remove)}))
	if errors.Is(err, ErrNotConsole) {
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeTeamNotConsole, "this team's members follow the directory; change them there"))
		return
	}
	if h.teamFailed(w, err, "change team members") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- plumbing ----------------------------------------------------------------------------------

func (h *Handler) admin(w http.ResponseWriter, r *http.Request) (deploy.Principal, bool) {
	p, err := h.auth(r)
	if err != nil {
		var e *apierr.Error
		if !errors.As(err, &e) {
			err = apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "a valid product access token is required")
		}
		h.fail(w, err)
		return deploy.Principal{}, false
	}
	if !store.IsUUID(p.Tenant) {
		h.fail(w, apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated, "the access token names no tenant"))
		return deploy.Principal{}, false
	}
	if !slices.Contains(p.Roles, deploy.RoleAdmin) {
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeForbidden, "the admin role is required"))
		return deploy.Principal{}, false
	}
	return p, true
}

func (h *Handler) audit(p deploy.Principal, action, objectType, objectID string, at time.Time, detail map[string]any) Audit {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["subject"] = p.Subject
	actor := p.Actor
	if strings.TrimSpace(actor) == "" {
		actor = p.Subject
	}
	return Audit{Actor: actor, Action: action, ObjectType: objectType, ObjectID: objectID, Detail: detail, At: at}
}

func (h *Handler) teamID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.ToLower(r.PathValue("id"))
	if !store.IsUUID(id) {
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such team"))
		return "", false
	}
	return id, true
}

func (h *Handler) teamName(w http.ResponseWriter, raw string) (string, bool) {
	name := strings.Join(strings.Fields(raw), " ")
	if name == "" || len([]rune(name)) > maxNameLen {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, fmt.Sprintf("a team name is 1 to %d characters", maxNameLen)))
		return "", false
	}
	return name, true
}

// refs validates a list of people's references, deduplicated.
func (h *Handler) refs(w http.ResponseWriter, raw []string) ([]string, bool) {
	if len(raw) > maxMemberChange {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, fmt.Sprintf("at most %d people per request", maxMemberChange)))
		return nil, false
	}
	seen := map[string]bool{}
	out := []string{}
	for _, ref := range raw {
		ref = strings.TrimSpace(ref)
		if !userRefPattern.MatchString(ref) {
			h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "a member is named by their user reference (u_ and 32 hex digits)"))
			return nil, false
		}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out, true
}

// graphTenant returns the customer's Entra directory, or answers why it cannot be read.
func (h *Handler) graphTenant(w http.ResponseWriter, r *http.Request, p deploy.Principal) (string, bool) {
	if h.cfg.Directory == nil {
		h.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable, "this deployment has no Microsoft Entra application, so it cannot read a directory"))
		return "", false
	}
	tid, err := h.store.EntraTenant(r.Context(), p.Tenant)
	if h.storeFailed(w, err, "entra tenant") {
		return "", false
	}
	if tid == "" {
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeNoEntraConnection, "reading the directory needs an active Microsoft Entra connection for this tenant"))
		return "", false
	}
	return tid, true
}

func (h *Handler) graphFailed(w http.ResponseWriter, err error) {
	var t *graphsync.TokenError
	switch {
	case graphsync.IsConsent(err), errors.As(err, &t):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeGraphConsent,
			"Microsoft Entra refused the request: an administrator of your directory needs to grant this application User.Read.All and GroupMember.Read.All"))
	default:
		h.cfg.Logger.Warn("control: directory unreachable", "error", err)
		h.fail(w, apierr.New(http.StatusBadGateway, apierr.CodeDirectoryUnreached, "Microsoft Entra could not be reached; try again"))
	}
}

func (h *Handler) storeFailed(w http.ResponseWriter, err error, what string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrUnknownTenant):
		h.fail(w, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is unknown to this deployment"))
	default:
		h.fail(w, apierr.Internal(fmt.Errorf("%s: %w", what, err)))
	}
	return true
}

func (h *Handler) teamFailed(w http.ResponseWriter, err error, what string) bool {
	switch {
	case errors.Is(err, ErrNotFound):
		h.fail(w, apierr.New(http.StatusNotFound, apierr.CodeNotFound, "no such team"))
		return true
	case errors.Is(err, ErrNameTaken):
		h.fail(w, apierr.New(http.StatusConflict, apierr.CodeTeamNameTaken, "a team already has this name"))
		return true
	}
	return h.storeFailed(w, err, what)
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		h.fail(w, apierr.New(http.StatusRequestEntityTooLarge, apierr.CodeInvalidRequest, "the request body is over the cap"))
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		h.fail(w, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation, "the request body is not the expected JSON object"))
		return false
	}
	return true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	apierr.Write(w, err, h.cfg.Now(), h.cfg.Logger)
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.cfg.Logger.Error("control: write response", "error", err)
	}
}
