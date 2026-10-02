package httpapi

import "net/http"

// requireFields refuses a request that is missing a field the handler needs, as a *transport*
// error (400) rather than a content refusal. The distinction matters: a refusal says "you may not",
// and a caller that forgot a field has not been refused anything — it has sent a malformed request,
// and dressing that up as `no_content_object` or `grant_required` would put a lie in the coverage
// report.
func (s *Server) requireFields(w http.ResponseWriter, fields map[string]string) bool {
	for name, value := range fields {
		if value == "" {
			s.writeTransportError(w, http.StatusBadRequest, "bad_request", "field "+name+" is required")
			return false
		}
	}
	return true
}
