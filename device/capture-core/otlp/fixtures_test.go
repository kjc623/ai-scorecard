package otlp

import (
	"os"
	"path/filepath"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// TestClaudeCodeFixturesDecode decodes every Claude Code fixture as the OTLP JSON request its name
// says it is. Unknown fields are refused, so a misspelt OTLP field fails here.
func TestClaudeCodeFixturesDecode(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "claude-code", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures under testdata/claude-code")
	}
	for _, f := range files {
		t.Run(filepath.ToSlash(f), func(t *testing.T) {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var m proto.Message
			var empty func() bool
			if filepath.Base(f) == "metrics.json" {
				req := &colmetricspb.ExportMetricsServiceRequest{}
				m, empty = req, func() bool { return len(req.GetResourceMetrics()) == 0 }
			} else {
				req := &collogspb.ExportLogsServiceRequest{}
				m, empty = req, func() bool { return len(req.GetResourceLogs()) == 0 }
			}
			if err := protojson.Unmarshal(body, m); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if empty() {
				t.Fatal("decoded to an empty request")
			}
		})
	}
}
