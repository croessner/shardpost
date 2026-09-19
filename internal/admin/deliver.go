package admin

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/model"
)

// maxAddress bounds recipient and sender (RFC 5321 path limit plus
// angle brackets) and maxMailbox a mailbox name. The timeouts bound the
// recipient lookup and the delivery, which run detached from the
// request: a client that goes away must not cut Sieve or side effects
// short halfway (LMTP does the same).
const (
	maxAddress      = 320
	maxMailbox      = 1024
	lookupTimeout   = 10 * time.Second
	deliveryTimeout = 5 * time.Minute
)

// loopback is the client address reported for local callers.
var loopback = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}

// Deliver is the local delivery agent: one message for one recipient,
// resolved and stored exactly like an LMTP recipient.
func (s *Server) Deliver(w http.ResponseWriter, r *http.Request, p adminapi.DeliverParams) {
	enforce := p.EnforceQuota == nil || *p.EnforceQuota
	runSieve := p.Sieve == nil || *p.Sieve
	from, mailbox := deref(p.From), deref(p.Mailbox)

	auditf(r, "recipient", p.Recipient, "from", from, "enforce_quota", enforce, "sieve", runSieve, "mailbox", mailbox)

	if !require(w, r, RoleDeliver) || (!enforce && !require(w, r, RoleDeliverNoEnforce)) {
		return
	}

	if msg := checkDeliverParams(p.Recipient, from, mailbox, r.Header.Get("Content-Type")); msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
		return
	}

	raw, release, err := delivery.ReadMessage(r.Body, s.o.MaxMessageBytes)
	defer release()

	if err != nil {
		s.deliveryError(w, r, err)
		return
	}

	detached := context.WithoutCancel(r.Context())

	res, ok := s.resolveRecipient(w, r, p.Recipient)
	if !ok {
		return
	}

	rcpt := strings.Trim(p.Recipient, "<>")
	msg := append(s.received(r, rcpt), raw...)

	dctx, cancel := context.WithTimeout(detached, deliveryTimeout)
	defer cancel()

	out, err := s.o.Delivery.Deliver(dctx, delivery.Request{
		Account: res.Account, Rcpt: rcpt, From: strings.Trim(from, "<>"), Raw: msg, Lookup: res,
		Mailbox: mailbox, NoSieve: !runSieve, NoQuotaEnforcement: !enforce, Log: s.o.Log,
	})
	if err != nil {
		s.deliveryError(w, r, err)
		return
	}

	result := adminapi.DeliveryResult{Account: res.Account, Stored: []adminapi.StoredMessage{}}
	for _, st := range out.Stored {
		result.Stored = append(result.Stored, adminapi.StoredMessage{Mailbox: st.Mailbox, Uid: int64(st.UID), Guid: st.GUID, Size: st.Size})
	}

	auditf(r, "stored", len(result.Stored))
	writeJSON(w, http.StatusOK, result)
}

// received is the trace header of a local delivery (RFC 5321 4.4).
func (s *Server) received(r *http.Request, rcpt string) []byte {
	by := "local"
	if p := principalOf(r.Context()); p != nil {
		by = p.transport
		// A certificate name is outside our control: only a clean one
		// goes into the header.
		if validPath(p.name) {
			by += " " + p.name
		}
	}

	return []byte(fmt.Sprintf("Received: from %s\r\n\tby %s with LDA\r\n\tfor <%s>; %s\r\n",
		by, s.o.Hostname, rcpt, time.Now().Format(time.RFC1123Z)))
}

// checkDeliverParams validates what the generated binding does not;
// it returns the problem or "".
func checkDeliverParams(recipient, from, mailbox, contentType string) string {
	switch {
	case recipient == "" || !validPath(recipient):
		return "invalid recipient"
	case !validPath(from):
		return "invalid sender"
	case len(mailbox) > maxMailbox || hasControl(mailbox):
		return "invalid mailbox"
	}

	if mt, _, err := mime.ParseMediaType(contentType); err != nil || mt != mimeRFC822 {
		return "the body must be message/rfc822"
	}

	return ""
}

// deliveryError maps a failed delivery to the error document.
func (s *Server) deliveryError(w http.ResponseWriter, r *http.Request, err error) {
	var rej *delivery.RejectedError

	switch {
	case errors.Is(err, auth.ErrInvalid):
		writeError(w, http.StatusNotFound, adminapi.NoSuchUser, "no such user")
	case errors.Is(err, delivery.ErrNoMailbox):
		writeError(w, http.StatusNotFound, adminapi.NoSuchMailbox, "no such mailbox")
	case errors.Is(err, model.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, adminapi.TooLarge, "message too large")
	case errors.As(err, &rej):
		writeError(w, http.StatusUnprocessableEntity, adminapi.Rejected, rej.Reason)
	case errors.Is(err, model.ErrOverQuota):
		writeError(w, http.StatusInsufficientStorage, adminapi.OverQuota, "mailbox full")
	case errors.Is(err, delivery.ErrQuotaExemptUnsupported):
		writeError(w, http.StatusInternalServerError, adminapi.Internal, err.Error())
	case errors.Is(err, auth.ErrTemporary), errors.Is(err, delivery.ErrBusy), errors.Is(err, delivery.ErrRedirectFailed), errors.Is(err, model.ErrImportLocked):
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "temporary failure, try again later")
	default:
		s.o.Log.Error("admin delivery failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "temporary failure, try again later")
	}
}

// clientAddr is the address reported to the authenticator: the peer of
// a TCP caller, loopback for local callers.
func clientAddr(r *http.Request) net.Addr {
	if r.TLS == nil {
		return loopback
	}

	if a, err := net.ResolveTCPAddr("tcp", r.RemoteAddr); err == nil {
		return a
	}

	return nil
}

// validPath accepts an envelope address with or without angle brackets:
// bounded, and without control characters, spaces or further brackets,
// so it cannot inject parameters into MAIL FROM or RCPT TO when it is
// relayed (Sieve redirect, vacation).
func validPath(s string) bool {
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">")

	return len(s) <= maxAddress && !hasControl(inner) && !strings.ContainsAny(inner, " \t<>")
}

// hasControl reports control characters (header and log injection).
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func deref(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

// resolveRecipient looks the recipient up like an LMTP recipient and
// checks the client scope on its account; ok false means the response
// is written.
func (s *Server) resolveRecipient(w http.ResponseWriter, r *http.Request, recipient string) (*auth.Result, bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lookupTimeout)
	defer cancel()

	res, err := s.o.Auth.Lookup(ctx, recipient, "lmtp", clientAddr(r))
	if err != nil {
		s.deliveryError(w, r, err)
		return nil, false
	}

	auditf(r, "account", res.Account)

	if !principalOf(r.Context()).inScope(res.Account) {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "account outside the client's scope")
		return nil, false
	}

	return res, true
}
