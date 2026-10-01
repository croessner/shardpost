// Package provision applies per-account settings that the authentication
// backend returns as attributes: the storage quota (the LDAP attribute
// Dovecot maps to quota_rule=*:bytes=<n>) and the over-quota flag check.
package provision

import (
	"errors"
	"log/slog"

	"shardpost/internal/auth"
	"shardpost/internal/config"
	"shardpost/internal/model"
	"shardpost/internal/quota"
)

// QuotaAttribute names the attribute carrying the storage quota in bytes
// ("" disables). Values accept an optional K/M/G/T suffix; 0 = unlimited.
var QuotaAttribute = "rnsMSQuota"

// Quota runs the over-quota flag check (nil: disabled).
var Quota *quota.Service

// Apply stores the settings from res on the user and checks the
// over-quota flag (idempotent; errors are logged, never fatal for the
// login).
func Apply(u model.User, res *auth.Result, log *slog.Logger) {
	if res == nil {
		return
	}

	applyLimit(u, res, log)
	CheckOverFlag(u, res, log)
}

// applyLimit stores the storage limit from the quota attribute; the
// message limit is left alone (it keeps following server.quota_msgs).
func applyLimit(u model.User, res *auth.Result, log *slog.Logger) {
	if QuotaAttribute == "" {
		return
	}

	vals := res.Attributes[QuotaAttribute]
	if len(vals) == 0 {
		return
	}

	bytes, ok := config.ParseSize(vals[0])
	if !ok {
		log.Warn("quota attribute not understood", "attr", QuotaAttribute, "value", vals[0])
		return
	}

	cur, err := u.QuotaLimit()
	if err != nil {
		log.Warn("quota limit not read", "err", err)
		return
	}

	if cur.Bytes == bytes {
		return
	}

	if err := u.SetStorageLimit(bytes); err != nil {
		log.Warn("quota limit not applied", "err", err)
	}
}

// CheckOverFlag compares the over-quota flag among the attributes of
// res with the user's current usage (see quota.Service.CheckOverFlag).
func CheckOverFlag(u model.User, res *auth.Result, log *slog.Logger) {
	if Quota == nil || res == nil {
		return
	}

	usage, err := u.QuotaUsage()
	if err != nil {
		log.Warn("quota usage not read", "err", err)
		return
	}

	limit, err := u.QuotaLimit()
	if err != nil {
		log.Warn("quota limit not read", "err", err)
		return
	}

	Quota.CheckOverFlag(res.Account, res.Attributes, usage, limit)
}

// Folder is a mailbox every account gets besides its INBOX
// (server.autocreate, Dovecot's autocreate).
type Folder struct {
	Name string
	Use  string
	// Subscribe subscribes the mailbox when it is created. It is not
	// applied to a mailbox that already exists: an account that
	// unsubscribed one keeps it that way.
	Subscribe bool
}

// Mailboxes creates INBOX and the configured mailboxes of an account that
// does not have them. A mailbox is subscribed when it is created, never
// afterwards. A mailbox added to the configuration later reaches the
// accounts that log in after it, as it does in Dovecot.
func Mailboxes(u model.User, folders []Folder) error {
	fresh, err := ensureMailbox(u, "INBOX", nil)
	if err != nil {
		return err
	}

	if fresh {
		_ = u.SetSubscribed("INBOX", true)
	}

	for _, f := range folders {
		var use []string
		if f.Use != "" {
			use = []string{f.Use}
		}

		fresh, err := ensureMailbox(u, f.Name, use)
		if errors.Is(err, model.ErrLimit) {
			// An account at its mailbox limit (limits.mailboxes) goes
			// without the folder rather than without the rest.
			continue
		}

		if err != nil {
			return err
		}

		if fresh && f.Subscribe {
			_ = u.SetSubscribed(f.Name, true)
		}
	}

	return nil
}

// ensureMailbox creates a mailbox if it is missing and reports whether it
// was created now.
func ensureMailbox(u model.User, name string, use []string) (bool, error) {
	if _, err := u.Lookup(name); err == nil {
		return false, nil
	} else if !errors.Is(err, model.ErrNoMailbox) {
		return false, err
	}

	if err := u.EnsureMailbox(name, use); err != nil {
		return false, err
	}

	return true, nil
}
