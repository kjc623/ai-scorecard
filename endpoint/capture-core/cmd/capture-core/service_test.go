package main

import "testing"

// --service selects service mode, carries the service name, and is exclusive with the other mode
// flags. The flag is accepted on every platform; a non-Windows host refuses it at run time (see
// service_other_test.go), so the flag set stays one shape.
func TestServiceModeParses(t *testing.T) {
	cfg, mode, err := parseFlags([]string{
		"--service", "--service-name", "ShadowAICapture",
		"--spool-dir", t.TempDir(),
		"--spool-key", t.TempDir() + "/spool.key",
		"--device-id", "11111111-1111-4111-8111-111111111111",
		"--tenant-id", "22222222-2222-4222-8222-222222222222",
	})
	if err != nil {
		t.Fatalf("parseFlags(--service): %v", err)
	}
	if !mode.service {
		t.Fatal("--service did not select service mode")
	}
	if cfg.ServiceName != "ShadowAICapture" {
		t.Fatalf("service name = %q, want ShadowAICapture", cfg.ServiceName)
	}
}

func TestServiceModeIsExclusiveWithOtherModes(t *testing.T) {
	if _, _, err := parseFlags([]string{"--service", "--print-config"}); err == nil {
		t.Fatal("--service with --print-config was accepted; the modes must be exclusive")
	}
}
