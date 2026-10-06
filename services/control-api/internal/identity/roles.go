package identity

import "github.com/shadow-ai-capture/control-api/internal/session"

// resolveRoles is the role rule: the IdP's role values mapped by the connection's
// role_map, united with the grants recorded in ops.role_grant.
//
// An empty role_map is the identity map over the product role names, so an Entra app role named
// "analyst" means analyst with no configuration. A non-empty map is the whole mapping: a value it
// does not name grants nothing, even when it happens to equal a product role, because an admin who
// wrote a map meant it to be the list. Nothing here ever supplies a default role; a person with no
// role is refused at sign-in.
func resolveRoles(idpValues []string, roleMap map[string]string, grants []string) []string {
	var out []string
	for _, v := range idpValues {
		if len(roleMap) == 0 {
			if session.ValidRole(v) {
				out = append(out, v)
			}
			continue
		}
		if mapped, ok := roleMap[v]; ok && session.ValidRole(mapped) {
			out = append(out, mapped)
		}
	}
	for _, g := range grants {
		if session.ValidRole(g) {
			out = append(out, g)
		}
	}
	return session.CanonicalRoles(out)
}
