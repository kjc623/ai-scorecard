package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/protocol"
)

// healthChannel is the device's health channel: per-provider coverage rows in the shape
// protocol.HealthReport defines, the closed seven counters, the spool's depth and drop counters,
// and whatever the extension last reported about itself.
//
// It is written as JSON lines to a file because the production transport (POST /v1/health, upserted
// by key) belongs to the ingest client, which is a separate component and is not part of this
// binary. The *content* is the contract shape either way, and the selftest asserts it.
type healthChannel struct {
	cfg Config
	log *slog.Logger
	svc *service

	mu        sync.Mutex
	extension *protocol.HealthReport
	writes    int
	lastErr   error
	lastAt    time.Time
	// publishes counts successful report POSTs and lastPublishErr is the most recent failure, so the
	// snapshot can say whether the heartbeat is reaching the control plane (C23/C25).
	publishes      int
	lastPublishAt  time.Time
	lastPublishErr error

	stop chan struct{}
	wg   sync.WaitGroup
}

func newHealthChannel(cfg Config, log *slog.Logger, svc *service) *healthChannel {
	return &healthChannel{cfg: cfg, log: log, svc: svc, stop: make(chan struct{})}
}

// canPublish reports whether a heartbeat can be sent: it needs a configured device endpoint (a
// drainer) to send to. A local/offline run writes only the health file.
func (h *healthChannel) canPublish() bool {
	return h.svc != nil && h.svc.drainer != nil
}

func (h *healthChannel) Start(ctx context.Context) {
	// The ticker runs when there is anywhere to put the snapshot — a file, a control plane, or both.
	// Starting it unconditionally would spend a goroutine on a local run with health disabled.
	if h.cfg.HealthFile == "" && !h.canPublish() {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		// One report at startup, so a device that has just come up is visible before the first
		// interval elapses. It is best-effort: a failure is recorded and the loop retries.
		h.tick(ctx)
		t := time.NewTicker(h.cfg.HealthInterval)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				h.tick(ctx)
			}
		}
	}()
}

// tick writes the snapshot wherever it is configured to go: the local health file, the control
// plane's POST /v1/health, or both. One failing does not skip the other.
func (h *healthChannel) tick(ctx context.Context) {
	if h.cfg.HealthFile != "" {
		if err := h.write(); err != nil {
			h.log.Warn("health channel write failed", "error", err)
		}
	}
	if h.canPublish() {
		h.publish(ctx)
	}
}

// publish sends one heartbeat. It is the endpoint half of docs/04 §3.7: an idle device that still
// reports is `reporting`, not mistaken for a dead one.
func (h *healthChannel) publish(ctx context.Context) {
	req := h.healthRequest()
	if len(req.Collectors) == 0 {
		// A device with no coverage rows has nothing to report; posting an empty report would be
		// refused by the server, so say so locally rather than manufacture an error.
		h.log.Warn("health: no collector rows to report; heartbeat skipped")
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, healthPublishTimeout)
	defer cancel()
	if _, err := h.svc.drainer.ReportHealth(callCtx, req); err != nil {
		h.mu.Lock()
		h.lastPublishErr = err
		h.mu.Unlock()
		h.log.Warn("health: heartbeat not delivered", "error", err)
		return
	}
	h.mu.Lock()
	h.publishes++
	h.lastPublishAt = time.Now()
	h.lastPublishErr = nil
	h.mu.Unlock()
}

// healthPublishTimeout bounds one heartbeat. The drainer's own HTTP client has a 20 s timeout; a
// shorter deadline here keeps a slow control plane from delaying the next tick.
const healthPublishTimeout = 15 * time.Second

func (h *healthChannel) Stop() {
	select {
	case <-h.stop:
		return
	default:
	}
	close(h.stop)
	h.wg.Wait()
}

// SetExtensionReport records the extension's own coverage row (§3.4: a failed connect is reported
// as core `absent` plus extension-side `degraded`, never as "no observations").
func (h *healthChannel) SetExtensionReport(rep protocol.HealthReport) {
	h.mu.Lock()
	defer h.mu.Unlock()
	copied := rep
	h.extension = &copied
}

// healthSnapshot is the device-level health document. The per-provider rows inside it are exactly
// protocol.HealthReport; the envelope is this binary's, because the device-level row the document
// describes (master §4.4) has no wire type in protocol yet.
type healthSnapshot struct {
	DeviceID       string                  `json:"device_id"`
	IdentitySource string                  `json:"identity_source"`
	AgentVersion   string                  `json:"agent_version"`
	GeneratedAt    time.Time               `json:"generated_at"`
	PolicyVersion  string                  `json:"policy_version,omitempty"`
	PolicyOutcome  string                  `json:"policy_outcome"`
	PolicyCause    string                  `json:"policy_cause,omitempty"`
	Classification classifierStatus        `json:"classifier"`
	Reports        []protocol.HealthReport `json:"reports"`
	Extension      *protocol.HealthReport  `json:"extension,omitempty"`
	Spool          protocol.SpoolStats     `json:"spool"`
	Drain          *drainStatus            `json:"drain,omitempty"`
	NamedGaps      []string                `json:"named_gaps,omitempty"`
	// LoopbackPortConflicts is the sticky half of §6.2 rule 5: how many times a port was found held
	// by another process since start. The provider's *state* is present-tense (a recovered port is
	// not `tampered`, because tampered is the only state that raises a security finding), and this is
	// where the history lives — visible without mislabelling a working port.
	LoopbackPortConflicts int    `json:"loopback_port_conflicts,omitempty"`
	Note                  string `json:"note"`
	// Heartbeat reporting state: whether the periodic POST /v1/health is reaching the control plane.
	// It is device-level and has no schema column, so it travels in the health document (docs/02 §9).
	HeartbeatPublished int       `json:"heartbeat_published,omitempty"`
	LastHeartbeatAt    time.Time `json:"last_heartbeat_at,omitempty"`
	LastHeartbeatError string    `json:"last_heartbeat_error,omitempty"`
}

type classifierStatus struct {
	Connected         bool   `json:"connected"`
	ClassifierVersion string `json:"classifier_version"`
	DegradedDetail    string `json:"degraded_detail,omitempty"`
}

// drainStatus is the device-to-cloud drain's health surface: the same state + error_code (detail)
// vocabulary the coverage rows use, so a failing drain is visible rather than silent (C23/C25). It
// is part of the device-level health document, not a provider row, because the drain is a transport
// rather than a collection route.
type drainStatus struct {
	State       protocol.CollectorState `json:"state"`
	Detail      protocol.Detail         `json:"detail,omitempty"`
	LastSuccess *time.Time              `json:"last_success_at,omitempty"`
	Endpoint    string                  `json:"endpoint,omitempty"`
	Enrolled    bool                    `json:"enrolled"`
}

// Snapshot renders the current health. It never blocks on a provider: Health() is required to be
// non-blocking, and the counters are read from the health channel only — never as events (§4.3).
func (h *healthChannel) Snapshot() healthSnapshot {
	policyVersion := ""
	outcome := string(h.svc.result.Outcome)
	cause := string(h.svc.result.Cause)
	if b := h.svc.currentBundle(); b != nil {
		policyVersion = b.Version
	}

	// The device identity is the issued one (credential when a drain is configured, the flags for
	// a local/offline run). The flags are only a display fallback when no identity is issued yet.
	var id core.Identity
	issued := false
	if h.svc.pipe != nil {
		id, issued = h.svc.pipe.Identity()
	}
	deviceID := id.DeviceID
	if deviceID == "" {
		deviceID = h.cfg.DeviceID
	}
	identitySource := "local"
	if strings.TrimSpace(h.cfg.DeviceEndpoint) != "" {
		if issued {
			identitySource = "credential"
		} else {
			identitySource = "unresolved"
		}
	}

	reports, errs := h.svc.reg.Reports(deviceID, policyVersion)
	for _, err := range errs {
		h.log.Warn("a health row did not validate; it is sent anyway", "error", err)
	}

	h.mu.Lock()
	ext := h.extension
	h.mu.Unlock()
	if ext != nil {
		reports = append(reports, *ext)
	}

	connected, classVersion, detail := h.svc.host.status()
	snap := healthSnapshot{
		DeviceID:       deviceID,
		IdentitySource: identitySource,
		AgentVersion:   version,
		GeneratedAt:    time.Now().UTC(),
		PolicyVersion:  policyVersion,
		PolicyOutcome:  outcome,
		PolicyCause:    cause,
		Classification: classifierStatus{
			Connected:         connected,
			ClassifierVersion: classVersion,
			DegradedDetail:    string(detail),
		},
		Reports:   reports,
		Extension: ext,
		Spool:     h.svc.sink.Stats(),
		Note:      "per-provider rows are protocol.HealthReport; the device-level row and the spool counters are carried here because protocol has no device-level type",
	}
	if h.svc.drainer != nil {
		st := h.svc.drainer.Status()
		ds := &drainStatus{
			State:    st.State,
			Detail:   st.Detail,
			Endpoint: st.Endpoint,
			Enrolled: st.Enrolled,
		}
		if !st.LastSuccess.IsZero() {
			t := st.LastSuccess
			ds.LastSuccess = &t
		}
		snap.Drain = ds
	}
	if h.svc.detect == nil {
		snap.NamedGaps = append(snap.NamedGaps, "proc.detect: no coverage row (provider not started on this host)")
	}
	if !h.cfg.EnableTLS {
		snap.NamedGaps = append(snap.NamedGaps, "proxy.tls: no coverage row (provider disabled by configuration)")
	}
	if h.cfg.ClassifierAddress == "" && h.cfg.ClassifierRelease == "" {
		snap.NamedGaps = append(snap.NamedGaps, "classifier-host: rules-only fallback, confidence=degraded on every classified event")
	}
	if !h.cfg.EnableLoopback {
		snap.NamedGaps = append(snap.NamedGaps, "proxy.loopback: no coverage row (provider disabled by configuration)")
	}
	if h.svc.broker != nil {
		snap.LoopbackPortConflicts = h.svc.broker.PortConflicts()
	}
	h.mu.Lock()
	snap.HeartbeatPublished = h.publishes
	if !h.lastPublishAt.IsZero() {
		snap.LastHeartbeatAt = h.lastPublishAt
	}
	if h.lastPublishErr != nil {
		snap.LastHeartbeatError = h.lastPublishErr.Error()
	}
	h.mu.Unlock()
	return snap
}

// collectorCodeByRoute maps a collection route onto the ref.collector code the coverage tables key
// on (docs/01 §4.3's "the collector name must come from ref.collector"). Routes name what was
// observed; collectors name the component that observed it, and the browser routes share the one
// extension collector.
var collectorCodeByRoute = map[string]string{
	"ext.web_request":  "capture_extension",
	"ext.page_context": "capture_extension",
	"ext.dom":          "capture_extension",
	"proxy.tls":        "egress_proxy",
	"proxy.loopback":   "loopback_broker",
	"cli.shim":         "cli_shim",
	"proc.detect":      "process_detector",
}

// collectorCode normalises a route name or an extension-style name to the ref.collector code.
func collectorCode(name string) string {
	if code, ok := collectorCodeByRoute[name]; ok {
		return code
	}
	return strings.ReplaceAll(name, "-", "_")
}

// healthRequest renders the protocol.HealthRequest the control plane's POST /v1/health accepts. The
// collector names are canonicalised to ref.collector codes; a report naming a route the coverage
// layer does not know would be refused by the server (docs/01 §4.3).
func (h *healthChannel) healthRequest() protocol.HealthRequest {
	snap := h.Snapshot()
	seen := map[string]bool{}
	collectors := make([]protocol.HealthReport, 0, len(snap.Reports)+1)
	add := func(reps []protocol.HealthReport) {
		for _, rep := range reps {
			code := collectorCode(rep.Collector)
			if seen[code] {
				continue
			}
			seen[code] = true
			rep.Collector = code
			// The device is the authenticated principal; carrying it per collector inside the
			// report would be a second, redundant identity claim (docs/02 §5.4).
			rep.DeviceID = ""
			collectors = append(collectors, rep)
		}
	}
	add(snap.Reports)
	if snap.Extension != nil {
		add([]protocol.HealthReport{*snap.Extension})
	}
	// classifier-host is a component rather than a collection route, and ref.collector knows it, so
	// it reports its own coverage row (docs/01 §4.6: it is "reported separately because C23 and C25
	// apply to it too"). A device with no host reports degraded, which is the truth the coverage
	// layer should see.
	add([]protocol.HealthReport{classifierReport(snap)})

	req := protocol.HealthRequest{
		SchemaVersion:       protocol.HealthSchemaVersion,
		ReportedAt:          snap.GeneratedAt,
		AgentVersion:        snap.AgentVersion,
		PolicyBundleVersion: snap.PolicyVersion,
		Spool: protocol.SpoolHealth{
			DepthEvents:   int64(snap.Spool.Depth),
			SpoolBytes:    snap.Spool.UsedBytes,
			DroppedTotal:  snap.Spool.DroppedTotal,
			RejectedTotal: snap.Spool.RejectedTotal,
		},
		Collectors: collectors,
	}
	if !snap.Spool.OldestSpooledAt.IsZero() {
		t := snap.Spool.OldestSpooledAt
		req.Spool.OldestSpooledAt = &t
	}
	return req
}

// classifierReport turns the classifier-host status into a coverage row. `connected` is the same
// positive observation the health document carries, so the row cannot claim more than the document.
func classifierReport(snap healthSnapshot) protocol.HealthReport {
	state := protocol.StateDegraded
	if snap.Classification.Connected {
		state = protocol.StateHealthy
	}
	rep := protocol.NewHealthReport("", "classifier_host", snap.Classification.ClassifierVersion, snap.GeneratedAt)
	rep.State = state
	rep.Detail = protocol.Detail(snap.Classification.DegradedDetail)
	return rep
}

// write appends one JSON line to the health file.
func (h *healthChannel) write() error {
	snap := h.Snapshot()
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if err := ensureDir(h.cfg.HealthFile); err != nil {
		return err
	}
	f, err := os.OpenFile(h.cfg.HealthFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	h.mu.Lock()
	h.writes++
	h.lastAt = time.Now()
	h.lastErr = nil
	h.mu.Unlock()
	return nil
}

// PrintSnapshot writes the snapshot to stdout as indented JSON. This is what `--print-config`-style
// diagnostics and the selftest use to show the health channel without an ingest endpoint.
func (h *healthChannel) PrintSnapshot(w *os.File) error {
	raw, err := json.MarshalIndent(h.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", raw)
	return err
}
