package cephstore

import (
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"
)

// Sieve scripts live in one object per user in the index pool:
//
//	"sieve"   omap: "s:<name>" -> script text
//	          xattr "active" -> name of the active script ("" = none)
//
// Scripts are small and rarely written; a single object per user with a
// CAS on write is plenty.
const (
	oidSieve       = "sieve"
	sieveKeyPrefix = "s:"
	sieveActive    = "active"
	MaxScriptLen   = 256 << 10
)

// ListScripts returns the script names and the active one.
func (u *User) ListScripts() (names []string, active string, err error) {
	vals, err := u.idx.GetAllOmapValues(oidSieve, "", sieveKeyPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, "", nil
		}

		return nil, "", err
	}

	for k := range vals {
		names = append(names, strings.TrimPrefix(k, sieveKeyPrefix))
	}

	sort.Strings(names)

	var buf [256]byte

	n, err := u.idx.GetXattr(oidSieve, sieveActive, buf[:])
	switch {
	case err == nil:
		active = string(buf[:n])
	case errCode(err) == syscall.ENODATA, errCode(err) == syscall.ENOENT:
	default:
		return nil, "", err
	}

	return names, active, nil
}

// GetScript returns a script by name.
func (u *User) GetScript(name string) (string, error) {
	vals, err := getOmapKeys(u.idx, oidSieve, []string{sieveKeyPrefix + name})
	if err != nil {
		return "", err
	}

	v, ok := vals[sieveKeyPrefix+name]
	if !ok {
		return "", ErrNoScript
	}

	return string(v), nil
}

// ActiveScript returns the active script text ("" if none).
func (u *User) ActiveScript() (string, error) {
	_, active, err := u.ListScripts()
	if err != nil || active == "" {
		return "", err
	}

	s, err := u.GetScript(active)
	if err == ErrNoScript {
		return "", nil
	}

	return s, err
}

// PutScript stores a script (validated by the caller).
func (u *User) PutScript(name, text string) error {
	if len(text) > MaxScriptLen || len(name) > 128 {
		return ErrTooLarge
	}

	return casOmap(u, u.idx, oidSieve, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{sieveKeyPrefix + name: []byte(text)}, nil
	})
}

// DeleteScript removes a script; the active script cannot be deleted.
func (u *User) DeleteScript(name string) error {
	_, active, err := u.ListScripts()
	if err != nil {
		return err
	}

	if active == name {
		return ErrScriptActive
	}

	return casOmap(u, u.idx, oidSieve, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return nil, []string{sieveKeyPrefix + name}
	})
}

// SetActiveScript activates a script ("" deactivates).
func (u *User) SetActiveScript(name string) error {
	if len(name) > 128 {
		return ErrTooLarge
	}

	if name != "" {
		if _, err := u.GetScript(name); err != nil {
			return err
		}
	}

	if name == "" {
		// deactivate: the xattr goes away (an empty value is rejected by
		// librados and makes go-ceph panic); the reader treats a missing
		// xattr or object as "no active script"
		err := u.idx.RmXattr(oidSieve, sieveActive)
		if errCode(err) == syscall.ENODATA || errCode(err) == syscall.ENOENT {
			return nil
		}

		return err
	}

	op := rados.CreateWriteOp()
	defer op.Release()

	op.Create(rados.CreateIdempotent)
	op.SetXattr(sieveActive, []byte(name))

	return op.Operate(u.idx, oidSieve, rados.OperationNoFlag)
}

// RenameScript renames a script, keeping it active if it was.
func (u *User) RenameScript(oldName, newName string) error {
	if len(newName) > 128 || newName == "" {
		return ErrTooLarge
	}

	text, err := u.GetScript(oldName)
	if err != nil {
		return err
	}

	_, active, err := u.ListScripts()
	if err != nil {
		return err
	}

	exists := false

	if err := casOmap(u, u.idx, oidSieve, []string{sieveKeyPrefix + newName}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if _, taken := cur[sieveKeyPrefix+newName]; taken {
			exists = true
			return nil, nil
		}

		return map[string][]byte{sieveKeyPrefix + newName: []byte(text)}, []string{sieveKeyPrefix + oldName}
	}); err != nil {
		return err
	}

	if exists {
		return ErrScriptExists
	}

	if active == oldName {
		return u.SetActiveScript(newName)
	}

	return nil
}

// vacationPrefix keys the auto-reply suppression records on the sieve
// object ("v:<key>" -> unix time of the last reply).
const vacationPrefix = "v:"

// VacationSeen implements model.User: one CAS records the reply and
// tells whether an earlier one is still within ttl.
func (u *User) VacationSeen(key string, ttl time.Duration) (bool, error) {
	k := vacationPrefix + key
	already := false
	// The value is the expiry time (unix seconds) of the record.
	err := casOmap(u, u.idx, oidSieve, []string{k}, func(cur map[string][]byte) (map[string][]byte, []string) {
		already = false

		if v, ok := cur[k]; ok {
			if exp, err := strconv.ParseInt(string(v), 10, 64); err == nil && time.Now().Before(time.Unix(exp, 0)) {
				already = true

				if ttl == 0 {
					return nil, []string{k}
				}

				return nil, nil
			}
		}

		if ttl < 0 {
			return nil, nil
		}

		if ttl == 0 {
			if _, ok := cur[k]; ok {
				return nil, []string{k}
			}

			return nil, nil
		}

		return map[string][]byte{k: []byte(strconv.FormatInt(time.Now().Add(ttl).Unix(), 10))}, nil
	})

	return already, err
}
