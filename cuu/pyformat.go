package main

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// pyFloatStr renders a float the way Python's str() does: integral values
// keep a trailing ".0" (str(2.0) == "2.0", not "2"), everything else uses
// Python's shortest round-tripping repr. Used wherever a number lands in
// prose or JSON that an agent reads back.
func pyFloatStr(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		// json.dumps would reject these; callers never produce them
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e16 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// pyRound mirrors Python's round(x, ndigits): correct rounding of the binary
// value (half to even on exact ties), returned as a float.
func pyRound(x float64, ndigits int) float64 {
	shift := math.Pow(10, float64(ndigits))
	scaled := x * shift
	if math.IsInf(scaled, 0) || math.IsNaN(scaled) {
		return x
	}
	r := math.RoundToEven(scaled)
	return r / shift
}

// pySlice mirrors Python's value[start:start+length] over code points.
// Start beyond the end yields ""; length is clamped by the caller.
func pySlice(s string, start, length int) string {
	if start < 0 || length <= 0 {
		return ""
	}
	runes := []rune(s)
	if start >= len(runes) {
		return ""
	}
	end := start + length
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[start:end])
}

// pyLen is Python's len(): code points, not bytes.
func pyLen(s string) int { return utf8.RuneCountInString(s) }

// pyTruncate mirrors `t[:limit-1] + "…"` in snippet().
func pyTruncate(s string, limit int) string {
	if pyLen(s) <= limit {
		return s
	}
	return pySlice(s, 0, limit-1) + "…"
}

// jsonNumberLiteral reports whether a json.Number literal is an integer
// (Python json.loads would have produced an int for it).
func jsonNumberIsInt(n string) bool {
	if n == "" {
		return false
	}
	body := strings.TrimPrefix(strings.TrimPrefix(n, "-"), "+")
	if body == "" {
		return false
	}
	for _, r := range body {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// jsonNumberIsFloat reports whether a json.Number literal would have
// decoded as a Python float (any number literal, including ints).
func jsonNumberIsFloat(n string) bool {
	_, err := strconv.ParseFloat(n, 64)
	return err == nil
}
