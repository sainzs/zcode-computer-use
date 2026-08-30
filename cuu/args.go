package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// Strict argument coercion over decoded JSON values. JSON is decoded with
// UseNumber so an int stays distinguishable from a float: 1.9 must not
// target element 1, the string "false" must not act as a boolean, and a
// bool must never pass for an int (Python's isinstance(True, int) trap).

type args map[string]any

func (a args) raw(key string) (any, bool) {
	v, ok := a[key]
	return v, ok
}

// argInt: exact-integer argument. Booleans and float literals are rejected.
func argInt(a args, key string, defaultValue any, hasDefault bool, minimum, maximum *int) (int, *ToolError) {
	value, ok := a[key]
	if !ok {
		if !hasDefault {
			return 0, toolErr("invalid_args", fmt.Sprintf("%s is required", key), "")
		}
		if defaultValue == nil {
			return 0, toolErr("invalid_args", fmt.Sprintf("%s is required", key), "")
		}
		return defaultValue.(int), nil
	}
	if _, isBool := value.(bool); isBool {
		return 0, toolErr("invalid_args", fmt.Sprintf("%s must be an integer", key),
			fmt.Sprintf("got %s", pyRepr(value)))
	}
	num, isNum := value.(json.Number)
	if !isNum || !jsonNumberIsInt(string(num)) {
		return 0, toolErr("invalid_args", fmt.Sprintf("%s must be an integer", key),
			fmt.Sprintf("got %s", pyRepr(value)))
	}
	n, err := strconv.Atoi(string(num))
	if err != nil {
		return 0, toolErr("invalid_args", fmt.Sprintf("%s must be an integer", key),
			fmt.Sprintf("got %s", pyRepr(value)))
	}
	if minimum != nil && n < *minimum {
		return 0, toolErr("invalid_args", fmt.Sprintf("%s must be >= %d", key, *minimum), "")
	}
	if maximum != nil && n > *maximum {
		return 0, toolErr("invalid_args", fmt.Sprintf("%s must be <= %d", key, *maximum), "")
	}
	return n, nil
}

// argNumber: any finite JSON number (int or float literal).
func argNumber(a args, key string, defaultValue any, hasDefault bool) (float64, *ToolError) {
	value, ok := a[key]
	if !ok {
		if !hasDefault {
			return 0, toolErr("invalid_args", fmt.Sprintf("%s is required", key), "")
		}
		if defaultValue == nil {
			return 0, toolErr("invalid_args", fmt.Sprintf("%s is required", key), "")
		}
		return defaultValue.(float64), nil
	}
	if _, isBool := value.(bool); isBool {
		return 0, toolErr("invalid_args", fmt.Sprintf("%s must be a finite number", key), "")
	}
	num, isNum := value.(json.Number)
	if !isNum || !jsonNumberIsFloat(string(num)) {
		return 0, toolErr("invalid_args", fmt.Sprintf("%s must be a finite number", key), "")
	}
	f, err := strconv.ParseFloat(string(num), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, toolErr("invalid_args", fmt.Sprintf("%s must be a finite number", key), "")
	}
	return f, nil
}

// argBool: strict boolean.
func argBool(a args, key string, defaultValue bool) (bool, *ToolError) {
	value, ok := a[key]
	if !ok {
		return defaultValue, nil
	}
	b, isBool := value.(bool)
	if !isBool {
		return false, toolErr("invalid_args", fmt.Sprintf("%s must be a boolean", key),
			fmt.Sprintf("got %s", pyRepr(value)))
	}
	return b, nil
}

// argStr: required = true rejects nil and ""; required = false maps them to nil.
func argStr(a args, key string, required bool) (string, *ToolError) {
	value, ok := a[key]
	if !ok || value == nil {
		if required {
			return "", toolErr("invalid_args", fmt.Sprintf("%s is required", key), "")
		}
		return "", nil
	}
	if s, isStr := value.(string); isStr {
		if s == "" {
			if required {
				return "", toolErr("invalid_args", fmt.Sprintf("%s is required", key), "")
			}
			return "", nil
		}
		return s, nil
	}
	return "", toolErr("invalid_args", fmt.Sprintf("%s must be a string", key), "")
}

// pyRepr renders a decoded JSON value roughly as Python's repr() would in
// the error text ("got True", "got 1.9", "got 'x'"). Diagnostics only.
func pyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return "'" + t + "'"
	case json.Number:
		return string(t)
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}
