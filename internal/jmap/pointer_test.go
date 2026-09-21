package jmap

import (
	"encoding/json"
	"reflect"
	"testing"
)

// decode is the test fixture as a decoded JSON value.
func decode(t *testing.T, s string) any {
	t.Helper()

	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}

	return v
}

// TestResolvePointer: the pointer forms RFC 8620 3.7 allows, including
// the "*" that maps the rest over an array and flattens it once.
func TestResolvePointer(t *testing.T) {
	doc := decode(t, `{
		"list": [{"id": "a", "ids": ["a1", "a2"]}, {"id": "b", "ids": ["b1"]}],
		"a/b": 1,
		"a~b": 2,
		"nested": {"deep": {"value": "x"}}
	}`)

	cases := []struct {
		want    any
		pointer string
	}{
		{doc, ""},
		{"x", "/nested/deep/value"},
		{float64(1), "/a~1b"},
		{float64(2), "/a~0b"},
		{"a", "/list/0/id"},
		{[]any{"a", "b"}, "/list/*/id"},
		{[]any{"a1", "a2", "b1"}, "/list/*/ids"},
	}

	for _, c := range cases {
		got, err := resolvePointer(doc, c.pointer)
		if err != nil {
			t.Errorf("pointer %q: %v", c.pointer, err)
			continue
		}

		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("pointer %q gave %#v, want %#v", c.pointer, got, c.want)
		}
	}
}

// TestResolvePointerRefuses: a pointer that does not resolve is an error
// and never a silent empty value, because the method would then run with
// an argument the client did not mean.
func TestResolvePointerRefuses(t *testing.T) {
	doc := decode(t, `{"list": [{"id": "a"}], "n": 1}`)

	for _, p := range []string{"nested", "/missing", "/list/1/id", "/list/01/id", "/list/-/id", "/n/x", "/list/*/missing"} {
		if got, err := resolvePointer(doc, p); err == nil {
			t.Errorf("pointer %q resolved to %#v, want an error", p, got)
		}
	}
}
