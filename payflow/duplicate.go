package payflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// rejectDuplicateFields rejects any JSON object in raw that declares the
// same field name more than once. kind is "config" or "context" and appears
// in the error message. Field names are compared as decoded JSON strings, so
// "enabled" and "enabled" name the same field; duplicate elements of
// arrays are not an error. The first duplicate in original document order is
// reported.
func rejectDuplicateFields(raw []byte, kind string) error {
	path, field, err := findDuplicateObjectField(raw, kind)
	if err != nil {
		return fmt.Errorf("%s: invalid JSON: %w", kind, err)
	}
	if field != "" {
		return fmt.Errorf("%s: ambiguous JSON object: duplicate field %q at %s", kind, field, path)
	}
	return nil
}

// findDuplicateObjectField tokenizes raw and returns the path of the first
// object that declares a field name twice, together with that field name.
// rootPath is the path of the top-level document ("config" or "context").
// Paths join object field names with dots and array indices with brackets,
// e.g. "config.flags[1].rules[0].conditions[0]".
func findDuplicateObjectField(raw []byte, rootPath string) (path, field string, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))

	type frame struct {
		isObj  bool
		path   string
		seen   map[string]struct{}
		key    string // object field whose value is currently being parsed
		hasKey bool
		idx    int // next array element index
	}
	var stack []*frame

	open := func(isObj bool, p string) {
		f := &frame{isObj: isObj, path: p}
		if isObj {
			f.seen = make(map[string]struct{})
		}
		stack = append(stack, f)
	}

	tok, err := dec.Token()
	if err != nil {
		return "", "", err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			open(true, rootPath)
		case '[':
			open(false, rootPath)
		default:
			return "", "", fmt.Errorf("unexpected closing delimiter %q", t)
		}
	}
	// A scalar root has no object fields to check.

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
		if len(stack) == 0 {
			return "", "", fmt.Errorf("trailing data after top-level value")
		}
		top := stack[len(stack)-1]

		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				var childPath string
				if top.isObj {
					childPath = top.path + "." + top.key
				} else {
					childPath = top.path + "[" + strconv.Itoa(top.idx) + "]"
				}
				open(t == '{', childPath)
			case '}', ']':
				if (t == '}') != top.isObj {
					return "", "", fmt.Errorf("mismatched closing delimiter %q", t)
				}
				stack = stack[:len(stack)-1]
				if len(stack) > 0 {
					parent := stack[len(stack)-1]
					if parent.isObj {
						parent.hasKey = false
					} else {
						parent.idx++
					}
				}
			}
		case string:
			if top.isObj && !top.hasKey {
				if _, dup := top.seen[t]; dup {
					return top.path, t, nil
				}
				top.seen[t] = struct{}{}
				top.key = t
				top.hasKey = true
			} else {
				// Scalar string value.
				if top.isObj {
					top.hasKey = false
				} else {
					top.idx++
				}
			}
		default:
			// Number, bool or nil: a scalar value.
			if top.isObj {
				top.hasKey = false
			} else {
				top.idx++
			}
		}
	}
	return "", "", nil
}
