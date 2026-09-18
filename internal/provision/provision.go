// Package provision applies per-account settings that the authentication
// backend returns as attributes: the storage quota (the LDAP attribute
// Dovecot maps to quota_rule=*:bytes=<n>) and the over-quota flag check.
package provision

import (
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
