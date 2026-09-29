/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package external

import (
	"errors"
	"sync"
	"time"
)

var (
	errExternalBulkheadRejected = errors.New("external router bulkhead is full")
	errExternalCircuitOpen      = errors.New("external router circuit is open")
)

// externalBulkhead bounds concurrent network exchanges without queueing; a
// full channel rejects immediately so Gateway latency remains bounded.
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

// externalCircuitToken binds a completion to the generation in which it was
// admitted. halfOpen also identifies the single probe owning probing=true.
type externalCircuitToken struct {
	halfOpen   bool
	generation uint64
}

// externalCircuit is intentionally process-local. generation advances each
// time the breaker opens so stale in-flight results cannot close or extend a
// newer open interval.
type externalCircuit struct {
	mu           sync.Mutex
	threshold    int
	openDuration time.Duration
	now          func() time.Time
	state        string
	failures     int
	openedAt     time.Time
	probing      bool
	generation   uint64
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
		return externalCircuitToken{generation: c.generation}, nil
	case "open":
		if c.now().Sub(c.openedAt) < c.openDuration || c.probing {
			return externalCircuitToken{}, errExternalCircuitOpen
		}
		c.state = "half_open"
		c.probing = true
		return externalCircuitToken{halfOpen: true, generation: c.generation}, nil
	case "half_open":
		if c.probing {
			return externalCircuitToken{}, errExternalCircuitOpen
		}
		c.probing = true
		return externalCircuitToken{halfOpen: true, generation: c.generation}, nil
	default:
		return externalCircuitToken{}, errExternalCircuitOpen
	}
}

func (c *externalCircuit) success(token externalCircuitToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token.generation != c.generation {
		return
	}
	if token.halfOpen {
		if c.state != "half_open" || !c.probing {
			return
		}
		c.state = "closed"
		c.failures = 0
		c.probing = false
		return
	}
	if c.state == "closed" {
		c.failures = 0
	}
}

func (c *externalCircuit) failure(token externalCircuitToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token.generation != c.generation {
		return
	}
	if token.halfOpen {
		if c.state != "half_open" || !c.probing {
			return
		}
		c.openLocked()
		return
	}
	if c.state != "closed" {
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
	defer c.mu.Unlock()
	if token.generation != c.generation || c.state != "half_open" || !c.probing {
		return
	}
	// Restart the cooldown after a cancelled half-open probe; otherwise every
	// following request could probe immediately.
	c.openLocked()
}

func (c *externalCircuit) openLocked() {
	c.state = "open"
	c.openedAt = c.now()
	c.probing = false
	c.generation++
}

func (c *externalCircuit) currentState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}
