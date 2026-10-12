package parsers

import (
	"net/url"
	"strings"
)

// URLFileName is the file name a file URL ends in (`https://host/docs/q3.pdf`,
// `gs://bucket/q3.pdf`): its last path segment when that has an extension. A URL that ends in an
// id rather than a name (`/v1beta/files/abc123`), a data URL and anything unparsable give "".
func URLFileName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "data" || u.Opaque != "" {
		return ""
	}
	seg := u.Path[strings.LastIndexByte(u.Path, '/')+1:]
	if dot := strings.LastIndexByte(seg, '.'); dot <= 0 || dot == len(seg)-1 {
		return ""
	}
	return seg
}
