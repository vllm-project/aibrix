/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package routingalgorithms

import (
	"errors"
	"sync"
	"time"
)

var (
	errExternalBulkheadRejected = errors.New("external router bulkhead is full")
	errExternalCircuitOpen      = errors.New("external router circuit is open")
)

type externalBulkhead struct {
	permits chan struct{}
}

func newExternalBulkhead(limit int) *externalBulkhead {
	return &externalBulkhead{permits: make(chan struct{}, limit)}
}

func (b *externalBulkhead) acquire() bool {
	select {
	case b.permits <- struct{}{}:
		return true
	default:
		return false
	}
}

func (b *externalBulkhead) release() {
	<-b.permits
}

type externalCircuitToken struct {
	halfOpen bool
}

type externalCircuit struct {
	mu           sync.Mutex
	threshold    int
	openDuration time.Duration
	now          func() time.Time
	state        string
	failures     int
	openedAt     time.Time
	probing      bool
}

func newExternalCircuit(threshold int, openDuration time.Duration) *externalCircuit {
	return &externalCircuit{
		threshold:    threshold,
		openDuration: openDuration,
		now:          time.Now,
		state:        "closed",
	}
}

func (c *externalCircuit) acquire() (externalCircuitToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case "closed":
		return externalCircuitToken{}, nil
	case "open":
		if c.now().Sub(c.openedAt) < c.openDuration || c.probing {
			return externalCircuitToken{}, errExternalCircuitOpen
		}
		c.state = "half_open"
		c.probing = true
		return externalCircuitToken{halfOpen: true}, nil
	case "half_open":
		if c.probing {
			return externalCircuitToken{}, errExternalCircuitOpen
		}
		c.probing = true
		return externalCircuitToken{halfOpen: true}, nil
	default:
		return externalCircuitToken{}, errExternalCircuitOpen
	}
}

func (c *externalCircuit) success(token externalCircuitToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = "closed"
	c.failures = 0
	c.probing = false
}

func (c *externalCircuit) failure(token externalCircuitToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probing = false
	if token.halfOpen {
		c.openLocked()
		return
	}
	c.failures++
	if c.failures >= c.threshold {
		c.openLocked()
	}
}

func (c *externalCircuit) cancel(token externalCircuitToken) {
	if !token.halfOpen {
		return
	}
	c.mu.Lock()
	c.probing = false
	c.state = "open"
	c.mu.Unlock()
}

func (c *externalCircuit) openLocked() {
	c.state = "open"
	c.openedAt = c.now()
}

func (c *externalCircuit) currentState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}
