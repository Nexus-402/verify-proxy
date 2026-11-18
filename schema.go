package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

// checkArguments holds a call's arguments to the tool's own input schema
// BEFORE the upstream sees them (ADR-0234, amended 2026-11-18): a call whose
// arguments the tool could not accept is refused 400, refunded, and costs the
// creator's server nothing.
//
// It is deliberately LENIENT: it checks only what it understands (type,
// required, properties, additionalProperties: false, enum, const, items, the
// numeric, length and item-count limits) and passes any part of a schema
// that uses a keyword it does not (anyOf, oneOf, allOf, not, $ref, if, ...),
// so it can refuse a call the server would also refuse, and never one the
// server would accept. It returns "" when the arguments pass.
func checkArguments(schema, args json.RawMessage) string {
	if len(bytes.TrimSpace(schema)) == 0 {
		return ""
	}
	var s map[string]any
	if json.Unmarshal(schema, &s) != nil {
		return ""
	}
	var v any = map[string]any{}
	if trimmed := bytes.TrimSpace(args); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		if decoder.Decode(&v) != nil {
			return "the arguments are not JSON"
		}
	}
	return checkValue(s, v, "arguments")
}

// unchecked are keywords whose meaning this checker does not implement; a
// schema node holding one is passed whole.
var unchecked = []string{"$ref", "$dynamicRef", "anyOf", "oneOf", "allOf", "not", "if", "dependentSchemas", "patternProperties", "unevaluatedProperties"}

func checkValue(s map[string]any, v any, path string) string {
	for _, keyword := range unchecked {
		if _, ok := s[keyword]; ok {
			return ""
		}
	}
	if reason := checkType(s["type"], v, path); reason != "" {
		return reason
	}
	if allowed, ok := s["enum"].([]any); ok && !containsJSON(allowed, v) {
		return fmt.Sprintf("%s must be one of %s", path, compact(allowed))
	}
	if want, ok := s["const"]; ok && !equalJSON(want, v) {
		return fmt.Sprintf("%s must be %s", path, compact(want))
	}
	switch value := v.(type) {
	case map[string]any:
		return checkObject(s, value, path)
	case []any:
		return checkArray(s, value, path)
	case string:
		n := float64(utf8.RuneCountInString(value))
		if limit, ok := number(s["minLength"]); ok && n < limit {
			return fmt.Sprintf("%s is shorter than %g characters", path, limit)
		}
		if limit, ok := number(s["maxLength"]); ok && n > limit {
			return fmt.Sprintf("%s is longer than %g characters", path, limit)
		}
	case json.Number:
		return checkNumber(s, value, path)
	}
	return ""
}

func checkObject(s map[string]any, value map[string]any, path string) string {
	if required, ok := s["required"].([]any); ok {
		for _, r := range required {
			if name, ok := r.(string); ok {
				if _, present := value[name]; !present {
					return fmt.Sprintf("%s is missing %q", path, name)
				}
			}
		}
	}
	properties, _ := s["properties"].(map[string]any)
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sub, declared := properties[key].(map[string]any)
		if declared {
			if reason := checkValue(sub, value[key], path+"."+key); reason != "" {
				return reason
			}
			continue
		}
		if extra, ok := s["additionalProperties"].(bool); ok && !extra {
			if _, listed := properties[key]; !listed {
				return fmt.Sprintf("%s has no field %q", path, key)
			}
		}
	}
	return ""
}

func checkArray(s map[string]any, value []any, path string) string {
	n := float64(len(value))
	if limit, ok := number(s["minItems"]); ok && n < limit {
		return fmt.Sprintf("%s has fewer than %g items", path, limit)
	}
	if limit, ok := number(s["maxItems"]); ok && n > limit {
		return fmt.Sprintf("%s has more than %g items", path, limit)
	}
	if items, ok := s["items"].(map[string]any); ok {
		for i, item := range value {
			if reason := checkValue(items, item, fmt.Sprintf("%s[%d]", path, i)); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func checkNumber(s map[string]any, value json.Number, path string) string {
	f, err := value.Float64()
	if err != nil {
		return ""
	}
	if limit, ok := number(s["minimum"]); ok && f < limit {
		return fmt.Sprintf("%s is below %g", path, limit)
	}
	if limit, ok := number(s["maximum"]); ok && f > limit {
		return fmt.Sprintf("%s is above %g", path, limit)
	}
	// Numeric in 2020-12; a boolean (draft-04) is not a limit and is skipped.
	if limit, ok := number(s["exclusiveMinimum"]); ok && f <= limit {
		return fmt.Sprintf("%s must be above %g", path, limit)
	}
	if limit, ok := number(s["exclusiveMaximum"]); ok && f >= limit {
		return fmt.Sprintf("%s must be below %g", path, limit)
	}
	return ""
}

// checkType holds v to "type", a name or a list of names. An unknown name
// passes.
func checkType(t, v any, path string) string {
	var names []string
	switch typ := t.(type) {
	case string:
		names = []string{typ}
	case []any:
		for _, name := range typ {
			if s, ok := name.(string); ok {
				names = append(names, s)
			}
		}
	default:
		return ""
	}
	for _, name := range names {
		if isType(name, v) {
			return ""
		}
	}
	return fmt.Sprintf("%s must be %s, not %s", path, strings.Join(names, " or "), typeOf(v))
}

func isType(name string, v any) bool {
	switch name {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f)
	default:
		return true
	}
}

func typeOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case nil:
		return "null"
	default:
		return "something else"
	}
}

func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// equalJSON compares a schema value (numbers as float64) with an argument
// (numbers as json.Number) as JSON values.
func equalJSON(schemaValue, arg any) bool {
	return reflect.DeepEqual(normalize(schemaValue), normalize(arg))
}

func containsJSON(allowed []any, v any) bool {
	for _, a := range allowed {
		if equalJSON(a, v) {
			return true
		}
	}
	return false
}

func normalize(v any) any {
	switch value := v.(type) {
	case json.Number:
		f, err := value.Float64()
		if err != nil {
			return value.String()
		}
		return f
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = normalize(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			out[k] = normalize(item)
		}
		return out
	default:
		return v
	}
}

func compact(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "the allowed values"
	}
	return string(raw)
}
