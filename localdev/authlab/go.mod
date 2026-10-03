module github.com/shadow-ai-capture/authlab

go 1.27

require github.com/shadow-ai-capture/device/protocol v0.0.0

// The device-side wire types are the on-disk source of truth (ADR 0010, ADR 0020). The client
// consumes them, never re-declares them, so a wire change is a compile error here.
replace github.com/shadow-ai-capture/device/protocol => ../../endpoint/protocol
