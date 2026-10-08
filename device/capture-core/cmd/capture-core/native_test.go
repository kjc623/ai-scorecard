package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/protocol"
)

// observationFrame is an extension observation as the extension frames it.
func observationFrame(t *testing.T, clientID string, content []byte) []byte {
	t.Helper()
	body, err := json.Marshal(protocol.ObservationMessage{
		ClientID: clientID, Route: protocol.RouteExtWebRequest, ToolFingerprint: "tool-a",
		OccurredAt: time.Now().UTC(), SizeBytes: int64(len(content)) + 10,
		HasContent: len(content) > 0, Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(protocol.NativeMessage{Type: protocol.TypeObservation, Version: protocol.Version, ID: clientID, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func frameType(t *testing.T, payload []byte) string {
	t.Helper()
	var msg protocol.NativeMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("answer is not a native message: %v (%s)", err, payload)
	}
	return msg.Type
}

func refusalReason(t *testing.T, payload []byte) protocol.RefusalReason {
	t.Helper()
	var msg protocol.NativeMessage
	var r protocol.Refusal
	if json.Unmarshal(payload, &msg) != nil || msg.Type != protocol.TypeRefusal || json.Unmarshal(msg.Body, &r) != nil {
		t.Fatalf("answer is not a refusal: %s", payload)
	}
	return r.Reason
}

func TestToCoreObservationKeepsContentBehindAReader(t *testing.T) {
	m0 := toCoreObservation(protocol.ObservationMessage{Route: protocol.RouteExtDOM, ToolFingerprint: "t"})
	if m0.Content != nil {
		t.Fatal("an observation without content has a content reader")
	}
	obs := toCoreObservation(protocol.ObservationMessage{Route: protocol.RouteExtDOM, ToolFingerprint: "t", HasContent: true, Content: []byte("hi"),
		Attachments: []protocol.AttachmentDescriptor{{Name: "a.pdf", SizeBytes: 3}}})
	got, err := obs.Content.Read(context.Background())
	if err != nil || string(got) != "hi" {
		t.Fatalf("content = %q, %v", got, err)
	}
	if len(obs.Attachments) != 1 || obs.Attachments[0].Name != "a.pdf" {
		t.Fatalf("attachments = %+v", obs.Attachments)
	}
	if obs.Decision == nil || obs.Decision.Action != protocol.ActionLogged {
		t.Fatal("an observation without a decision is not recorded as logged")
	}
}

// enrolledService is a started service against a fake cloud, with a bundle in force at mode.
func enrolledService(t *testing.T, mode protocol.CollectionMode) (*service, *fakeCloud) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cloud := startFakeCloud(t, signedTestBundle(t, priv, "5", mode))
	svc, err := newService(context.Background(), testConfig(t, cloud, pub), testLogger(t))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return svc, cloud
}

func TestNativeSessionRefusesMalformedInput(t *testing.T) {
	svc, _ := enrolledService(t, protocol.ModeM1)
	s := newNativeSession(svc, svc.peerPerson(hostinfo.User{}))
	for name, tc := range map[string]struct {
		frame string
		want  protocol.RefusalReason
	}{
		"not json":          {`{`, protocol.RefusalMalformed},
		"no type":           {`{"version":1}`, protocol.RefusalMalformed},
		"unknown type":      {`{"type":"teleport","version":1}`, protocol.RefusalUnknownType},
		"wrong version":     {`{"type":"health","version":9}`, protocol.RefusalVersionMismatch},
		"bad observation":   {`{"type":"observation","version":1,"body":{"route":"proxy.tls","tool_fingerprint":"t"}}`, protocol.RefusalMalformed},
		"content unflagged": {`{"type":"observation","version":1,"body":{"route":"ext.dom","tool_fingerprint":"t","has_content":false,"content":"aGk="}}`, protocol.RefusalMalformed},
		"chunk first":       {`{"type":"attachment_chunk","version":1,"body":{"transfer_id":"x","seq":0,"data":"aGk="}}`, protocol.RefusalMalformed},
		"oversize declare":  {`{"type":"attachment_manifest","version":1,"body":{"transfer_id":"x","observation_id":"o","descriptor":{"name":"big","size_bytes":99999999999}}}`, protocol.RefusalAttachmentTooLarge},
	} {
		if got := refusalReason(t, s.Handle(context.Background(), []byte(tc.frame))); got != tc.want {
			t.Errorf("%s: refusal %q, want %q", name, got, tc.want)
		}
	}
}

// sendTo frames body as one native message of type typ and hands it to the session.
func sendTo(s *nativeSession, typ string, body any) []byte {
	raw, _ := json.Marshal(body)
	frame, _ := json.Marshal(protocol.NativeMessage{Type: typ, Version: protocol.Version, Body: raw})
	return s.Handle(context.Background(), frame)
}

// A device whose policy reads no content refuses an attachment before any byte moves.
func TestNativeSessionRefusesAttachmentsAtM0(t *testing.T) {
	svc, _ := enrolledService(t, protocol.ModeM0)
	s := newNativeSession(svc, svc.peerPerson(hostinfo.User{}))
	got := refusalReason(t, sendTo(s, protocol.TypeAttachmentManifest, protocol.AttachmentManifest{TransferID: "t1", Observation: "o1",
		Descriptor: protocol.AttachmentDescriptor{Name: "a.txt", SizeBytes: 4}}))
	if got != protocol.RefusalModeForbidsRead {
		t.Fatalf("a manifest at M0 answered %q, want %q", got, protocol.RefusalModeForbidsRead)
	}
	if s.attachments.Pending() != 0 {
		t.Fatal("a refused manifest reserved bytes")
	}
}

// An attachment is accepted before any byte moves, its chunks are contiguous and bounded by the
// declared size, and the completion closes the transfer.
func TestNativeSessionAttachmentLadder(t *testing.T) {
	svc, _ := enrolledService(t, protocol.ModeM1)
	s := newNativeSession(svc, svc.peerPerson(hostinfo.User{}))
	send := func(typ string, body any) []byte { return sendTo(s, typ, body) }
	if typ := frameType(t, send(protocol.TypeAttachmentManifest, protocol.AttachmentManifest{TransferID: "t1", Observation: "o1",
		Descriptor: protocol.AttachmentDescriptor{Name: "a.txt", SizeBytes: 4}})); typ != protocol.TypeAck {
		t.Fatalf("manifest answered %s", typ)
	}
	if got := refusalReason(t, send(protocol.TypeAttachmentChunk, protocol.AttachmentChunk{TransferID: "t1", Seq: 1, Data: []byte("ab")})); got != protocol.RefusalMalformed {
		t.Fatalf("a gap answered %q", got)
	}
	if typ := frameType(t, send(protocol.TypeAttachmentChunk, protocol.AttachmentChunk{TransferID: "t1", Seq: 0, Data: []byte("ab")})); typ != protocol.TypeAck {
		t.Fatalf("chunk 0 answered %s", typ)
	}
	if got := refusalReason(t, send(protocol.TypeAttachmentChunk, protocol.AttachmentChunk{TransferID: "t1", Seq: 1, Data: []byte("cdef")})); got != protocol.RefusalAttachmentTooLarge {
		t.Fatalf("bytes past the declared size answered %q", got)
	}
	if got := refusalReason(t, send(protocol.TypeAttachmentComplete, protocol.AttachmentComplete{TransferID: "t1"})); got != protocol.RefusalMalformed {
		t.Fatalf("completing a refused transfer answered %q", got)
	}
}

// cardClassifier labels content carrying a test card number, and records what it was asked.
type cardClassifier struct {
	mu   sync.Mutex
	reqs []protocol.ClassifyRequest
}

func (c *cardClassifier) Classify(_ context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	resp := protocol.ClassifyResponse{Labels: []protocol.Label{}, ClassifierVersion: "test-1", Confidence: protocol.ConfidenceHigh}
	if bytes.Contains(req.Content, []byte("4111 1111 1111 1111")) {
		resp.Labels = append(resp.Labels, protocol.Label{Class: "payment_card", Score: 0.99, RuleID: "pan"})
	}
	return resp, nil
}

func (c *cardClassifier) requests() []protocol.ClassifyRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.ClassifyRequest(nil), c.reqs...)
}

// transferAttachment sends one attachment as the extension does: manifest, one chunk, completion.
// It returns the descriptor the observation then names it by.
func transferAttachment(t *testing.T, s *nativeSession, transfer, observation, name string, data []byte) protocol.AttachmentDescriptor {
	t.Helper()
	d := protocol.AttachmentDescriptor{Name: name, MediaType: "text/plain", SizeBytes: int64(len(data))}
	for _, step := range []struct {
		typ  string
		body any
	}{
		{protocol.TypeAttachmentManifest, protocol.AttachmentManifest{TransferID: transfer, Observation: observation, Descriptor: d}},
		{protocol.TypeAttachmentChunk, protocol.AttachmentChunk{TransferID: transfer, Seq: 0, Data: data}},
		{protocol.TypeAttachmentComplete, protocol.AttachmentComplete{TransferID: transfer, Chunks: 1}},
	} {
		if answer := sendTo(s, step.typ, step.body); frameType(t, answer) != protocol.TypeAck {
			t.Fatalf("%s answered %s", step.typ, answer)
		}
	}
	sum := sha256.Sum256(data)
	d.ContentDigest = "sha256:" + hex.EncodeToString(sum[:])
	return d
}

// The bytes transferred for an observation are classified under its mode with their media type,
// the labels join the event's classification, the descriptor travels in the envelope, and the
// bytes themselves are neither in the spool nor still held.
func TestNativeSessionClassifiesAttachmentsAndDropsTheBytes(t *testing.T) {
	svc, _ := enrolledService(t, protocol.ModeM1)
	cl := &cardClassifier{}
	svc.pipe.Classifier = cl
	s := newNativeSession(svc, svc.peerPerson(hostinfo.User{}))

	doc := []byte("Invoice 7. Please charge card 4111 1111 1111 1111 for the balance.")
	d := transferAttachment(t, s, "t1", "obs-att", "invoice.txt", doc)
	stray := transferAttachment(t, s, "t2", "obs-other", "other.txt", []byte("not for this observation"))

	body, _ := json.Marshal(protocol.ObservationMessage{
		ClientID: "obs-att", Route: protocol.RouteExtWebRequest, ToolFingerprint: "tool-a",
		OccurredAt: time.Now().UTC(), SizeBytes: 40, HasContent: true, Content: []byte("summarise the attached invoice"),
		Attachments: []protocol.AttachmentDescriptor{d},
	})
	frame, _ := json.Marshal(protocol.NativeMessage{Type: protocol.TypeObservation, Version: protocol.Version, ID: "obs-att", Body: body})
	if answer := s.Handle(context.Background(), frame); frameType(t, answer) != protocol.TypeAck {
		t.Fatalf("observation answered %s", answer)
	}

	var attached *protocol.ClassifyRequest
	for _, req := range cl.requests() {
		if bytes.Equal(req.Content, doc) {
			attached = &req
		}
	}
	if attached == nil || attached.MediaType != "text/plain" || attached.Mode != protocol.ModeM1 {
		t.Fatalf("the attachment was not classified with its media type under the observation's mode: %+v", attached)
	}

	entries, err := svc.spool.sp.Peek(10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("spool holds %d entries (%v), want the one observation", len(entries), err)
	}
	payload := entries[0].Payload
	var env struct {
		Labels      []protocol.Label                `json:"labels"`
		Attachments []protocol.AttachmentDescriptor `json:"attachments"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Labels) != 1 || env.Labels[0].Class != "payment_card" {
		t.Fatalf("labels = %+v, want the attachment's payment_card", env.Labels)
	}
	want := protocol.AttachmentDescriptor{Name: d.Name, SizeBytes: d.SizeBytes, ContentDigest: d.ContentDigest}
	if len(env.Attachments) != 1 || env.Attachments[0] != want {
		t.Fatalf("attachments = %+v, want the descriptor %+v", env.Attachments, want)
	}
	if bytes.Contains(payload, []byte("4111")) || bytes.Contains(payload, []byte("Invoice 7")) {
		t.Fatal("attachment bytes reached the spool")
	}

	// The other observation's transfer stays held until its observation claims it or it expires;
	// a claim naming a different digest classifies nothing and frees it.
	if got := s.attachments.Pending(); got != stray.SizeBytes {
		t.Fatalf("held bytes = %d, want only the unclaimed transfer's %d", got, stray.SizeBytes)
	}
	if held := s.attachments.Claim("obs-other", []protocol.AttachmentDescriptor{d}); len(held) != 0 || s.attachments.Pending() != 0 {
		t.Fatalf("a claim naming another digest returned %d attachments and left %d bytes", len(held), s.attachments.Pending())
	}
}

// The extension is handed the verified bundle in force, and told when it already has it.
func TestNativeSessionPolicySync(t *testing.T) {
	svc, _ := enrolledService(t, protocol.ModeM1)
	s := newNativeSession(svc, svc.peerPerson(hostinfo.User{}))
	ask := func(known string) protocol.PolicyBundleMessage {
		body, _ := json.Marshal(protocol.PolicySyncRequest{KnownVersion: known})
		frame, _ := json.Marshal(protocol.NativeMessage{Type: protocol.TypePolicySync, Version: protocol.Version, Body: body})
		var msg protocol.NativeMessage
		var pb protocol.PolicyBundleMessage
		if err := json.Unmarshal(s.Handle(context.Background(), frame), &msg); err != nil || json.Unmarshal(msg.Body, &pb) != nil {
			t.Fatalf("policy_sync answer unreadable: %v", err)
		}
		return pb
	}
	if pb := ask(""); pb.PolicyVersion != "5" || len(pb.Bundle) == 0 || pb.Unchanged {
		t.Fatalf("first sync = %+v, want version 5 with the bundle", pb)
	}
	if pb := ask("5"); !pb.Unchanged || (len(pb.Bundle) != 0 && string(pb.Bundle) != "null") {
		t.Fatalf("sync at the current version = %+v, want unchanged with no bundle", pb)
	}
}

// The relay a browser starts and the service talk over the real native messaging endpoint (a
// named pipe on Windows, a Unix socket elsewhere): a frame written to the relay's stdin reaches
// the service, the answer comes back on its stdout, and the observation is attributed to the
// account of the process that connected.
func TestRelayCarriesFramesToTheServiceOverTheEndpoint(t *testing.T) {
	trustTestServer(t)
	svc, _ := enrolledService(t, protocol.ModeM1)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	relayDone := make(chan error, 1)
	go func() { relayDone <- runRelay(stdinR, stdoutW, dialNative) }()

	if err := localipc.WriteFrame(stdinW, observationFrame(t, "obs-relay", []byte("summarise this contract"))); err != nil {
		t.Fatal(err)
	}
	answer := make(chan []byte, 1)
	go func() {
		payload, _ := localipc.ReadFrame(stdoutR)
		answer <- payload
	}()
	select {
	case payload := <-answer:
		if typ := frameType(t, payload); typ != protocol.TypeAck {
			t.Fatalf("the relay returned %s: %s", typ, payload)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no answer came back through the relay")
	}

	entries, err := svc.spool.sp.Peek(10)
	if err != nil || len(entries) == 0 {
		t.Fatalf("spool holds %d entries (%v), want the relayed observation", len(entries), err)
	}
	var env struct {
		UserRef     string `json:"user_ref"`
		SubjectName string `json:"subject_name"`
		Source      string `json:"source"`
	}
	if err := json.Unmarshal(entries[len(entries)-1].Payload, &env); err != nil {
		t.Fatal(err)
	}
	me, err := hostinfo.SystemUserSources().Process()
	if err != nil {
		t.Fatal(err)
	}
	if env.Source != string(protocol.RouteExtWebRequest) || !strings.HasPrefix(env.UserRef, "u_") {
		t.Fatalf("relayed envelope = %+v, want an attributed extension observation", env)
	}
	if want := me.DisplayName(); want != "" && env.SubjectName != want {
		t.Fatalf("subject_name = %q, want the connecting account %q", env.SubjectName, want)
	}
	if svc.native.Clients() != 1 {
		t.Fatalf("connected relays = %d, want 1", svc.native.Clients())
	}

	// The browser closing its end ends the relay cleanly.
	_ = stdinW.Close()
	select {
	case err := <-relayDone:
		if err != nil {
			t.Fatalf("relay ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not end when the browser closed stdin")
	}
}

// The relay hands frames only to an endpoint served by the service's account.
func TestRelayRefusesAnUntrustedServer(t *testing.T) {
	enrolledService(t, protocol.ModeM0)
	distrustTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if conn, err := dialNative(ctx); err == nil {
		conn.Close()
		t.Fatal("the relay connected to an endpoint not served by the service account")
	}
}
