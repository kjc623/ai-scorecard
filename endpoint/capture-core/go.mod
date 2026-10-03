module github.com/shadow-ai-capture/device/capture-core

go 1.27

require (
	github.com/shadow-ai-capture/device/canon v0.0.0
	github.com/shadow-ai-capture/device/capture-spool v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
)

replace github.com/shadow-ai-capture/device/protocol => ../protocol

replace github.com/shadow-ai-capture/device/capture-spool => ../capture-spool

replace github.com/shadow-ai-capture/device/canon => ../canon
