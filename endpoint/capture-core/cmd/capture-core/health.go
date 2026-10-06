package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// Health reporting cadence. The control plane can slow the fleet with next_report_after_s, within
// these bounds.
const (
	healthInterval       = time.Minute
	minHealthInterval    = 30 * time.Second
	maxHealthInterval    = time.Hour
	healthPublishTimeout = 15 * time.Second
)

// healthChannel reports the device's health: per-collector coverage rows with the closed counter
// set, the spool's depth and drop counters, and the extension's own row. Each tick posts the report
// to POST /v1/health and writes the full snapshot to health.json in the state directory, where an
// administrator can read what the agent last saw.
type healthChannel struct {
	svc *service

	mu             sync.Mutex
	extension      *protocol.HealthReport
	interval       time.Duration
	publishes      int
	lastPublishAt  time.Time
	lastPublishErr error

	stop chan struct{}
	wg   sync.WaitGroup
}

func newHealthChannel(svc *service) *healthChannel {
	return &healthChannel{svc: svc, interval: healthInterval, stop: make(chan struct{})}
}

func (h *healthChannel) Start(ctx context.Context) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		for {
			h.tick(ctx)
			h.mu.Lock()
			wait := h.interval
			h.mu.Unlock()
			t := time.NewTimer(wait)
			select {
			case <-h.stop:
				t.Stop()
				return
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
}

func (h *healthChannel) Stop() {
	select {
	case <-h.stop:
		return
	default:
	}
	close(h.stop)
	h.wg.Wait()
}

// tick sends one report and writes the snapshot. A failure of one does not skip the other.
func (h *healthChannel) tick(ctx context.Context) {
	h.publish(ctx)
	raw, err := json.MarshalIndent(h.Snapshot(), "", "  ")
	if err == nil {
		err = state.WriteFile(h.svc.dir.Path(state.HealthFile), raw)
	}
	if err != nil {
		h.svc.log.Warn("health: writing the snapshot failed", "error", err)
	}
}

// publish sends one heartbeat, so an idle device still reports and an absence of events is never
// read as a dead device.
func (h *healthChannel) publish(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, healthPublishTimeout)
	defer cancel()
	resp, err := h.svc.drainer.ReportHealth(callCtx, h.healthRequest())
	h.mu.Lock()
	if err != nil {
		h.lastPublishErr = err
		h.mu.Unlock()
		h.svc.log.Warn("health: report not delivered", "error", err)
		return
	}
	h.publishes++
	h.lastPublishAt = time.Now()
	h.lastPublishErr = nil
	if resp.NextReportAfterS > 0 {
		h.interval = min(max(time.Duration(resp.NextReportAfterS)*time.Second, minHealthInterval), maxHealthInterval)
	}
	h.mu.Unlock()
	// The response restates the tenant's device identity setting, which is how a device enrolled
	// under one setting learns the tenant changed it.
	h.svc.adoptDeviceIdentity(resp.DeviceIdentity)
}

// SetExtensionReport records the extension's own coverage row.
func (h *healthChannel) SetExtensionReport(rep protocol.HealthReport) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.extension = &rep
}

// healthSnapshot is the device-level health document written to health.json. The per-collector
// rows are protocol.HealthReport.
type healthSnapshot struct {
	DeviceID       string                  `json:"device_id,omitempty"`
	Enrolled       bool                    `json:"enrolled"`
	AgentVersion   string                  `json:"agent_version"`
	GeneratedAt    time.Time               `json:"generated_at"`
	Hostname       string                  `json:"hostname,omitempty"`
	ManagedState   string                  `json:"managed_state,omitempty"`
	CollectionMode string                  `json:"collection_mode,omitempty"`
	DeviceIdentity string                  `json:"device_identity"`
	UserRefSource  string                  `json:"user_ref_source"`
	PolicyVersion  string                  `json:"policy_version,omitempty"`
	PolicyOutcome  string                  `json:"policy_outcome,omitempty"`
	PolicyCause    string                  `json:"policy_cause,omitempty"`
	PolicyFetch    *policySyncStatus       `json:"policy_fetch,omitempty"`
	Classifier     classifierStatus        `json:"classifier"`
	Reports        []protocol.HealthReport `json:"reports"`
	Extension      *protocol.HealthReport  `json:"extension,omitempty"`
	Spool          protocol.SpoolStats     `json:"spool"`
	ContentHeld    int                     `json:"content_held_objects"`
	Drain          drainStatus             `json:"drain"`
	NativeClients  int                     `json:"native_clients"`
	Heartbeats     int                     `json:"heartbeats_published"`
	LastHeartbeat  *time.Time              `json:"last_heartbeat_at,omitempty"`
	HeartbeatError string                  `json:"last_heartbeat_error,omitempty"`
}

type classifierStatus struct {
	Connected      bool   `json:"connected"`
	Version        string `json:"version"`
	DegradedDetail string `json:"degraded_detail,omitempty"`
}

type drainStatus struct {
	State       protocol.CollectorState `json:"state"`
	Detail      protocol.Detail         `json:"detail,omitempty"`
	LastSuccess *time.Time              `json:"last_success_at,omitempty"`
	NotAfter    *time.Time              `json:"certificate_not_after,omitempty"`
}

// Snapshot renders the current health. It never blocks on a provider.
func (h *healthChannel) Snapshot() healthSnapshot {
	svc := h.svc
	res := svc.policyResult()
	snap := healthSnapshot{
		AgentVersion:   version,
		GeneratedAt:    time.Now().UTC(),
		ManagedState:   string(svc.managed),
		DeviceIdentity: string(svc.currentDeviceIdentity()),
		PolicyOutcome:  string(res.Outcome),
		PolicyCause:    string(res.Cause),
		Spool:          svc.spool.Stats(),
		ContentHeld:    svc.content.HeldObjects(),
		NativeClients:  svc.native.Clients(),
	}
	if c := svc.issuedCredential(); c != nil {
		snap.DeviceID, snap.Enrolled = c.DeviceID, true
	}
	b := svc.currentBundle()
	if b != nil {
		snap.PolicyVersion = b.Version
		snap.CollectionMode = string(baseMode(b, snap.DeviceID))
	}
	if snap.DeviceIdentity == string(protocol.DeviceIdentityClear) {
		snap.Hostname = svc.resolvedHostname()
	}
	_, snap.UserRefSource = svc.consolePerson()
	if svc.policySync != nil {
		st := svc.policySync.Status()
		snap.PolicyFetch = &st
	}
	connected, classVersion, detail := svc.host.status()
	snap.Classifier = classifierStatus{Connected: connected, Version: classVersion, DegradedDetail: string(detail)}

	reports, errs := svc.reg.Reports(snap.DeviceID, snap.PolicyVersion)
	for _, err := range errs {
		svc.log.Warn("a health row did not validate; it is sent anyway", "error", err)
	}
	snap.Reports = reports

	st := svc.drainer.Status()
	snap.Drain = drainStatus{State: st.State, Detail: st.Detail}
	if !st.LastSuccess.IsZero() {
		t := st.LastSuccess
		snap.Drain.LastSuccess = &t
	}
	if !st.NotAfter.IsZero() {
		t := st.NotAfter
		snap.Drain.NotAfter = &t
	}

	h.mu.Lock()
	snap.Extension = h.extension
	snap.Heartbeats = h.publishes
	if !h.lastPublishAt.IsZero() {
		t := h.lastPublishAt
		snap.LastHeartbeat = &t
	}
	if h.lastPublishErr != nil {
		snap.HeartbeatError = h.lastPublishErr.Error()
	}
	h.mu.Unlock()
	return snap
}

// collectorCodeByRoute maps a collection route onto the collector code the coverage tables key
// on. Routes name what was observed; collectors name the component that observed it, and the
// browser routes share the one extension collector.
var collectorCodeByRoute = map[string]string{
	"ext.web_request":  "capture_extension",
	"ext.page_context": "capture_extension",
	"ext.dom":          "capture_extension",
	"proxy.tls":        "egress_proxy",
	"proxy.loopback":   "loopback_broker",
	"cli.shim":         "cli_shim",
}

// collectorCode normalises a route name or an extension-style name to a collector code.
func collectorCode(name string) string {
	if code, ok := collectorCodeByRoute[name]; ok {
		return code
	}
	return strings.ReplaceAll(name, "-", "_")
}

// baseMode is the device's base collection mode: its own override when the bundle names it, else
// the tenant default. It deliberately does not run full scope resolution, which would fold in the
// class priors for an unnamed user.
func baseMode(b *policy.Bundle, deviceID string) protocol.CollectionMode {
	if m, ok := b.DeviceModes[deviceID]; ok && m.Valid() {
		return m
	}
	if b.TenantDefault.Valid() {
		return b.TenantDefault
	}
	return ""
}

// healthRequest renders the POST /v1/health body. Collector names are mapped to collector codes,
// and the device identity is absent from each row because the credential already names the device.
func (h *healthChannel) healthRequest() protocol.HealthRequest {
	snap := h.Snapshot()
	seen := map[string]bool{}
	var collectors []protocol.HealthReport
	add := func(rep protocol.HealthReport) {
		code := collectorCode(rep.Collector)
		if seen[code] {
			return
		}
		seen[code] = true
		rep.Collector = code
		rep.DeviceID = ""
		collectors = append(collectors, rep)
	}
	for _, rep := range snap.Reports {
		add(rep)
	}
	if snap.Extension != nil {
		add(*snap.Extension)
	}
	// The classifier host is a component rather than a route; it reports its own row, degraded
	// when no host answers.
	classifier := protocol.NewHealthReport("", "classifier_host", snap.Classifier.Version, snap.GeneratedAt)
	classifier.State = protocol.StateDegraded
	if snap.Classifier.Connected {
		classifier.State = protocol.StateHealthy
	}
	classifier.Detail = protocol.Detail(snap.Classifier.DegradedDetail)
	add(classifier)

	req := protocol.HealthRequest{
		SchemaVersion:       protocol.HealthSchemaVersion,
		ReportedAt:          snap.GeneratedAt,
		AgentVersion:        snap.AgentVersion,
		Hostname:            snap.Hostname,
		CollectionMode:      snap.CollectionMode,
		ManagedState:        snap.ManagedState,
		PolicyBundleVersion: snap.PolicyVersion,
		CredentialNotAfter:  snap.Drain.NotAfter,
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
