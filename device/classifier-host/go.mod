module github.com/shadow-ai-capture/device/classifier-host

go 1.27

require (
	github.com/shadow-ai-capture/device/protocol v0.0.0
	golang.org/x/sys v0.48.0
	golang.org/x/text v0.41.0
)

replace github.com/shadow-ai-capture/device/protocol => ../protocol
