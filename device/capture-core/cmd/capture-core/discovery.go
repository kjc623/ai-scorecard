package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/protocol"
)

// discoveryEmitter is the one discovery emitter every discovery collector reports to. It is built
// for the issued device at the first record after enrolment, and again if the issued device
// changes. Before enrolment a record is refused and counted as an error, as the pipeline would
// refuse it.
type discoveryEmitter struct {
	svc *service

	mu     sync.Mutex
	device string
	e      *discovery.Emitter
}

func (d *discoveryEmitter) emitter() (*discovery.Emitter, error) {
	c := d.svc.issuedCredential()
	if c == nil {
		return nil, errors.New("discovery: the device is not enrolled yet")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.e == nil || d.device != c.DeviceID {
		e, err := discovery.New(discovery.Config{
			Pipeline: d.svc.pipe,
			Dir:      d.svc.dir,
			Clock:    time.Now,
			Bundles:  d.svc.currentBundle,
			DeviceID: c.DeviceID,
		})
		if err != nil {
			return nil, err
		}
		d.e, d.device = e, c.DeviceID
	}
	return d.e, nil
}

// Emit implements procmon.Emitter.
func (d *discoveryEmitter) Emit(ctx context.Context, collector *core.CounterSet, r discovery.Record) error {
	e, err := d.emitter()
	if err != nil {
		collector.Add(protocol.CounterErrors)
		return err
	}
	return e.Emit(ctx, collector, r)
}

// Stop implements procmon.Emitter. A stop is never an envelope, so it is counted with or without
// an enrolment.
func (d *discoveryEmitter) Stop(ctx context.Context, collector *core.CounterSet, r discovery.Record) {
	if e, err := d.emitter(); err == nil {
		e.Stop(ctx, collector, r)
		return
	}
	collector.Add(protocol.CounterObserved)
}
