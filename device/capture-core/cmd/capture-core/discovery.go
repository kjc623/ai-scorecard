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

// discoveryEmitter is the one discovery emitter every discovery collector shares, so the device
// keeps one seen file and one daily budget. The emitter needs the device id, so it is built at its
// first use: collectors start only once the identity is resolved, which is after enrolment.
type discoveryEmitter struct {
	svc *service

	mu sync.Mutex
	e  *discovery.Emitter
}

// Emit implements inventory.Emitter. Before enrolment it counts an error and emits nothing.
func (d *discoveryEmitter) Emit(ctx context.Context, collector *core.CounterSet, r discovery.Record) error {
	e, err := d.emitter()
	if err != nil {
		collector.Add(protocol.CounterErrors)
		return err
	}
	return e.Emit(ctx, collector, r)
}

func (d *discoveryEmitter) emitter() (*discovery.Emitter, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.e != nil {
		return d.e, nil
	}
	c := d.svc.issuedCredential()
	if c == nil {
		return nil, errors.New("discovery: the device is not enrolled yet")
	}
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
	d.e = e
	return e, nil
}
