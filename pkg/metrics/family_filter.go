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
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// maxPooledScrapeBuffer bounds the scrape buffers kept for reuse.
const maxPooledScrapeBuffer = 4 << 20

// Parsed families copy their strings, so a scrape buffer can be reused once parsed.
var scrapeBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// parseFamilies is ParseMetricsFromReader restricted to families: it skips the lines of other
// families that start with a HELP or TYPE line. Every family it returns parses the same.
func parseFamilies(reader io.Reader, families map[string]struct{}) (map[string]*dto.MetricFamily, error) {
	buf := scrapeBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledScrapeBuffer {
			scrapeBufferPool.Put(buf)
		}
	}()
	if _, err := buf.ReadFrom(reader); err != nil {
		return nil, fmt.Errorf("error parsing metric families: %v", err)
	}
	filter := familyLineFilter{keepFamilies: families, seen: make(map[string]familyState, 128)}
	var parser expfmt.TextParser
	parsed, err := parser.TextToMetricFamilies(bytes.NewReader(filter.apply(buf.Bytes())))
	if err != nil {
		return nil, fmt.Errorf("error parsing metric families: %v", err)
	}
	return parsed, nil
}

// typeNotSet marks a family the text parser has created but not typed yet.
const typeNotSet dto.MetricType = -1

type familyState struct {
	typ dto.MetricType
	// A family that starts with a sample rather than a HELP or TYPE line is
	// always kept, so a response that is not metrics text fails as before.
	passthrough bool
}

// familyLineFilter assigns each line to a metric family the way expfmt.TextParser in
// prometheus/common v0.55.0 does, so kept families parse as before; re-check it on upgrades.
type familyLineFilter struct {
	keepFamilies map[string]struct{}
	seen         map[string]familyState
}

// apply blanks, in place, the lines that keep rejects. Newlines stay, so a
// parse error still names the line of the original input.
func (f *familyLineFilter) apply(text []byte) []byte {
	w := 0
	for r := 0; r < len(text); {
		end := bytes.IndexByte(text[r:], '\n')
		if end < 0 {
			// An unterminated last line fails the parse unless it is blank.
			w += copy(text[w:], text[r:])
			break
		}
		if f.keep(text[r : r+end]) {
			w += copy(text[w:], text[r:r+end+1])
		} else {
			text[w] = '\n'
			w++
		}
		r += end + 1
	}
	return text[:w]
}

// keep reports whether a line must reach the parser: it belongs to a kept or
// passthrough family, or it is malformed and the parser should report it.
func (f *familyLineFilter) keep(line []byte) bool {
	i := skipBlankTab(line, 0)
	if i == len(line) {
		return false
	}
	if line[i] != '#' {
		end := metricNameEnd(line, i)
		if end == i {
			return true
		}
		family := f.family(line[i:end], true)
		state := f.seen[string(family)]
		if state.typ == typeNotSet {
			state.typ = dto.MetricType_UNTYPED
			f.seen[string(family)] = state
		}
		return state.passthrough || f.kept(family)
	}
	i = skipBlankTab(line, i+1)
	keywordEnd := i
	for keywordEnd < len(line) && line[keywordEnd] != ' ' && line[keywordEnd] != '\t' {
		keywordEnd++
	}
	isType := string(line[i:keywordEnd]) == "TYPE"
	if keywordEnd == len(line) || (!isType && string(line[i:keywordEnd]) != "HELP") {
		return false
	}
	i = skipBlankTab(line, keywordEnd)
	end := metricNameEnd(line, i)
	if end == len(line) {
		return false
	}
	if line[end] != ' ' && line[end] != '\t' {
		return true
	}
	family := f.family(line[i:end], false)
	state := f.seen[string(family)]
	if isType && state.typ == typeNotSet {
		if typ, ok := parseMetricType(line[skipBlankTab(line, end):]); ok {
			state.typ = typ
			f.seen[string(family)] = state
		}
	}
	return state.passthrough || f.kept(family)
}

func (f *familyLineFilter) kept(family []byte) bool {
	_, ok := f.keepFamilies[string(family)]
	return ok
}

// family mirrors expfmt.TextParser.setOrCreateCurrentMF: an existing family by
// that name, else the summary or histogram the suffix belongs to, else a new one.
func (f *familyLineFilter) family(name []byte, fromSample bool) []byte {
	if _, ok := f.seen[string(name)]; ok {
		return name
	}
	if base := trimSummarySuffix(name); len(base) < len(name) {
		if state, ok := f.seen[string(base)]; ok && state.typ == dto.MetricType_SUMMARY {
			return base
		}
	}
	if base := trimHistogramSuffix(name); len(base) < len(name) {
		if state, ok := f.seen[string(base)]; ok && state.typ == dto.MetricType_HISTOGRAM {
			return base
		}
	}
	f.seen[string(name)] = familyState{typ: typeNotSet, passthrough: fromSample}
	return name
}

// parseMetricType reads the rest of a TYPE line as expfmt.TextParser does,
// which drops every backslash from it.
func parseMetricType(token []byte) (dto.MetricType, bool) {
	if bytes.IndexByte(token, '\\') >= 0 {
		token = bytes.ReplaceAll(token, []byte(`\`), nil)
	}
	switch string(token) {
	case "counter":
		return dto.MetricType_COUNTER, true
	case "gauge":
		return dto.MetricType_GAUGE, true
	case "histogram":
		return dto.MetricType_HISTOGRAM, true
	case "summary":
		return dto.MetricType_SUMMARY, true
	case "untyped":
		return dto.MetricType_UNTYPED, true
	}
	typ, ok := dto.MetricType_value[strings.ToUpper(string(token))]
	return dto.MetricType(typ), ok
}

func skipBlankTab(line []byte, i int) int {
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return i
}

// metricNameEnd returns the end of the metric name starting at i, or i if none.
func metricNameEnd(line []byte, i int) int {
	if i == len(line) || !isMetricNameStart(line[i]) {
		return i
	}
	for i++; i < len(line) && (isMetricNameStart(line[i]) || (line[i] >= '0' && line[i] <= '9')); i++ {
	}
	return i
}

func isMetricNameStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == ':'
}

func trimSummarySuffix(name []byte) []byte {
	switch {
	case len(name) > 6 && bytes.HasSuffix(name, []byte("_count")):
		return name[:len(name)-6]
	case len(name) > 4 && bytes.HasSuffix(name, []byte("_sum")):
		return name[:len(name)-4]
	}
	return name
}

func trimHistogramSuffix(name []byte) []byte {
	if len(name) > 7 && bytes.HasSuffix(name, []byte("_bucket")) {
		return name[:len(name)-7]
	}
	return trimSummarySuffix(name)
}
