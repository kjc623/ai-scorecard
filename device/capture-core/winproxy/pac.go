// Package winproxy captures desktop AI apps on Windows through a per-user PAC. It serves a PAC
// over loopback and writes its URL into each signed-in user's Internet Settings (AutoConfigURL).
// The PAC routes only the interception hosts the signed policy names to proxy.tls and returns the
// customer's previous route for every other host, so a device that already sets a proxy or PAC
// keeps it for everything we do not intercept.
//
// It fails open: when proxy.tls is not in the path the PAC returns the previous route for every
// host, and stopping the service (or uninstalling) restores each user's original AutoConfigURL.
package winproxy

import (
	"encoding/json"
	"strings"
)

// Original is one user's existing proxy behaviour, read from their Internet Settings. It is the
// route every non-intercepted host keeps.
type Original struct {
	// AutoConfigURL is the user's existing PAC URL, if any. When set and AutoConfigBody was
	// fetched, the generated PAC delegates to it for every host we do not intercept.
	AutoConfigURL string
	// AutoConfigBody is the fetched content of AutoConfigURL, inlined so the generated PAC can
	// delegate without the browser fetching a second PAC. Empty while AutoConfigURL is set means
	// the body could not be fetched, which fails open (the setting is left untouched).
	AutoConfigBody string
	// ProxyEnable and ProxyServer reproduce a fixed proxy. ProxyServer is "host:port" or a
	// ";"-separated list, as WinINET stores it.
	ProxyEnable bool
	ProxyServer string
	// ProxyOverride is the bypass list: "<local>", "*.example.com", an exact host, or an IP
	// address or CIDR range.
	ProxyOverride []string
}

// Spec is what the PAC renders from. It is evaluated on every request so a policy change or a
// proxy that drops out of the path takes effect on the next PAC fetch rather than a restart.
type Spec struct {
	// ProxyAddr is the loopback address proxy.tls is bound to. Empty means the proxy is not in
	// the path, so no host is intercepted and the PAC returns the original route for all of them.
	ProxyAddr string
	// InterceptHosts are the interception hosts from the signed bundle (seed and tenant hosts).
	InterceptHosts []string
	Original       Original
}

// Render returns the PAC script for spec.
func Render(spec Spec) []byte {
	if strings.TrimSpace(spec.ProxyAddr) == "" {
		// Fail open: nothing to intercept, so the PAC is the original route for every host.
		return []byte(renderOriginalOnly(spec.Original))
	}
	if spec.Original.AutoConfigURL != "" && delegatable(spec.Original.AutoConfigBody) {
		return []byte(renderDelegating(spec))
	}
	return []byte(renderDirect(spec))
}

// delegatable reports whether an existing PAC body can be inlined and wrapped: it must declare
// FindProxyForURL as a function declaration, so this script can capture it before redefining it. A
// body that only assigns to FindProxyForURL would capture undefined, so it is refused (fail open).
func delegatable(body string) bool {
	return strings.Contains(body, "function FindProxyForURL")
}

// classifyHosts splits interception hosts into exact and suffix patterns with the same meaning
// policy uses: a leading dot (or a leading "*") matches the bare host and its subdomains; anything
// else matches only the exact host. Matching is case-insensitive and ignores a trailing dot.
func classifyHosts(hosts []string) (exact, suffix []string) {
	seen := map[string]bool{}
	for _, h := range hosts {
		p := strings.ToLower(strings.TrimSpace(h))
		p = strings.TrimPrefix(p, "*")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if strings.HasPrefix(p, ".") {
			suffix = append(suffix, p)
		} else {
			exact = append(exact, p)
		}
	}
	return exact, suffix
}

// renderNormalise returns the host normalisation all templates share, so a trailing dot or case
// difference never changes the interception decision.
func renderNormalise() string {
	return `host = host.toLowerCase().replace(/\.+$/, "");`
}

// renderIntercept returns the JS that decides whether a host is intercepted. The matching rules
// here mirror classifyHosts, which the tests exercise directly.
func renderIntercept(hosts []string) string {
	exact, suffix := classifyHosts(hosts)
	var b strings.Builder
	b.WriteString("function __sacIntercept(host) {\n")
	if len(exact) > 0 {
		b.WriteString("  var exact = {")
		for i, h := range exact {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(jsString(h) + ":1")
		}
		b.WriteString("};\n")
		b.WriteString("  if (exact[host]) { return true; }\n")
	}
	if len(suffix) > 0 {
		b.WriteString("  var suffix = [")
		for i, h := range suffix {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(jsString(h))
		}
		b.WriteString("];\n")
		b.WriteString("  for (var i = 0; i < suffix.length; i++) {\n")
		b.WriteString("    var s = suffix[i];\n")
		b.WriteString("    if (host === s.slice(1) || host.slice(-s.length) === s) { return true; }\n")
		b.WriteString("  }\n")
	}
	b.WriteString("  return false;\n")
	b.WriteString("}\n")
	return b.String()
}

// renderDirect renders the fixed-proxy and direct cases: intercepted hosts go to the proxy, every
// other host reproduces the customer's fixed proxy (with its bypass list) or goes direct.
func renderDirect(spec Spec) string {
	var b strings.Builder
	b.WriteString("// Managed by Shadow AI Capture. Do not edit.\n")
	b.WriteString("function FindProxyForURL(url, host) {\n")
	b.WriteString("  " + renderNormalise() + "\n")
	b.WriteString("  if (__sacIntercept(host)) { return " + jsString("PROXY "+spec.ProxyAddr) + "; }\n")
	b.WriteString("  return __sacOriginal(url, host);\n")
	b.WriteString("}\n")
	b.WriteString(renderIntercept(spec.InterceptHosts))
	b.WriteString(renderFixedOriginal(spec.Original))
	return b.String()
}

// renderFixedOriginal renders __sacOriginal for a customer with a fixed proxy or no proxy at all.
// The bypass list is checked first, then the proxy list, then DIRECT as the final fallback so a
// dead proxy never strands the user.
func renderFixedOriginal(o Original) string {
	var b strings.Builder
	b.WriteString("function __sacOriginal(url, host) {\n")
	if bypass := renderBypass(o.ProxyOverride); bypass != "" {
		b.WriteString("  if (" + bypass + ") { return \"DIRECT\"; }\n")
	}
	b.WriteString("  return " + proxyRoute(o) + ";\n")
	b.WriteString("}\n")
	return b.String()
}

// proxyRoute is the JS proxy route for a fixed-proxy customer: each server, then DIRECT so a dead
// proxy falls back rather than stranding the user. With no proxy enabled it is just DIRECT. The
// result is a quoted JS string literal.
func proxyRoute(o Original) string {
	if !o.ProxyEnable || strings.TrimSpace(o.ProxyServer) == "" {
		return jsString("DIRECT")
	}
	var servers []string
	for _, entry := range strings.Split(o.ProxyServer, ";") {
		entry = strings.TrimSpace(entry)
		if i := strings.Index(entry, "="); i >= 0 {
			entry = strings.TrimSpace(entry[i+1:]) // WinINET "http=host:port" form
		}
		if entry == "" {
			continue
		}
		servers = append(servers, "PROXY "+entry)
	}
	if len(servers) == 0 {
		return jsString("DIRECT")
	}
	servers = append(servers, "DIRECT")
	return jsString(strings.Join(servers, "; "))
}

// renderBypass renders the bypass-list condition, returning "" when there is nothing to check. It
// mirrors WinINET's ProxyOverride: "<local>" matches a host with no dot, a "*"-prefixed entry
// matches the domain and its subdomains, and anything else matches the exact host.
func renderBypass(override []string) string {
	var conds []string
	for _, e := range override {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		switch {
		case strings.EqualFold(e, "<local>"):
			conds = append(conds, `host.indexOf(".") === -1`)
		case strings.HasPrefix(e, "*"):
			d := strings.ToLower(strings.TrimPrefix(e, "*"))
			if d == "" {
				continue
			}
			if !strings.HasPrefix(d, ".") {
				d = "." + d
			}
			conds = append(conds, hostSuffixCond(d))
		default:
			conds = append(conds, "host === "+jsString(strings.ToLower(e)))
		}
	}
	if len(conds) == 0 {
		return ""
	}
	return strings.Join(conds, " || ")
}

// hostSuffixCond is the JS condition "host is d, or a subdomain of d", for d with a leading dot
// (".example.com"): host equals "example.com", or ends with ".example.com".
func hostSuffixCond(d string) string {
	return "(host === " + jsString(d[1:]) + " || host.slice(-" + itoa(len(d)) + ") === " + jsString(d) + ")"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// renderDelegating renders the case where the customer already has a PAC: the original body is
// inlined and its FindProxyForURL is captured before this script redefines it, so non-intercepted
// hosts keep exactly the customer's previous decision.
func renderDelegating(spec Spec) string {
	var b strings.Builder
	b.WriteString("// Managed by Shadow AI Capture. Do not edit.\n")
	b.WriteString(strings.TrimSpace(spec.Original.AutoConfigBody) + "\n")
	b.WriteString("var __sacOriginalFindProxyForURL = FindProxyForURL;\n")
	b.WriteString("FindProxyForURL = function(url, host) {\n")
	b.WriteString("  " + renderNormalise() + "\n")
	b.WriteString("  if (__sacIntercept(host)) { return " + jsString("PROXY "+spec.ProxyAddr) + "; }\n")
	b.WriteString("  return __sacOriginalFindProxyForURL(url, host);\n")
	b.WriteString("};\n")
	b.WriteString(renderIntercept(spec.InterceptHosts))
	return b.String()
}

// renderOriginalOnly renders the customer's previous route with no interception at all, used when
// the proxy is not in the path: the PAC becomes a faithful copy of what the user had before.
func renderOriginalOnly(o Original) string {
	if o.AutoConfigURL != "" && delegatable(o.AutoConfigBody) {
		return "// Managed by Shadow AI Capture. Do not edit.\n" +
			strings.TrimSpace(o.AutoConfigBody) + "\n"
	}
	var b strings.Builder
	b.WriteString("// Managed by Shadow AI Capture. Do not edit.\n")
	b.WriteString("function FindProxyForURL(url, host) {\n")
	b.WriteString("  " + renderNormalise() + "\n")
	b.WriteString("  return __sacOriginal(url, host);\n")
	b.WriteString("}\n")
	b.WriteString(renderFixedOriginal(o))
	return b.String()
}

// jsString renders s as a JavaScript string literal (a JSON string is a valid JS literal).
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// A string always marshals; this is unreachable.
		return `""`
	}
	return string(b)
}
