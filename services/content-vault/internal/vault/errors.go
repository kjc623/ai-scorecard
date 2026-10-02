// Package vault is the content vault's authorisation and key logic: the part that decides whether
// a caller may store, read, shred or search content, and the only part of the system that can
// unwrap a content key (docs/06 §5.3, D7).
//
// The two rules that shape every method:
//
//   - **Content is read only through a recorded, unexpired, single-use grant bound to the
//     requesting principal and the event** (docs/02 §11). No grant, an expired grant, a consumed
//     grant, a grant used for another event or another principal: each is a refusal with a reason
//     from a closed set, and the refusal is what the caller gets — never content.
//   - **A read is audited before it is served, and fails closed** (docs/02 §11, §6.3). If the audit
//     row cannot be committed, the read does not happen. An access that returned nothing is safe
//     to record; content served with no record is not.
//
// Destruction is a third: destroying a key destroys the content, so the read path reports the
// result as *destroyed* — `no_longer_available` — and never as an error (docs/06 §6.4).
package vault

import (
	"errors"
	"fmt"
	"sort"
)

// DenialReason is the closed refusal vocabulary.
//
// The first four are docs/02-ingest-and-transport.md §10.2's grant-decision reasons, verbatim: they
// travel with any content whose *grant* was denied, and a caller asking for such content gets the
// original reason rather than a new one. The rest are the refusals §11's retrieval path needs and
// does not enumerate — "no grant", "expired", "already used", "wrong event", "wrong principal",
// the C16 approval fields, the ADR 0014 custody/search conflict, and the search tiers. They are a
// closed set here for the same reason §10.2's four are a closed set in the schema: a refusal that
// cannot be grouped is a refusal nobody can count, and `ops.grant.denial_reason`'s CHECK is the
// precedent. README.md records this as a documentation gap rather than presenting the extra values
// as if §10.2 had listed them.
type DenialReason string

const (
	// §10.2, verbatim: the four grant-decision reasons.
	DenyRetentionExpired  DenialReason = "retention_expired"
	DenyNotPolicyRelevant DenialReason = "not_policy_relevant"
	DenyOverBudget        DenialReason = "over_budget"
	DenyModeNotPermitted  DenialReason = "mode_not_permitted"

	// Retrieval and redemption refusals.
	DenyGrantRequired             DenialReason = "grant_required"
	DenyGrantExpired              DenialReason = "grant_expired"
	DenyGrantAlreadyUsed          DenialReason = "grant_already_used"
	DenyGrantEventMismatch        DenialReason = "grant_event_mismatch"
	DenyGrantPrincipalMismatch    DenialReason = "grant_principal_mismatch"
	DenyNoContentObject           DenialReason = "no_content_object"
	DenyCaseReferenceRequired     DenialReason = "case_reference_required"
	DenySecondApproverRequired    DenialReason = "second_approver_required"
	DenySecondApproverNotDistinct DenialReason = "second_approver_not_distinct"
	DenyAuditUnavailable          DenialReason = "audit_unavailable"
	DenyTenantNotPermitted        DenialReason = "tenant_not_permitted"
	DenyRetrievalDisabled         DenialReason = "retrieval_disabled"
	DenyKeyCustodySearchConflict  DenialReason = "key_custody_search_conflict"
	DenySearchTierRequiresM3      DenialReason = "search_tier_requires_m3"
	DenySearchDisabled            DenialReason = "search_disabled"
	DenySearchTierNotInScope      DenialReason = "search_tier_not_in_scope"
	DenySearchUnitNotPermitted    DenialReason = "search_unit_not_permitted"
	DenyCustodyModeUnsupported    DenialReason = "custody_mode_unsupported"
	DenyIndexUnitNotPermitted     DenialReason = "index_unit_not_permitted"
	DenyTenantMismatch            DenialReason = "tenant_mismatch"
)

// AllDenialReasons is the closed set, for validation and for a coverage report that enumerates
// refusals rather than discovering them.
var AllDenialReasons = []DenialReason{
	DenyRetentionExpired, DenyNotPolicyRelevant, DenyOverBudget, DenyModeNotPermitted,
	DenyGrantRequired, DenyGrantExpired, DenyGrantAlreadyUsed, DenyGrantEventMismatch,
	DenyGrantPrincipalMismatch, DenyNoContentObject, DenyCaseReferenceRequired,
	DenySecondApproverRequired, DenySecondApproverNotDistinct, DenyAuditUnavailable,
	DenyTenantNotPermitted, DenyRetrievalDisabled, DenyKeyCustodySearchConflict,
	DenySearchTierRequiresM3, DenySearchDisabled, DenySearchTierNotInScope,
	DenySearchUnitNotPermitted, DenyCustodyModeUnsupported, DenyIndexUnitNotPermitted,
	DenyTenantMismatch,
}

// Valid reports membership of the closed set.
func (d DenialReason) Valid() bool {
	for _, k := range AllDenialReasons {
		if k == d {
			return true
		}
	}
	return false
}

// GrantDecisionReasons are §10.2's four, exposed so a test can assert that the reason a caller is
// given for a denied grant is one of them and that they are a subset of the closed set above.
var GrantDecisionReasons = []DenialReason{DenyRetentionExpired, DenyNotPolicyRelevant, DenyOverBudget, DenyModeNotPermitted}

// UnavailableReason is docs/02 §11's `no_longer_available` vocabulary, verbatim. It is a separate
// type from DenialReason because it answers a different question: a denial means "you may not",
// and unavailability means "it is not there any more". Collapsing them would make an erasure look
// like a permissions problem.
type UnavailableReason string

const (
	UnavailableRetentionExpired UnavailableReason = "retention_expired"
	UnavailableErasure          UnavailableReason = "erasure"
	UnavailableHoldReleased     UnavailableReason = "hold_released"
	UnavailableTenantOffboarded UnavailableReason = "tenant_offboarded"
	UnavailableKeyUnavailable   UnavailableReason = "key_unavailable"
)

// AllUnavailableReasons is §11's closed set.
var AllUnavailableReasons = []UnavailableReason{
	UnavailableRetentionExpired, UnavailableErasure, UnavailableHoldReleased,
	UnavailableTenantOffboarded, UnavailableKeyUnavailable,
}

// Valid reports membership of the closed set.
func (r UnavailableReason) Valid() bool {
	for _, k := range AllUnavailableReasons {
		if k == r {
			return true
		}
	}
	return false
}

// Denial is a refusal. It carries the closed reason and a detail for the operator; the reason is
// what a caller branches on and what a coverage report counts.
type Denial struct {
	Reason DenialReason
	Detail string
}

func (d *Denial) Error() string {
	if d.Detail == "" {
		return string(d.Reason)
	}
	return string(d.Reason) + ": " + d.Detail
}

// Denialf builds a refusal.
func Denialf(reason DenialReason, format string, args ...any) *Denial {
	if !reason.Valid() {
		// A refusal outside the closed set is a programming error, and it must not be silently
		// served as if it were one of the known reasons.
		return &Denial{Reason: reason, Detail: fmt.Sprintf("UNCLOSED REASON: "+format, args...)}
	}
	return &Denial{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// IsDenial reports whether err is a refusal, and returns it.
func IsDenial(err error) (*Denial, bool) {
	var d *Denial
	if errors.As(err, &d) {
		return d, true
	}
	return nil, false
}

// Unavailable is the §11 result for content that is no longer there. It is a *result*, served with
// 200: "an expired, erased or shredded record returns 200 with an explicit result, never 404 and
// never an empty body" (C17).
type Unavailable struct {
	Reason     UnavailableReason
	ReceiptRef string
	Detail     string
}

func (u *Unavailable) Error() string {
	return "no_longer_available: " + string(u.Reason)
}

// IsUnavailable reports whether err is a §11 unavailability.
func IsUnavailable(err error) (*Unavailable, bool) {
	var u *Unavailable
	if errors.As(err, &u) {
		return u, true
	}
	return nil, false
}

// SortedDenialReasons returns the closed set as strings, sorted, for documentation output.
func SortedDenialReasons() []string {
	out := make([]string, 0, len(AllDenialReasons))
	for _, r := range AllDenialReasons {
		out = append(out, string(r))
	}
	sort.Strings(out)
	return out
}
