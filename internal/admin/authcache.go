package admin

import (
	"net/http"
	"strings"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
)

// AuthFlusher flushes the auth cache here and on every other server
// (auth.FlushLog).
type AuthFlusher interface {
	Flush(account string) error
}

// FlushAuthCache drops the cached logins and lookups of an account, or
// all of them (role repair). This server's cache is flushed before the
// answer; the other servers follow the flush log.
func (s *Server) FlushAuthCache(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, RoleRepair) {
		return
	}

	var req adminapi.AuthFlushRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	account := ""
	if req.Account != nil {
		account = strings.ToLower(auth.NormalizeAddress(*req.Account))
	}

	if account == "" {
		account = auth.FlushAll
	}

	auditf(r, "flush_account", account)

	if s.o.AuthFlush == nil {
		writeJSON(w, http.StatusOK, adminapi.AuthFlush{Account: account, Cached: false})
		return
	}

	if err := s.o.AuthFlush.Flush(account); err != nil {
		s.o.Log.Error("auth cache flush not recorded for the other servers", "account", account, "err", err)
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary,
			"this server's auth cache was flushed, but the flush could not be recorded for the other servers; try again")

		return
	}

	writeJSON(w, http.StatusOK, adminapi.AuthFlush{Account: account, Cached: true})
}
