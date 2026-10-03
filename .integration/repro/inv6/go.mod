module shadow-ai-capture.invalid/integration/repro/inv6

go 1.27

require (
	github.com/shadow-ai-capture/device/capture-core v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
)

replace github.com/shadow-ai-capture/device/capture-core => ../../../endpoint/capture-core

replace github.com/shadow-ai-capture/device/protocol => ../../../endpoint/protocol
