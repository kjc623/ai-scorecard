//go:build !windows

package toolconfig

// MachineEnvironment is nil here: the agent does not relocate local model servers on macOS and
// Linux yet.
func MachineEnvironment() MachineEnv { return nil }
