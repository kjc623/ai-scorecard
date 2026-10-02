module github.com/shadow-ai-capture/device/integration

go 1.27

// Cross-component integration tests for the device tier. This module imports every device
// component and wires them together for real; it is the only place that does, which is the point.
// Each component's own suite proves its own behaviour against its own fakes - none of them can
// prove that the pieces compose, and that is what this module exists to check.
//
// Only test files belong here. If a piece of production logic ever needs to move into this module,
// it belongs in a component instead.
require (
	github.com/shadow-ai-capture/device/capture-core v0.0.0
	github.com/shadow-ai-capture/device/capture-spool v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
)

replace github.com/shadow-ai-capture/device/protocol => ../protocol

replace github.com/shadow-ai-capture/device/capture-core => ../capture-core

replace github.com/shadow-ai-capture/device/capture-spool => ../capture-spool
