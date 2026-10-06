package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/attachments"
	"github.com/shadow-ai-capture/device/capture-core/classifierlink"
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/protocol"
)

// An attachment goes from the extension's frames to a label on the spooled envelope through the
// real device components: the frames decode with the protocol types, the attachment holder
// assembles and verifies the bytes, the pipeline classifies them under the observation's mode over
// the classifier link, and the classifier is the real classifier-host process with the shipped
// rules, which hands the document to its isolated parser child. The envelope carries the label and
// the descriptor, never the bytes, and is accepted the way ingest accepts it.

// realClassifier builds classifier-host, signs a release of the shipped rules and model, and
// connects to the host over the classifier link exactly as the service does.
func realClassifier(t *testing.T) *classifierlink.Client {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is needed to build classifier-host: %v", err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "classifier-host")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goTool, "build", "-o", exe, "./cmd/classifier-host")
	build.Dir = filepath.Join("..", "classifier-host")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build classifier-host: %v\n%s", err, out)
	}

	rules, err := os.ReadFile(filepath.Join("..", "classifier-host", "rules", "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	model, err := os.ReadFile(filepath.Join("..", "classifier-host", "rules", "model.json"))
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(dir, "release")
	if _, err := release.Write(rel, "integration-release-1", rules, model, priv); err != nil {
		t.Fatalf("sign the release: %v", err)
	}

	var stderr bytes.Buffer
	cl := classifierlink.New(exe, []string{"serve", "--release", rel, "--pubkey", hex.EncodeToString(pub), "--transport", "stdio"},
		&stderr, "integration", 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cl.Connect(ctx); err != nil {
		t.Fatalf("connect to classifier-host: %v\n%s", err, stderr.String())
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

type attachmentCase struct {
	Frames  []json.RawMessage `json:"frames"`
	Expects struct {
		Label string `json:"label"`
		// EnvelopeAttachment is the descriptor as the contract carries it: name, size and digest.
		EnvelopeAttachment protocol.AttachmentDescriptor `json:"envelope_attachment"`
	} `json:"expects"`
}

// replayAttachmentFrames feeds a golden case's frames through the protocol types and the holder,
// as the native session does, and returns the observation and the bytes it claimed.
func replayAttachmentFrames(t *testing.T, c attachmentCase) (protocol.ObservationMessage, []attachments.Held) {
	t.Helper()
	holder := attachments.New(time.Now)
	var obs *protocol.ObservationMessage
	for i, raw := range c.Frames {
		var msg protocol.NativeMessage
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Version != protocol.Version {
			t.Fatalf("frame %d is not a native message at version %d: %v", i, protocol.Version, err)
		}
		var err error
		switch msg.Type {
		case protocol.TypeAttachmentManifest:
			var m protocol.AttachmentManifest
			if err = json.Unmarshal(msg.Body, &m); err == nil {
				err = holder.Open(m)
			}
		case protocol.TypeAttachmentChunk:
			var ch protocol.AttachmentChunk
			if err = json.Unmarshal(msg.Body, &ch); err == nil {
				err = holder.Append(ch)
			}
		case protocol.TypeAttachmentComplete:
			var done protocol.AttachmentComplete
			if err = json.Unmarshal(msg.Body, &done); err == nil {
				_, err = holder.Complete(done)
			}
		case protocol.TypeObservation:
			var o protocol.ObservationMessage
			if err = json.Unmarshal(msg.Body, &o); err == nil {
				err = o.Validate()
				obs = &o
			}
		default:
			t.Fatalf("frame %d has unexpected type %q", i, msg.Type)
		}
		if err != nil {
			t.Fatalf("frame %d (%s): %v", i, msg.Type, err)
		}
	}
	if obs == nil {
		t.Fatal("the case has no observation frame")
	}
	held := holder.Claim(obs.ClientID, obs.Attachments)
	if holder.Pending() != 0 {
		t.Fatalf("the holder still has %d bytes after the observation claimed its attachments", holder.Pending())
	}
	return *obs, held
}

// recordingContentStore keeps what the pipeline held for upload.
type recordingContentStore struct{ held [][]byte }

func (s *recordingContentStore) Put(_ context.Context, _ string, content []byte, _ time.Time) error {
	s.held = append(s.held, append([]byte(nil), content...))
	return nil
}

type descriptorExtractor struct{ atts []dedup.Attachment }

func (e descriptorExtractor) Extract(payload []byte, _ string) (string, []dedup.Attachment, error) {
	return string(payload), e.atts, nil
}

func TestAttachmentPath_DocumentLabelsTheEnvelopeAndItsBytesStayOnTheDevice(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "attachment", "docx-payment-card.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c attachmentCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("attachment case: %v", err)
	}
	cl := realClassifier(t)
	schema := deviceSubmissionSchema(t)

	for _, mode := range []protocol.CollectionMode{protocol.ModeM1, protocol.ModeM3} {
		t.Run(string(mode), func(t *testing.T) {
			obs, held := replayAttachmentFrames(t, c)
			if len(held) != 1 || len(obs.Attachments) != 1 || held[0].Descriptor != obs.Attachments[0] {
				t.Fatalf("claimed %d attachments, want the one document the observation names", len(held))
			}
			doc := held[0].Bytes

			sp := openSpool(t)
			p := newPipeline(t, sp, nil, mode)
			p.Classifier = cl
			p.ClassifyBudget = 30 * time.Second
			store := &recordingContentStore{}
			p.Content = store

			var atts []dedup.Attachment
			for _, d := range obs.Attachments {
				atts = append(atts, dedup.Attachment{Name: d.Name, MediaType: d.MediaType, SizeBytes: d.SizeBytes, ContentDigest: d.ContentDigest})
			}
			in := toCoreObservation(obs, &countingReader{body: obs.Content}, descriptorExtractor{atts: atts})
			in.Attachments = atts
			for _, h := range held {
				in.AttachmentContent = append(in.AttachmentContent, core.AttachmentContent{MediaType: h.Descriptor.MediaType, Content: &countingReader{body: h.Bytes}})
			}
			out, err := p.Process(context.Background(), in)
			if err != nil || !out.Emitted {
				t.Fatalf("Process: %v (%+v)", err, out)
			}

			entries, err := sp.Peek(10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("spool holds %d entries (%v), want one", len(entries), err)
			}
			payload := entries[0].Payload
			var env struct {
				Labels      []protocol.Label                `json:"labels"`
				Confidence  protocol.Confidence             `json:"confidence"`
				Attachments []protocol.AttachmentDescriptor `json:"attachments"`
			}
			if err := json.Unmarshal(payload, &env); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, l := range env.Labels {
				found = found || l.Class == c.Expects.Label
			}
			if !found {
				t.Fatalf("labels = %+v (confidence %s), want %s from the document", env.Labels, env.Confidence, c.Expects.Label)
			}
			if len(env.Attachments) != 1 || env.Attachments[0] != c.Expects.EnvelopeAttachment {
				t.Fatalf("attachments = %+v, want the descriptor %+v", env.Attachments, c.Expects.EnvelopeAttachment)
			}
			for _, leak := range [][]byte{doc, []byte(base64.StdEncoding.EncodeToString(doc)), []byte("4111")} {
				if bytes.Contains(payload, leak) {
					t.Fatal("the document's bytes reached the spooled envelope")
				}
				for _, object := range store.held {
					if bytes.Contains(object, leak) {
						t.Fatal("the document's bytes reached the content store")
					}
				}
			}
			if mode == protocol.ModeM3 && (len(store.held) != 1 || !bytes.Equal(store.held[0], obs.Content)) {
				t.Fatalf("M3 held %d objects, want only the prompt", len(store.held))
			}
			if err := acceptLikeIngest(t, schema, payload); err != nil {
				t.Fatalf("ingest would refuse the envelope: %v\n%s", err, payload)
			}
		})
	}
}
