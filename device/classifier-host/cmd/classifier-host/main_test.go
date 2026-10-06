package main

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/protocol"
)

// With CLASSIFIER_HOST_COMMAND set, the test binary is the classifier-host command, so the tests
// can start it the way capture-core does, and its parser children are this binary too.
func TestMain(m *testing.M) {
	if os.Getenv("CLASSIFIER_HOST_COMMAND") == "1" {
		os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// signedRelease writes the shipped rules and model as a release and returns its directory and
// public key.
func signedRelease(t *testing.T) (string, string) {
	t.Helper()
	priv, pub := testrig.Key(t)
	dir := filepath.Join(t.TempDir(), "classifier")
	if _, err := release.Write(dir, "e2e-1", testrig.Rules(t), testrig.Model(t), priv); err != nil {
		t.Fatal(err)
	}
	return dir, hex.EncodeToString(pub)
}

type child struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *bytes.Buffer
}

func start(t *testing.T, args ...string) *child {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: exec.Command(exe, args...), stderr: &bytes.Buffer{}}
	c.cmd.Env = append(os.Environ(), "CLASSIFIER_HOST_COMMAND=1")
	c.cmd.Stderr = c.stderr
	if c.stdin, err = c.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if c.stdout, err = c.cmd.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.cmd.Process.Kill(); c.cmd.Wait() })
	return c
}

func (c *child) exchange(t *testing.T, request, response any) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(c.stdin, body); err != nil {
		t.Fatalf("writing to the host: %v (stderr: %s)", err, c.stderr)
	}
	payload, err := protocol.ReadFrameChecked(c.stdout)
	if err != nil {
		t.Fatalf("reading from the host: %v (stderr: %s)", err, c.stderr)
	}
	if err := json.Unmarshal(payload, response); err != nil {
		t.Fatalf("the host's answer is not JSON: %v", err)
	}
}

// wait returns the exit code once the host exits, failing the test if it does not within 10s.
func (c *child) wait(t *testing.T) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
		return c.cmd.ProcessState.ExitCode()
	case <-time.After(10 * time.Second):
		t.Fatal("the host did not exit")
		return -1
	}
}

func docx(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"[Content_Types].xml": `<Types/>`,
		"word/document.xml":   `<w:document><w:body><w:p>` + text + `</w:p></w:body></w:document>`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestServeOverStdio runs the host exactly as capture-core does and classifies a prompt and a
// document; the document is parsed by a parser child of the host.
func TestServeOverStdio(t *testing.T) {
	dir, pub := signedRelease(t)
	host := start(t, "serve", "--release", dir, "--pubkey", pub, "--transport", "stdio", "--parser-timeout", "5s")

	var hs protocol.HandshakeResponse
	host.exchange(t, protocol.HandshakeRequest{CoreVersion: "capture-core/test", ProtocolVersion: protocol.Version}, &hs)
	if !hs.OK || hs.ClassifierVersion != "e2e-1" {
		t.Fatalf("handshake: %+v", hs)
	}
	cases := []struct {
		name      string
		mediaType string
		content   []byte
		class     string
	}{
		{"prompt", "text/plain", []byte("please charge card 4111 1111 1111 1111"), "payment_card"},
		{"document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			docx(t, "-----BEGIN RSA PRIVATE KEY----- MIIEow"), "credential"},
		{"nothing to find", "text/plain", []byte("the weather is mild"), ""},
	}
	for _, tc := range cases {
		var resp protocol.ClassifyResponse
		host.exchange(t, protocol.ClassifyRequest{Content: tc.content, Mode: protocol.ModeM1, MediaType: tc.mediaType, BudgetMS: 2000}, &resp)
		if err := resp.Validate(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := ""
		if len(resp.Labels) > 0 {
			got = resp.Labels[0].Class
		}
		if resp.Confidence != protocol.ConfidenceHigh || got != tc.class || resp.ClassifierVersion != "e2e-1" {
			t.Errorf("%s: %+v, want class %q", tc.name, resp, tc.class)
		}
	}

	host.stdin.Close()
	if code := host.wait(t); code != 0 {
		t.Fatalf("the host exited %d when its stdin closed (stderr: %s)", code, host.stderr)
	}
	if !strings.Contains(host.stderr.String(), `"release":"e2e-1"`) {
		t.Errorf("the host did not log the release it serves: %s", host.stderr)
	}
}

func TestServeRefusesToStartWithoutAVerifiedRelease(t *testing.T) {
	dir, pub := signedRelease(t)
	_, otherPub := testrig.Key(t)
	cases := map[string][]string{
		"another key":         {"--release", dir, "--pubkey", hex.EncodeToString(otherPub)},
		"no key":              {"--release", dir},
		"malformed key":       {"--release", dir, "--pubkey", "not-hex"},
		"no release":          {"--pubkey", pub},
		"missing release":     {"--release", filepath.Join(dir, "absent"), "--pubkey", pub},
		"unsupported channel": {"--release", dir, "--pubkey", pub, "--transport", "tcp"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			host := start(t, append([]string{"serve"}, args...)...)
			if code := host.wait(t); code == 0 {
				t.Fatalf("the host started (stderr: %s)", host.stderr)
			}
			if !strings.Contains(host.stderr.String(), `"level":"ERROR"`) {
				t.Errorf("the refusal was not logged: %s", host.stderr)
			}
		})
	}
}

func TestUnknownCommandIsRefused(t *testing.T) {
	for _, args := range [][]string{nil, {"classify"}, {"release"}} {
		if code := run(args, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
			t.Errorf("%v exited %d, want 2", args, code)
		}
	}
}
