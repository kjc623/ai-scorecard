package vault

import (
	"errors"
	"fmt"
)

// Reason is the closed set of codes a refusal carries. Callers branch on the code; the detail is for
// people.
type Reason string

// Refusal reasons.
const (
	// The request itself is malformed: a digest that does not match the body, an empty search.
	ReasonInvalidRequest Reason = "invalid_request"
	// The tenant is unknown, closed, or not accepting content.
	ReasonTenantNotPermitted Reason = "tenant_not_permitted"
	// Reads are switched off for the tenant.
	ReasonRetrievalDisabled Reason = "retrieval_disabled"

	// Upload grants.
	ReasonGrantUnknown    Reason = "grant_unknown"     // no such grant for this tenant and event
	ReasonGrantNotGranted Reason = "grant_not_granted" // the decision was not to grant
	ReasonGrantExpired    Reason = "grant_expired"     // the upload window has closed
	ReasonGrantConsumed   Reason = "grant_consumed"    // the grant was already used
	ReasonAlreadyStored   Reason = "already_stored"    // the event already has different content

	// Retrieval.
	ReasonSecondApproverNotDistinct Reason = "second_approver_not_distinct"
	ReasonNoContentObject           Reason = "no_content_object"
	ReasonGrantRequired             Reason = "grant_required" // no such retrieval grant
	ReasonGrantAlreadyUsed          Reason = "grant_already_used"

	// Search.
	ReasonSearchDisabled      Reason = "search_disabled"
	ReasonSearchCursorInvalid Reason = "search_cursor_invalid"
)

// Denial is a refusal with its reason.
type Denial struct {
	Reason Reason
	Detail string
}

func (d *Denial) Error() string { return string(d.Reason) + ": " + d.Detail }

func deny(reason Reason, format string, args ...any) *Denial {
	return &Denial{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// AsDenial reports whether err is a refusal.
func AsDenial(err error) (*Denial, bool) {
	var d *Denial
	ok := errors.As(err, &d)
	return d, ok
}

// UnavailableReason says why content that once existed cannot be served.
type UnavailableReason string

// Unavailability reasons.
const (
	UnavailableRetentionExpired UnavailableReason = "retention_expired"
	UnavailableErasure          UnavailableReason = "erasure"
	UnavailableKeyUnavailable   UnavailableReason = "key_unavailable"
)

// Unavailable is the result for content that is no longer there. It is an answer, not a failure:
// callers render it as an explicit state, never as "not found".
type Unavailable struct {
	Reason UnavailableReason
	// ReceiptRef is the tenant's latest erasure receipt, when there is one.
	ReceiptRef string
	Detail     string
}

func (u *Unavailable) Error() string { return "no_longer_available: " + string(u.Reason) }

// AsUnavailable reports whether err is an unavailability.
func AsUnavailable(err error) (*Unavailable, bool) {
	var u *Unavailable
	ok := errors.As(err, &u)
	return u, ok
}
