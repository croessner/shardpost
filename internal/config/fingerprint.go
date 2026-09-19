package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
)

// Fingerprint is a SHA-256 over the effective configuration with the
// values of fields tagged secret:"true" left out, so two servers can be
// compared (admin API GET /admin/v1/server) without exposing or hashing
// secrets. Secret files are referenced by path and do take part.
func (c *Config) Fingerprint() string {
	b, _ := json.Marshal(redacted(reflect.ValueOf(*c)))
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// redacted converts v into maps, slices and plain values without the
// values of secret fields (field order is kept by the JSON map sorting).
func redacted(v reflect.Value) any {
	switch v.Kind() {
	case reflect.Struct:
		out := map[string]any{}

		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}

			if f.Tag.Get("secret") == "true" {
				out[f.Name] = "(secret)"
				continue
			}

			out[f.Name] = redacted(v.Field(i))
		}

		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = redacted(v.Index(i))
		}

		return out
	case reflect.Map:
		out := map[string]any{}

		for _, k := range v.MapKeys() {
			b, _ := json.Marshal(k.Interface())
			out[string(b)] = redacted(v.MapIndex(k))
		}

		return out
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}

		return redacted(v.Elem())
	default:
		return v.Interface()
	}
}
