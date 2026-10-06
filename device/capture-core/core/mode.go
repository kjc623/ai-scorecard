package core

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// ScopeQuery is what the device knows about an observation *before* it reads anything: the
// tool's behaviour-derived fingerprint, the user's population, and the device. Data class is
// deliberately absent — it is a *result*, and the mode must be chosen before the content is
// read, which is what the class-prior map exists for.
type ScopeQuery struct {
	ToolFingerprint string
	Population      string
	DeviceID        string
	UserRef         string
	// SubjectName is the clear account name, carried so the pipeline can stamp it on the envelope
	// when the tenant's device_identity is 'clear'. It has no part in mode resolution.
	SubjectName string
}

// Contribution is one scope entry's input to the resolved mode, kept so an operator can see
// *which* entry over-restricted a tool rather than being told "policy said so".
type Contribution struct {
	Axis   string // tool | population | device | class_ceiling | tenant_default
	Key    string
	Mode   protocol.CollectionMode
	Reason string
}

// Resolution is the effective mode for one observation plus the reasons it resolved that
// way. Mode is never empty: an unresolvable matrix resolves *downward* to M0.
type Resolution struct {
	Mode          protocol.CollectionMode
	Contributions []Contribution
	Reasons       []string
	PolicyVersion string
}

// ReadsContent reports whether the resolved mode permits reading the payload at all. Every
// content path must consult this before touching bytes.
func (r Resolution) ReadsContent() bool { return r.Mode.ReadsContent() }

// Reason strings used in reports. They are stable so a coverage report can group by them.
const (
	ReasonNoBundle         = "no_bundle_in_force"
	ReasonNoticeUnacked    = "notice_unacknowledged"
	ReasonTenantDefault    = "tenant_default_applies"
	ReasonClassPriorAbsent = "class_prior_absent"
	ReasonUnresolved       = "scope_entry_unresolvable_resolved_downward"
)

// Resolve applies the scope rule:
//
//	effective_mode = most_restrictive(
//	    mode_for_tool(tool_fingerprint),
//	    mode_for_population(population),
//	    mode_for_device(device),
//	    class ceiling for every class the tool's prior admits)
//
// with two gates in front of it that can only ever *lower* the mode:
//
//   - With no bundle in force (a fresh install whose first bundle failed
//     verification, or any moment before the first poll) the device is at M0. M0 is a
//     reduction in capability, never an increase, and no code path in this package can
//     return a content-reading mode without a verified bundle behind it.
//   - A user who has not acknowledged the required notice version resolves to
//     M0, with the reason reported rather than the downgrade being silent.
func Resolve(b *policy.Bundle, q ScopeQuery) Resolution {
	if b == nil {
		return Resolution{
			Mode:    protocol.ModeM0,
			Reasons: []string{ReasonNoBundle},
		}
	}
	res := Resolution{PolicyVersion: b.Version}
	if b.RequiredNoticeVersion != "" {
		if got := b.AcknowledgedNotices[q.UserRef]; got != b.RequiredNoticeVersion {
			res.Mode = protocol.ModeM0
			res.Reasons = append(res.Reasons, ReasonNoticeUnacked)
			res.Contributions = append(res.Contributions, Contribution{
				Axis:   "notice",
				Key:    q.UserRef,
				Mode:   protocol.ModeM0,
				Reason: fmt.Sprintf("notice version %q required, %q acknowledged", b.RequiredNoticeVersion, got),
			})
			return res
		}
	}

	axes := []struct {
		axis string
		key  string
		set  map[string]protocol.CollectionMode
	}{
		{"tool", q.ToolFingerprint, b.ToolModes},
		{"population", q.Population, b.PopulationModes},
		{"device", q.DeviceID, b.DeviceModes},
	}
	for _, a := range axes {
		if strings.TrimSpace(a.key) == "" {
			// No key on this axis: the tenant default is the only value that can apply, and
			// there is no value of "unset" that means "everything".
			res.Contributions = append(res.Contributions, Contribution{
				Axis: a.axis, Key: a.key, Mode: b.TenantDefault, Reason: ReasonTenantDefault,
			})
			addReason(&res, ReasonTenantDefault)
			continue
		}
		m, ok := a.set[a.key]
		if !ok {
			res.Contributions = append(res.Contributions, Contribution{
				Axis: a.axis, Key: a.key, Mode: b.TenantDefault, Reason: ReasonTenantDefault,
			})
			addReason(&res, ReasonTenantDefault)
			continue
		}
		if !m.Valid() {
			// A matrix that fails to resolve resolves downward; the strictest downward value
			// is M0, and the reason is named rather than silently substituted.
			res.Reasons = append(res.Reasons, ReasonUnresolved)
			res.Contributions = append(res.Contributions, Contribution{
				Axis: a.axis, Key: a.key, Mode: protocol.ModeM0, Reason: ReasonUnresolved,
			})
			continue
		}
		res.Contributions = append(res.Contributions, Contribution{Axis: a.axis, Key: a.key, Mode: m})
	}

	// The class ceiling: for every class the tool's prior admits, the tenant's mode for that
	// class. This is what makes resolution conservative rather than optimistic — a tool whose
	// prior admits `health` resolves against the tenant's `health` mode even when this prompt
	// contains none, because the device cannot know the class without reading.
	prior, hasPrior := b.ClassPriors[q.ToolFingerprint]
	if !hasPrior || len(prior) == 0 {
		res.Reasons = append(res.Reasons, ReasonClassPriorAbsent)
	} else {
		classes := append([]string(nil), prior...)
		sort.Strings(classes) // deterministic contribution order
		for _, class := range classes {
			m, ok := b.ClassModes[class]
			if !ok {
				m = b.TenantDefault
			}
			if !m.Valid() {
				m = protocol.ModeM0
				res.Reasons = append(res.Reasons, ReasonUnresolved)
			}
			res.Contributions = append(res.Contributions, Contribution{
				Axis: "class_ceiling", Key: class, Mode: m,
			})
		}
	}

	res.Mode = MostRestrictive(contributionsToModes(res.Contributions)...)
	return res
}

func contributionsToModes(cs []Contribution) []protocol.CollectionMode {
	out := make([]protocol.CollectionMode, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Mode)
	}
	return out
}

// addReason records a reason once. The three axes can each fall back to the tenant default in
// one resolution, and a coverage report wants the fact once, not three times.
func addReason(res *Resolution, reason string) {
	for _, r := range res.Reasons {
		if r == reason {
			return
		}
	}
	res.Reasons = append(res.Reasons, reason)
}

// ModeRank orders the modes: m0 < m1 < m2 < m3. An unknown mode ranks below M0, so it can
// only ever restrict — never widen.
func ModeRank(m protocol.CollectionMode) int {
	switch m {
	case protocol.ModeM0:
		return 0
	case protocol.ModeM1:
		return 1
	case protocol.ModeM2:
		return 2
	case protocol.ModeM3:
		return 3
	default:
		return -1
	}
}

// MostRestrictive returns the lowest-ranked mode, which for the mode ordering
// (m0 < m1 < m2 < m3) is the most restrictive. With no inputs it returns M0: "most
// restrictive" of nothing is the one mode that reads nothing.
func MostRestrictive(modes ...protocol.CollectionMode) protocol.CollectionMode {
	best := protocol.ModeM0
	bestRank := 1 << 30
	for _, m := range modes {
		if r := ModeRank(m); r < bestRank {
			best, bestRank = m, r
		}
	}
	if bestRank == 1<<30 {
		return protocol.ModeM0
	}
	if !best.Valid() {
		return protocol.ModeM0
	}
	return best
}

// ModeAnswer renders the resolution for the extension's mode_query / mode_answer pair, so an
// inline decision can use policy the extension already holds instead of a round trip.
func (r Resolution) ModeAnswer(host string) (protocol.ModeAnswer, error) {
	if !r.Mode.Valid() {
		return protocol.ModeAnswer{}, fmt.Errorf("core: resolution produced mode %q outside the closed set", r.Mode)
	}
	reason := strings.Join(r.Reasons, ",")
	if len(r.Contributions) > 0 {
		parts := make([]string, 0, len(r.Contributions))
		for _, c := range r.Contributions {
			parts = append(parts, fmt.Sprintf("%s=%s", c.Axis, c.Mode))
		}
		if reason != "" {
			reason += ";"
		}
		reason += strings.Join(parts, ",")
	}
	return protocol.ModeAnswer{Mode: r.Mode, PolicyVersion: r.PolicyVersion, Reason: reason}, nil
}
