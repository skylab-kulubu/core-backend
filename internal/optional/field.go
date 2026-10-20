// Package optional reads a JSON body field that may be left out: an update
// keeps what the body does not send, and tells that from a field sent null.
package optional

import (
	"bytes"
	"encoding/json"
)

// Field is a body field: Set when the key is in the body, Null when its
// value is null (Value is then the zero value), else Value holds it. A
// field left out is the zero Field.
type Field[T any] struct {
	Set   bool
	Null  bool
	Value T
}

// UnmarshalJSON runs only for a key the body has, null included.
func (f *Field[T]) UnmarshalJSON(raw []byte) error {
	var value T
	f.Set = true
	f.Null = bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	if !f.Null {
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
	}
	f.Value = value
	return nil
}

// Or is the field's value when it was sent (the zero value for null), else
// kept.
func (f Field[T]) Or(kept T) T {
	if !f.Set {
		return kept
	}
	return f.Value
}
