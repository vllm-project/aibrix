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

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

// renderJSONLikePython re-renders a JSON document the way Python's
// json.dumps(json.loads(doc), ensure_ascii=False, separators=(",", ":")) does, which is how
// SGLang turns a structured /v1/decisions "input" or /v1/systemone "state" into prompt
// text. Parsing and re-serializing, rather than only stripping whitespace, matters because
// the result is the text the router hashes: "你" and "你", or 1e0 and 1.0, are the same
// prompt to the engine and must hash the same here.
//
// Matched: key order, string escaping (only quotes, backslashes and control characters are
// escaped; everything else is emitted raw), integers (arbitrary precision, "-0" becomes
// "0"), and floats (shortest round-trip digits in Python's repr layout, "Infinity" on
// overflow).
//
// Not matched, because they are not worth the cost for routing text: a repeated object key
// is kept as written, where Python keeps the first position with the last value; a lone
// surrogate escape becomes U+FFFD, where Python keeps the surrogate; and the NaN/Infinity
// literals Python accepts are rejected as invalid JSON.
func renderJSONLikePython(doc []byte) (string, error) {
	// json.Valid caps nesting depth, which bounds the recursion below on hostile input.
	if !json.Valid(doc) {
		return "", errors.New("invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var sb strings.Builder
	sb.Grow(len(doc))
	if err := writeJSONLikePython(dec, &sb); err != nil {
		return "", err
	}
	return sb.String(), nil
}

func writeJSONLikePython(dec *json.Decoder, sb *strings.Builder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		closing := byte(']')
		if v == '{' {
			closing = '}'
		}
		sb.WriteByte(byte(v))
		for first := true; dec.More(); first = false {
			if !first {
				sb.WriteByte(',')
			}
			if v == '{' {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				writePythonJSONString(sb, key.(string))
				sb.WriteByte(':')
			}
			if err := writeJSONLikePython(dec, sb); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		sb.WriteByte(closing)
	case string:
		writePythonJSONString(sb, v)
	case json.Number:
		sb.WriteString(pythonJSONNumber(v.String()))
	case bool:
		sb.WriteString(strconv.FormatBool(v))
	case nil:
		sb.WriteString(jsonNull)
	}
	return nil
}

// writePythonJSONString writes s as Python's json.dumps does with ensure_ascii=False.
// Working byte by byte is safe: every escape is ASCII and s is valid UTF-8, so bytes
// >= 0x80 only ever belong to a multi-byte character that is emitted unchanged.
func writePythonJSONString(sb *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	sb.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			sb.WriteString(`\"`)
		case c == '\\':
			sb.WriteString(`\\`)
		case c == '\n':
			sb.WriteString(`\n`)
		case c == '\r':
			sb.WriteString(`\r`)
		case c == '\t':
			sb.WriteString(`\t`)
		case c == '\b':
			sb.WriteString(`\b`)
		case c == '\f':
			sb.WriteString(`\f`)
		case c < 0x20:
			sb.WriteString(`\u00`)
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0xf])
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('"')
}

// pythonJSONNumber renders a JSON number literal as Python does after loads/dumps: a literal
// with no fraction or exponent is an int and keeps its digits, anything else is a float
// printed with repr().
func pythonJSONNumber(lit string) string {
	if !strings.ContainsAny(lit, ".eE") {
		if lit == "-0" {
			return "0"
		}
		return lit
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil && !math.IsInf(f, 0) {
		return lit // not reachable for a literal json.Valid accepted
	}
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return pythonFloatRepr(f)
}

// pythonFloatRepr formats a finite float like Python's repr(float): the shortest digits that
// round-trip, in positional notation unless the decimal exponent is below -4 or at least 16,
// and always with a fractional part or an exponent.
func pythonFloatRepr(f float64) string {
	// 'e' with precision -1 yields the shortest round-trip digits as d.ddde±XX.
	s := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	mantissa, expStr, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mantissa, ".", "", 1)

	// decpt is the position of the decimal point relative to the digits (0.DIGITS * 10^decpt).
	decpt := exp + 1
	if decpt <= -4 || decpt > 16 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		e := decpt - 1
		esign := "+"
		if e < 0 {
			esign, e = "-", -e
		}
		estr := strconv.Itoa(e)
		if len(estr) < 2 {
			estr = "0" + estr
		}
		return sign + out + "e" + esign + estr
	}
	switch {
	case decpt <= 0:
		return sign + "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	default:
		return sign + digits[:decpt] + "." + digits[decpt:]
	}
}
