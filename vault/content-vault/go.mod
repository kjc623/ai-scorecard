module github.com/shadow-ai-capture/content-vault

go 1.27.0

require github.com/shadow-ai-capture/device/protocol v0.0.0

// The device-side protocol package is consumed for the collection-mode vocabulary (m0..m3) that
// the search tiers and the grant path branch on, so this service cannot fork a second spelling of
// "m3". It is consumed, never redefined.
replace github.com/shadow-ai-capture/device/protocol => ../../endpoint/protocol
