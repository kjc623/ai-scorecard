module shadow-ai-capture.invalid/integration/repro/inv4

go 1.27

require (
	github.com/shadow-ai-capture/device/capture-spool v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
)

replace github.com/shadow-ai-capture/device/capture-spool => ../../../endpoint/capture-spool

replace github.com/shadow-ai-capture/device/protocol => ../../../endpoint/protocol
