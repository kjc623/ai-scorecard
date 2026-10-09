package userhelper

import (
	"fmt"
	"strings"

	"github.com/shadow-ai-capture/device/protocol"
)

// AppUserModelID is the identity toasts are shown under. Windows shows a desktop app's toast only
// when a Start-menu shortcut carries this ID as its System.AppUserModel.ID property; the MSI
// installs that shortcut (device/installer/manifest.mjs).
const AppUserModelID = "ShadowAICapture.Agent"

// toastXML is the toast for n: the title and body as the generic template's two text lines, and a
// link as a button that opens it through protocol activation. Every character outside printable
// ASCII is written as a character reference, so the document is ASCII.
func toastXML(n protocol.Notify) string {
	var b strings.Builder
	b.WriteString(`<toast><visual><binding template="ToastGeneric"><text>`)
	b.WriteString(xmlText(n.Title))
	b.WriteString(`</text><text>`)
	b.WriteString(xmlText(n.Body))
	b.WriteString(`</text></binding></visual>`)
	if n.Link != "" {
		b.WriteString(`<actions><action content="Open" activationType="protocol" arguments="`)
		b.WriteString(xmlText(n.Link))
		b.WriteString(`"/></actions>`)
	}
	b.WriteString(`</toast>`)
	return b.String()
}

func xmlText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '&':
			b.WriteString("&amp;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\'':
			b.WriteString("&apos;")
		case r < 0x20 || r > 0x7e:
			fmt.Fprintf(&b, "&#x%X;", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
