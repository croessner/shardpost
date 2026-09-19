package config

import (
	"reflect"
	"regexp"
	"testing"
)

func TestFingerprintIgnoresSecrets(t *testing.T) {
	a := &Config{}
	a.Server.HTTPAddr = ":8080"
	a.Nauthilus.Password = "one"
	a.Ceph.Key = "k1"

	b := *a
	b.Nauthilus.Password = "two"
	b.Ceph.Key = "k2"

	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("a secret value changed the fingerprint")
	}

	b.Server.HTTPAddr = ":9090"
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("a setting did not change the fingerprint")
	}
}

// TestSecretFieldsAreTagged guards new settings: a string field whose
// name suggests a secret must be tagged secret:"true" (paths and URLs
// excepted).
func TestSecretFieldsAreTagged(t *testing.T) {
	suspect := regexp.MustCompile(`(?i)(password|secret|token)`)
	exempt := regexp.MustCompile(`(File|URL|Audience|Scopes|Cert)$`)

	var walk func(typ reflect.Type, path string)

	walk = func(typ reflect.Type, path string) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)

			switch {
			case f.Type.Kind() == reflect.Struct:
				walk(f.Type, path+f.Name+".")
			case f.Type.Kind() == reflect.String && suspect.MatchString(f.Name) && !exempt.MatchString(f.Name) &&
				f.Tag.Get("secret") != "true":
				t.Errorf("%s%s looks secret but is not tagged secret:\"true\"", path, f.Name)
			}
		}
	}

	walk(reflect.TypeOf(Config{}), "")
}
