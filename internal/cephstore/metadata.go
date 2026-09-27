package cephstore

import (
	"fmt"
	"strings"
	"syscall"
)

// METADATA (RFC 5464). The annotations of a namespace live on one object
// "metadata"; omap keys are
//
//	s:<mailbox id|->:<entry>              shared entries
//	p:<accessor>:<mailbox id|->:<entry>   private entries of one account
//
// "-" stands for the server (mailbox ""). Mailbox ids survive RENAME; a
// deleted mailbox leaves its entries behind until the scrubber (they are
// unreachable, the id is never reused).
const oidMetadata = "metadata"

// MaxAnnotationSize bounds one value (RFC 5464 MAXSIZE).
const MaxAnnotationSize = 64 << 10

func (u *User) annotPrefix(mailbox, accessor string) (string, error) {
	id := "-"

	if mailbox != "" {
		mb, err := u.Lookup(mailbox)
		if err != nil {
			return "", err
		}

		id = mb.ID
	}

	if accessor == "" {
		return "s:" + id + ":", nil
	}

	return "p:" + accessor + ":" + id + ":", nil
}

// GetMetadata returns the shared entries of the mailbox and the private
// entries of accessor.
func (u *User) GetMetadata(mailbox, accessor string) (map[string]string, error) {
	out := map[string]string{}

	for _, acc := range []string{"", accessor} {
		prefix, err := u.annotPrefix(mailbox, acc)
		if err != nil {
			return nil, err
		}

		vals, err := u.idx.GetAllOmapValues(oidMetadata, "", prefix, omapPageSize)
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				continue
			}

			return nil, fmt.Errorf("metadata: %w", err)
		}

		for k, v := range vals {
			out[strings.TrimPrefix(k, prefix)] = string(v)
		}
	}

	return out, nil
}

// SetMetadata sets or removes entries (nil value = remove) in one CAS.
func (u *User) SetMetadata(mailbox, accessor string, entries map[string]*string) error {
	set := map[string][]byte{}

	var rm []string

	for entry, val := range entries {
		acc := ""
		if strings.HasPrefix(entry, "/private/") {
			acc = accessor
		}

		prefix, err := u.annotPrefix(mailbox, acc)
		if err != nil {
			return err
		}

		if val == nil {
			rm = append(rm, prefix+entry)
		} else {
			if len(*val) > MaxAnnotationSize {
				return ErrTooLarge
			}

			set[prefix+entry] = []byte(*val)
		}
	}

	return casOmap(u, u.idx, oidMetadata, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return set, rm
	})
}

// SwapMetadata sets one entry (nil: removes it) in one CAS, only while it
// still holds old ("" is an absent entry).
func (u *User) SwapMetadata(mailbox, accessor, entry, old string, next *string) (bool, error) {
	acc := ""
	if strings.HasPrefix(entry, "/private/") {
		acc = accessor
	}

	prefix, err := u.annotPrefix(mailbox, acc)
	if err != nil {
		return false, err
	}

	if next != nil && len(*next) > MaxAnnotationSize {
		return false, ErrTooLarge
	}

	key := prefix + entry
	swapped := false

	err = casOmap(u, u.idx, oidMetadata, []string{key}, func(cur map[string][]byte) (map[string][]byte, []string) {
		swapped = string(cur[key]) == old
		if !swapped {
			return nil, nil
		}

		if next == nil {
			if _, had := cur[key]; !had {
				return nil, nil
			}

			return nil, []string{key}
		}

		return map[string][]byte{key: []byte(*next)}, nil
	})

	return swapped && err == nil, err
}
