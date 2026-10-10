//go:build !windows

package main

// platformAgentUpdate: the agent updates itself on Windows only; elsewhere the MDM's package
// manager replaces it.
func platformAgentUpdate() (string, agentInstaller) { return "", nil }
