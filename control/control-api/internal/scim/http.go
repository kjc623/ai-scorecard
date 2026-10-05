package scim

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// MaxBodyBytes bounds a SCIM request body. A user is a few KiB; a group PUT with its members is the
// largest document a provider sends, and Okta pages those.
const MaxBodyBytes = 1 << 20

// Handler is the SCIM HTTP surface. It owns authentication, routing, the body cap and the SCIM
// response and error envelopes; every decision is the Service's.
type Handler struct {
	svc    *Service
	prefix string
	logger *slog.Logger
}

// NewHandler serves the API under prefix, normally "/scim/v2" (the URL an identity provider is
// given is `{public URL}/scim/v2`). Mount it for both the prefix and everything under it.
func NewHandler(svc *Service, prefix string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, prefix: strings.TrimRight(prefix, "/"), logger: logger}
}

// statusRecorder keeps the status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ServeHTTP implements http.Handler. The access log names the method, the resource kind, the tenant
// and the status; never the query, because a filter carries a userName.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	tenant := ""
	kind := "unknown"
	defer func() {
		h.logger.Info("scim request", "method", r.Method, "resource", kind, "tenant", tenant, "status", rec.status)
	}()

	rest, ok := strings.CutPrefix(r.URL.Path, h.prefix)
	if !ok || (rest != "" && rest[0] != '/') {
		h.fail(rec, &Error{Status: http.StatusNotFound, Detail: "not a SCIM endpoint"})
		return
	}
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segs) > 2 || segs[0] == "" {
		h.fail(rec, &Error{Status: http.StatusNotFound, Detail: "not a SCIM endpoint"})
		return
	}
	kind = segs[0]
	id := ""
	if len(segs) == 2 {
		id = segs[1]
	}

	// Every endpoint, discovery included, requires the token: both providers send it on every
	// request, and an unauthenticated surface on the public edge is one more thing to reason about.
	p, e := h.svc.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if e != nil {
		rec.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
		h.fail(rec, e)
		return
	}
	tenant = p.TenantID

	switch strings.ToLower(kind) {
	case "serviceproviderconfig", "serviceproviderconfigs":
		if id != "" {
			h.fail(rec, notFound("resource"))
			return
		}
		if !h.allow(rec, r, http.MethodGet) {
			return
		}
		h.write(rec, http.StatusOK, h.svc.serviceProviderConfig())
	case "schemas":
		if !h.allow(rec, r, http.MethodGet) {
			return
		}
		h.discovery(rec, h.svc.schemas(), id, "schema")
	case "resourcetypes":
		if !h.allow(rec, r, http.MethodGet) {
			return
		}
		h.discovery(rec, h.svc.resourceTypes(), id, "resource type")
	case "users":
		h.users(rec, r, p, id)
	case "groups":
		h.groups(rec, r, p, id)
	case "bulk":
		h.fail(rec, &Error{Status: http.StatusNotImplemented, Detail: "bulk operations are not supported"})
	default:
		h.fail(rec, &Error{Status: http.StatusNotFound, Detail: "not a SCIM endpoint"})
	}
}

func (h *Handler) discovery(w http.ResponseWriter, docs []map[string]any, id, what string) {
	if id == "" {
		h.write(w, http.StatusOK, listResponse(len(docs), 1, docs))
		return
	}
	d := findDoc(docs, id)
	if d == nil {
		h.fail(w, notFound(what))
		return
	}
	h.write(w, http.StatusOK, d)
}

func (h *Handler) users(w http.ResponseWriter, r *http.Request, p Principal, id string) {
	ctx := r.Context()
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			resp, e := h.svc.ListUsers(ctx, p, listQuery(r))
			h.respond(w, http.StatusOK, resp, e)
		case http.MethodPost:
			body, e := readBody(r)
			if e != nil {
				h.fail(w, e)
				return
			}
			res, e := h.svc.CreateUser(ctx, p, body)
			if e == nil {
				if loc := h.svc.location("Users", res["id"].(string)); loc != "" {
					w.Header().Set("Location", loc)
				}
			}
			h.respond(w, http.StatusCreated, res, e)
		default:
			h.notAllowed(w, "GET, POST")
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		res, e := h.svc.GetUser(ctx, p, id)
		h.respond(w, http.StatusOK, res, e)
	case http.MethodPut:
		body, e := readBody(r)
		if e != nil {
			h.fail(w, e)
			return
		}
		res, e := h.svc.ReplaceUser(ctx, p, id, body)
		h.respond(w, http.StatusOK, res, e)
	case http.MethodPatch:
		body, e := readBody(r)
		if e != nil {
			h.fail(w, e)
			return
		}
		res, e := h.svc.PatchUser(ctx, p, id, body)
		h.respond(w, http.StatusOK, res, e)
	case http.MethodDelete:
		h.respond(w, http.StatusNoContent, nil, h.svc.DeleteUser(ctx, p, id))
	default:
		h.notAllowed(w, "GET, PUT, PATCH, DELETE")
	}
}

func (h *Handler) groups(w http.ResponseWriter, r *http.Request, p Principal, id string) {
	ctx := r.Context()
	q := listQuery(r)
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			resp, e := h.svc.ListGroups(ctx, p, q)
			h.respond(w, http.StatusOK, resp, e)
		case http.MethodPost:
			body, e := readBody(r)
			if e != nil {
				h.fail(w, e)
				return
			}
			res, e := h.svc.CreateGroup(ctx, p, body)
			if e == nil {
				if loc := h.svc.location("Groups", res["id"].(string)); loc != "" {
					w.Header().Set("Location", loc)
				}
			}
			h.respond(w, http.StatusCreated, res, e)
		default:
			h.notAllowed(w, "GET, POST")
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		res, e := h.svc.GetGroup(ctx, p, id, q.ExcludeMembers)
		h.respond(w, http.StatusOK, res, e)
	case http.MethodPut:
		body, e := readBody(r)
		if e != nil {
			h.fail(w, e)
			return
		}
		res, e := h.svc.ReplaceGroup(ctx, p, id, body)
		h.respond(w, http.StatusOK, res, e)
	case http.MethodPatch:
		body, e := readBody(r)
		if e != nil {
			h.fail(w, e)
			return
		}
		// 204, as Entra's own reference answers a group PATCH: a large group's member list would
		// otherwise be rendered back on every membership change.
		h.respond(w, http.StatusNoContent, nil, h.svc.PatchGroup(ctx, p, id, body))
	case http.MethodDelete:
		h.respond(w, http.StatusNoContent, nil, h.svc.DeleteGroup(ctx, p, id))
	default:
		h.notAllowed(w, "GET, PUT, PATCH, DELETE")
	}
}

func listQuery(r *http.Request) ListQuery {
	v := r.URL.Query()
	q := ListQuery{Filter: v.Get("filter"), StartIndex: 1, Count: -1}
	if n, err := strconv.Atoi(strings.TrimSpace(v.Get("startIndex"))); err == nil {
		q.StartIndex = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v.Get("count"))); err == nil {
		if n < 0 {
			n = 0
		}
		q.Count = n
	}
	for _, a := range strings.Split(v.Get("excludedAttributes"), ",") {
		if strings.EqualFold(strings.TrimSpace(a), "members") {
			q.ExcludeMembers = true
		}
	}
	return q
}

func readBody(r *http.Request) (map[string]any, *Error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, badRequest("invalidSyntax", "the request body could not be read")
	}
	if len(raw) > MaxBodyBytes {
		return nil, &Error{Status: http.StatusRequestEntityTooLarge, Detail: "the request body is too large"}
	}
	m, err := decodeObject(raw)
	if err != nil {
		return nil, badRequest("invalidSyntax", "the request body is not a JSON object")
	}
	return m, nil
}

func (h *Handler) allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	h.notAllowed(w, method)
	return false
}

func (h *Handler) notAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	h.fail(w, &Error{Status: http.StatusMethodNotAllowed, Detail: "method not allowed"})
}

// respond writes v with status, or the error. A typed nil pointer in v is never written as `null`.
func (h *Handler) respond(w http.ResponseWriter, status int, v any, e *Error) {
	if e != nil {
		h.fail(w, e)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	h.write(w, status, v)
}

func (h *Handler) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/scim+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail renders RFC 7644 §3.12's error envelope. status is a string there, by the RFC's own example
// and as Entra parses it.
func (h *Handler) fail(w http.ResponseWriter, e *Error) {
	if e.Status >= 500 {
		h.logger.Error("scim request failed", "status", e.Status, "error", e.Error())
	}
	body := map[string]any{
		"schemas": []string{SchemaError},
		"status":  strconv.Itoa(e.Status),
		"detail":  e.Detail,
	}
	if e.ScimType != "" {
		body["scimType"] = e.ScimType
	}
	h.write(w, e.Status, body)
}
