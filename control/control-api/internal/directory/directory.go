// Package directory holds what the organisational dimension (ops.user_dim; docs/03 §3.3, docs/04
// §3.3, Q2 in docs/risks/Q2-organisational-dimension.md) needs on the control plane: the Cipher that
// seals every `*_enc` column, the tenant's user-reference key (UserRefKeys), and the lab's offline
// synchroniser.
//
// A customer's people reach the product through SCIM, pushed by their identity provider
// (internal/scim), for any provider: the Entra application asks for no directory-read permission, so
// there is no pull from Microsoft Graph here. What remains in this package of the original pull is
// the file Source and the Syncer, which the lab uses to give the sample tenant's simulated people a
// department without an identity provider. In that path the join key is explicit: each file entry
// names the user_ref the device simulator sends.
//
// The synchroniser never deletes. A user the source no longer returns is retired (status
// 'inactive'), because history stays attributable to a person who has left (the task's first
// requirement). A user the source returns without a department is written with a NULL department,
// which the read path counts as the explicit `unmapped` series rather than dropping.
package directory

import (
	"context"
	"fmt"
	"strings"
)

// User is one directory person as a provider yields them, reduced to the columns ops.user_dim holds.
//
// UserRef is the join key to the wire: it must equal the user reference the device was configured
// with, because ingest stores that value on every submission and the read path joins on equality
// (query/query-api/src/registry.js, the user_dim join). DirectoryID is the provider's own immutable
// identifier for the person; it is sealed before it is stored and is the only column that maps a
// user_ref to a real person. Department, Population and ManagerRef are the directory attributes the
// dashboard reads; an empty Department means "the directory has no department for this person", not
// "leave the previous value alone", so the sync writes NULL and the person is counted unmapped.
type User struct {
	UserRef     string
	DirectoryID string
	DisplayName string
	Department  string
	Population  string
	ManagerRef  string
	Status      string
}

// StatusActive and StatusInactive are the two ops.user_dim status values the sync writes.
// StatusUnknown is the schema's default for a row nothing has decided about; the sync never writes
// it. A directory account that is disabled is inactive for the same reason a departed user is: the
// person is not expected to be sending anything.
const (
	StatusActive   = "active"
	StatusInactive = "inactive"
)

// Source is one directory provider. List returns a complete view of the directory: a partial read
// must be an error rather than a short list, because the synchroniser retires every existing row
// the returned set omits. Name is the provider's vocabulary word, for logs and the CLI.
type Source interface {
	Name() string
	List(ctx context.Context) ([]User, error)
}

// normalizeStatus maps a provider's notion of enabled/disabled onto the schema's vocabulary.
// An empty or unrecognised value is 'active': a user the directory returned is a user it holds,
// and the sync must not invent a departure from a missing field.
func normalizeStatus(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "inactive", "disabled", "false", "no":
		return StatusInactive
	case "active", "enabled", "true", "yes", "":
		return StatusActive
	default:
		return StatusActive
	}
}

// validate rejects a source set that cannot be synced coherently: a duplicated user reference would
// make the upsert ambiguous, and an ambiguous join key is worse than no row. A user with no user
// reference at all is not an error here: it is counted and skipped by the sync, because a directory
// routinely holds accounts the mapping attribute does not name.
func validate(users []User) error {
	seen := make(map[string]struct{}, len(users))
	for _, u := range users {
		ref := strings.TrimSpace(u.UserRef)
		if ref == "" {
			continue
		}
		if _, dup := seen[ref]; dup {
			return fmt.Errorf("directory: user_ref %q appears more than once in one read", ref)
		}
		seen[ref] = struct{}{}
	}
	return nil
}
