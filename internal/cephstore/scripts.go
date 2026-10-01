package cephstore

import (
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
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
	MaxScriptLen   = model.MaxScriptSize
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

	active, err = u.activeScript()
	if err != nil {
		return nil, "", err
	}

	return names, active, nil
}

// activeScript reads the name of the active script ("" for none).
func (u *User) activeScript() (string, error) {
	// The xattr holds a script name, which is never longer than this.
	var buf [model.MaxScriptName]byte

	n, err := u.idx.GetXattr(oidSieve, sieveActive, buf[:])

	switch {
	case err == nil:
		return string(buf[:n]), nil
	case errCode(err) == syscall.ENODATA, errCode(err) == syscall.ENOENT:
		return "", nil
	}

	return "", err
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
	if len(text) > MaxScriptLen || len(name) > model.MaxScriptName {
		return ErrTooLarge
	}

	// An account that only keeps Sieve scripts (ManageSieve, the admin
	// API) gets its registry, and with it its directory entry: without
	// them its scripts would look like debris to the sweep.
	if err := u.ensureRegistry(); err != nil {
		return err
	}

	return casOmap(u, u.idx, oidSieve, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{sieveKeyPrefix + name: []byte(text)}, nil
	})
}

// regCreated is the registry key that ensureRegistry writes when it
// creates the registry (the time, unix seconds).
const regCreated = "created"

// ensureRegistry creates the account's registry when it has none (one
// read when it exists); the directory hook registers the account.
func (u *User) ensureRegistry() error {
	_, ver, err := u.getKeysVer(u.idx, oidRegistry, []string{regCreated})
	if err != nil || ver != 0 {
		return err
	}

	return casOmap(u, u.idx, oidRegistry, []string{regCreated}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if _, ok := cur[regCreated]; ok {
			return nil, nil
		}

		return map[string][]byte{regCreated: []byte(strconv.FormatInt(time.Now().Unix(), 10))}, nil
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
	if len(name) > model.MaxScriptName {
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

// RenameScript renames a script, keeping it active if it was. The omap
// change and the active name are one write op: there is no moment in
// which the active name points at a script that is not there, which
// delivery would read as "no filter".
func (u *User) RenameScript(oldName, newName string) error {
	if len(newName) > model.MaxScriptName || newName == "" {
		return ErrTooLarge
	}

	var planErr error

	err := casOmapOp(u, u.idx, oidSieve, []string{sieveKeyPrefix + oldName, sieveKeyPrefix + newName},
		func(cur map[string][]byte) (map[string][]byte, []string, func(*rados.WriteOp)) {
			active, err := u.activeScript()
			if err != nil {
				planErr = err

				return nil, nil, nil
			}

			set, rm, activate, err := renamePlan(cur, active, oldName, newName)
			planErr = err

			if err != nil || !activate {
				return set, rm, nil
			}

			return set, rm, func(op *rados.WriteOp) { op.SetXattr(sieveActive, []byte(newName)) }
		})
	if err != nil {
		return err
	}

	return planErr
}

// renamePlan is the omap change of a rename, and whether the active name
// moves with it: ErrNoScript without the old script, ErrScriptExists when
// the new name is taken.
func renamePlan(cur map[string][]byte, active, oldName, newName string) (map[string][]byte, []string, bool, error) {
	text, ok := cur[sieveKeyPrefix+oldName]
	if !ok {
		return nil, nil, false, ErrNoScript
	}

	if _, taken := cur[sieveKeyPrefix+newName]; taken {
		return nil, nil, false, ErrScriptExists
	}

	return map[string][]byte{sieveKeyPrefix + newName: text}, []string{sieveKeyPrefix + oldName}, active == oldName, nil
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
