package memstore

import (
	"reflect"
	"testing"
	"time"

	"shardpost/internal/model"
)

// populated returns an account that holds something of every kind the
// store keeps for an account, and a grantor that shares a mailbox with
// it.
func populated(t *testing.T) *User {
	t.Helper()

	s := New()

	u := s.user("alice")
	note := "x"

	steps := []func() error{
		func() error { return u.CreateMailbox("INBOX", nil) },
		func() error { _, err := u.Deliver("INBOX", []byte("Subject: hi\r\n\r\nbody\r\n"), nil, time.Time{}); return err },
		func() error { _, err := u.PutBlob([]byte("upload")); return err },
		func() error { return u.SetMetadata("INBOX", "", map[string]*string{"/shared/comment": &note}) },
		func() error { _, err := u.VacationSeen("k", time.Hour); return err },
		func() error { return u.SetSubscribed("Missing", true) },
		func() error { return u.SetPublicSubscribed("0123456789abcdef0123456789abcdef", true) },
		func() error { return u.SetPrivateSeen("0123456789abcdef0123456789abcdef", 1, []uint32{1}, true, 1) },
		func() error {
			return u.IndexApply("jmaploc", []string{"k"}, func(_ []string, _ map[string][]byte) (map[string][]byte, []string) {
				return map[string][]byte{"k": []byte("v")}, nil
			})
		},
		func() error { return u.PutScript("s", "keep;") },
		func() error { return u.SetActiveScript("s") },
		func() error { return u.SetQuotaLimit(model.Quota{Bytes: 100}) },
		func() error { _, err := u.BeginDelivery(); return err },
		func() error { return u.SetTrace(model.TraceState{Until: time.Now().Add(time.Hour)}) },
		func() error { _, err := u.ImportLock("s", time.Hour); return err },
	}

	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}

	bob, err := s.User("bob")
	if err != nil {
		t.Fatal(err)
	}

	if err := bob.CreateMailbox("Shared", nil); err != nil {
		t.Fatal(err)
	}

	if err := bob.SetACL("Shared", "alice", "lr"); err != nil {
		t.Fatal(err)
	}

	return u
}

// TestPurgeLeavesNothing: a purge of a fully populated account leaves no
// state of it behind (what the RADOS store's purge deletes object by
// object, scaling plan B9).
func TestPurgeLeavesNothing(t *testing.T) {
	u := populated(t)

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// Not data of the account: its name and store, the lock, the GUID
	// counter and the watchers of the account log.
	skip := map[string]bool{"name": true, "store": true, "mu": true, "guidN": true}

	v := reflect.ValueOf(u).Elem()
	typ := v.Type()

	for i := range v.NumField() {
		name := typ.Field(i).Name
		if skip[name] {
			continue
		}

		if !empty(v.Field(i)) {
			t.Errorf("field %s is left after the purge", name)
		}
	}
}

// empty reports whether a field holds nothing: an empty map, slice or
// string, a zero value, or (for a struct) only fields that are empty or
// watchers.
func empty(f reflect.Value) bool {
	switch f.Kind() {
	case reflect.Map, reflect.Slice:
		return f.Len() == 0
	case reflect.Struct:
		for i := range f.NumField() {
			if f.Type().Field(i).Name == "watchers" {
				continue
			}

			if !empty(f.Field(i)) {
				return false
			}
		}

		return true
	default:
		return f.IsZero()
	}
}
