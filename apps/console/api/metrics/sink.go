/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"errors"
	"strings"
	"time"
)

// Tag is a key-value pair attached to a metric emission.
type Tag struct {
	Name  string
	Value string
}

// T is a shorthand for constructing a Tag.
func T(name, value string) Tag {
	return Tag{Name: name, Value: value}
}

// Sink is the interface that every metrics backend must implement.
// Name is a dot-separated metric path (e.g. "console.request.count").
type Sink interface {
	Counter(name string, val float32, tags ...Tag)
	Gauge(name string, val float32, tags ...Tag)
	Timer(name string, val float32, tags ...Tag)
	// Store records an arbitrary numeric observation.
	// Backends that lack a native store type should delegate to Gauge.
	Store(name string, val float32, tags ...Tag)
	// Rate records a rate observation.
	// Backends that lack a native rate type should delegate to Counter.
	Rate(name string, val float32, tags ...Tag)
	Close() error
}

// FanoutSink fans out every emission to all constituent sinks.
type FanoutSink []Sink

func (f FanoutSink) Counter(name string, val float32, tags ...Tag) {
	for _, s := range f {
		s.Counter(name, val, tags...)
	}
}

func (f FanoutSink) Gauge(name string, val float32, tags ...Tag) {
	for _, s := range f {
		s.Gauge(name, val, tags...)
	}
}

func (f FanoutSink) Timer(name string, val float32, tags ...Tag) {
	for _, s := range f {
		s.Timer(name, val, tags...)
	}
}

func (f FanoutSink) Store(name string, val float32, tags ...Tag) {
	for _, s := range f {
		s.Store(name, val, tags...)
	}
}

func (f FanoutSink) Rate(name string, val float32, tags ...Tag) {
	for _, s := range f {
		s.Rate(name, val, tags...)
	}
}

func (f FanoutSink) Close() error {
	var errs []error
	for _, s := range f {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NoopSink discards all emissions.
type NoopSink struct{}

func (NoopSink) Counter(string, float32, ...Tag) {}
func (NoopSink) Gauge(string, float32, ...Tag)   {}
func (NoopSink) Timer(string, float32, ...Tag)   {}
func (NoopSink) Store(string, float32, ...Tag)   {}
func (NoopSink) Rate(string, float32, ...Tag)    {}
func (NoopSink) Close() error                    { return nil }

// Duration records elapsed time since start as a timer observation (ms).
func Duration(sink Sink, name string, start time.Time, tags ...Tag) {
	sink.Timer(name+".ms", float32(time.Since(start).Milliseconds()), tags...)
}

// splitName splits a dot-separated metric name into path components.
func splitName(name string) []string {
	parts := strings.Split(name, ".")
	filtered := parts[:0]
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return []string{"unnamed"}
	}
	return filtered
}
