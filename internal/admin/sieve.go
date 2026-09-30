package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/foxcpp/go-sieve/interp"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// Sieve support (roadmap 8d): list, show, store (compiled first),
// activate, delete, and a check that runs a script without side effects.

// sieveCheckTimeout bounds one dry run.
const sieveCheckTimeout = 10 * time.Second

// scriptETag is the version of a script (its content).
func scriptETag(text string) string {
	sum := sha256.Sum256([]byte(text))
	return `"s-` + hex.EncodeToString(sum[:8]) + `"`
}

// sieveError maps a failed script operation.
func (s *Server) sieveError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, model.ErrNoScript):
		writeError(w, http.StatusNotFound, adminapi.NoSuchScript, "no such script")
	case errors.Is(err, model.ErrScriptActive):
		writeError(w, http.StatusConflict, adminapi.Conflict, "the active script cannot be deleted")
	case errors.Is(err, model.ErrVersionConflict):
		writeError(w, http.StatusPreconditionFailed, adminapi.PreconditionFailed, "the script changed (If-Match)")
	default:
		s.importError(w, r, err)
	}
}

// ListSieveScripts lists the scripts with sizes, versions and the active one.
func (s *Server) ListSieveScripts(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	u, ok := s.openAccount(w, r, RoleDiagnose, account, true)
	if !ok {
		return
	}
	defer u.Close()

	s.writeSieveList(w, r, u)
}

func (s *Server) writeSieveList(w http.ResponseWriter, r *http.Request, u model.User) {
	names, active, err := u.ListScripts()
	if err != nil {
		s.sieveError(w, r, err)
		return
	}

	sort.Strings(names)

	out := adminapi.SieveList{Active: optional(active), Scripts: []adminapi.SieveScriptInfo{}}

	for _, name := range names {
		text, err := u.GetScript(name)
		if err != nil {
			s.sieveError(w, r, err)
			return
		}

		out.Scripts = append(out.Scripts, adminapi.SieveScriptInfo{Name: name, Active: name == active, Size: int64(len(text)),
			Etag: scriptETag(text)})
	}

	writeJSON(w, http.StatusOK, out)
}

// openScript opens the account for a script operation and validates the
// name; ok false means the response is written.
func (s *Server) openScript(w http.ResponseWriter, r *http.Request, role, account, name string) (model.User, bool) {
	if !validScriptName(name) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid script name")
		return nil, false
	}

	u, ok := s.openAccount(w, r, role, account, true)
	if ok {
		auditf(r, "script", name)
	}

	return u, ok
}

// GetSieveScript returns one script.
func (s *Server) GetSieveScript(w http.ResponseWriter, r *http.Request, account adminapi.Account, name adminapi.ScriptName) {
	u, ok := s.openScript(w, r, RoleContent, account, name)
	if !ok {
		return
	}
	defer u.Close()

	s.writeScript(w, r, u, name)
}

func (s *Server) writeScript(w http.ResponseWriter, r *http.Request, u model.User, name string) {
	text, err := u.GetScript(name)
	if err != nil {
		s.sieveError(w, r, err)
		return
	}

	_, active, err := u.ListScripts()
	if err != nil {
		s.sieveError(w, r, err)
		return
	}

	etag := scriptETag(text)
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, adminapi.SieveScript{Name: name, Active: name == active, Script: text, Etag: etag})
}

// checkScriptVersion checks If-Match / If-None-Match against the stored
// script.
func checkScriptVersion(u model.User, name string, ifMatch *string, createOnly bool) error {
	text, err := u.GetScript(name)

	exists := err == nil
	if err != nil && !errors.Is(err, model.ErrNoScript) {
		return err
	}

	switch {
	case createOnly && exists:
		return model.ErrVersionConflict
	case ifMatch == nil:
		return nil
	case !exists:
		return model.ErrNoScript
	case !matches(ifMatch, scriptETag(text)):
		return model.ErrVersionConflict
	}

	return nil
}

// PutSieveScript compiles and stores a script, optionally activating it.
func (s *Server) PutSieveScript(w http.ResponseWriter, r *http.Request, account adminapi.Account, name adminapi.ScriptName,
	p adminapi.PutSieveScriptParams,
) {
	var req adminapi.SievePut
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	if refuseReserved(w, name) {
		return
	}

	u, ok := s.openScript(w, r, RoleSupport, account, name)
	if !ok {
		return
	}
	defer u.Close()

	activate := req.Activate != nil && *req.Activate
	auditf(r, "bytes", len(req.Script), "activate", activate)

	if err := checkScriptVersion(u, name, p.IfMatch, p.IfNoneMatch != nil); err != nil {
		s.sieveError(w, r, err)
		return
	}

	if err := sieve.CompileWith(req.Script, s.scriptHooks(u, name, req.Script)); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "script does not compile: "+err.Error())
		return
	}

	err := u.PutScript(name, req.Script)
	if err == nil && activate {
		err = u.SetActiveScript(name)
	}

	if err != nil {
		s.sieveError(w, r, err)
		return
	}

	s.writeScript(w, r, u, name)
}

// scriptHooks are the compile hooks of the account: includes resolve
// against its scripts, name against text (the script being stored).
func (s *Server) scriptHooks(u model.User, name, text string) sieve.Hooks {
	stored := sieve.IncludeLoader(u, s.globalScripts())

	return sieve.Hooks{Config: s.sieveConfig(), Include: func(n string, global bool) ([]byte, bool, error) {
		if !global && n == name {
			return []byte(text), true, nil
		}

		return stored(n, global)
	}}
}

// DeleteSieveScript deletes an inactive script.
func (s *Server) DeleteSieveScript(w http.ResponseWriter, r *http.Request, account adminapi.Account, name adminapi.ScriptName,
	p adminapi.DeleteSieveScriptParams,
) {
	if refuseReserved(w, name) {
		return
	}

	u, ok := s.openScript(w, r, RoleSupport, account, name)
	if !ok {
		return
	}
	defer u.Close()

	err := checkScriptVersion(u, name, p.IfMatch, false)
	if err == nil {
		err = u.DeleteScript(name)
	}

	if err != nil {
		s.sieveError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// SetActiveSieveScript activates a script ("" deactivates filtering).
func (s *Server) SetActiveSieveScript(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	var req adminapi.SieveActivate
	if err := decodeJSON(r, &req); err != nil || (req.Name != "" && !validScriptName(req.Name)) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	if refuseReserved(w, req.Name) {
		return
	}

	u, ok := s.openAccount(w, r, RoleSupport, account, true)
	if !ok {
		return
	}
	defer u.Close()

	auditf(r, "script", req.Name)

	if err := u.SetActiveScript(req.Name); err != nil {
		s.sieveError(w, r, err)
		return
	}

	s.writeSieveList(w, r, u)
}

// refuseReserved answers 409 for a script the server keeps itself (the
// JMAP VacationResponse) and reports whether it did. The script may be
// listed, shown and checked; it changes over JMAP.
func refuseReserved(w http.ResponseWriter, name string) bool {
	if !sieve.ReservedScript(name) {
		return false
	}

	writeError(w, http.StatusConflict, adminapi.Conflict, sieve.DescReservedScript)

	return true
}

// CheckSieve compiles a script and, with a message, runs it without
// side effects.
func (s *Server) CheckSieve(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	var req adminapi.SieveCheck
	if err := decodeJSON(r, &req); err != nil || (req.Message != nil && req.Raw != nil) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request (message and raw exclude each other)")
		return
	}

	role := RoleDiagnose
	if req.Message != nil || (req.Script == nil) {
		role = RoleContent // a stored message or script is read
	}

	u, ok := s.openAccount(w, r, role, account, true)
	if !ok {
		return
	}
	defer u.Close()

	name, text, err := s.checkedScript(u, req)
	if err != nil {
		s.sieveError(w, r, err)
		return
	}

	auditf(r, "script", name, "bytes", len(text))

	raw, ok := s.checkMessage(w, r, u, req)
	if !ok {
		return
	}

	hooks := s.scriptHooks(u, name, text)
	if err := sieve.CompileWith(text, hooks); err != nil {
		writeJSON(w, http.StatusOK, adminapi.SieveCheckResult{Valid: false, Error: optional(err.Error())})
		return
	}

	out := adminapi.SieveCheckResult{Valid: true}

	if raw != nil {
		acts, err := s.dryRun(r.Context(), u, account, text, name, raw, req)
		if err != nil {
			out.Error = optional(err.Error())
		}

		out.Actions = acts
	}

	writeJSON(w, http.StatusOK, out)
}

// errNoActiveScript is model.ErrNoScript for a check of the active
// script of an account that has none.
var errNoActiveScript = fmt.Errorf("%w: no active script", model.ErrNoScript)

// checkedScript returns the script to check: the given text, the stored
// script name, or the active script — an empty one for an account
// without an active script when global scripts run for it anyway.
func (s *Server) checkedScript(u model.User, req adminapi.SieveCheck) (string, string, error) {
	name, text, err := storedScript(u, req)
	if errors.Is(err, errNoActiveScript) && s.o.Delivery != nil && s.o.Delivery.HasGlobalScripts() {
		// Without a script of its own the account still runs the global
		// ones: check what a delivery does.
		return "", "", nil
	}

	return name, text, err
}

func storedScript(u model.User, req adminapi.SieveCheck) (string, string, error) {
	name := deref(req.Name)
	if req.Script != nil {
		return name, *req.Script, nil
	}

	if name == "" {
		_, active, err := u.ListScripts()
		if err != nil {
			return "", "", err
		}

		if active == "" {
			return "", "", errNoActiveScript
		}

		name = active
	}

	text, err := u.GetScript(name)

	return name, text, err
}

// checkMessage returns the message of a check: from the store, the given
// raw text, or nil (compile only).
func (s *Server) checkMessage(w http.ResponseWriter, r *http.Request, u model.User, req adminapi.SieveCheck) ([]byte, bool) {
	if req.Raw != nil {
		return []byte(*req.Raw), true
	}

	if req.Message == nil {
		return nil, true
	}

	ref := req.Message
	auditf(r, "mailbox_id", ref.MailboxId, "uid", ref.Uid)

	mb, err := mailboxByID(u, ref.MailboxId)
	if err != nil {
		s.importError(w, r, err)
		return nil, false
	}

	if int64(mb.UIDValidity) != ref.Uidvalidity {
		writeError(w, http.StatusPreconditionFailed, adminapi.PreconditionFailed, errUIDValidity.Error())
		return nil, false
	}

	metas, err := u.GetMessages(mb.ID, []uint32{uint32(min(max(ref.Uid, 0), int64(^uint32(0))))})
	if err == nil && (len(metas) == 0 || int64(metas[0].UID) != ref.Uid) {
		writeError(w, http.StatusNotFound, adminapi.NoSuchMessage, "no such message")
		return nil, false
	}

	var raw []byte
	if err == nil {
		raw, err = u.Read(&metas[0])
	}

	if err != nil {
		s.importError(w, r, err)
		return nil, false
	}

	return raw, true
}

// dryRun runs the script over raw with the store hooks (read-only) and a
// program runner that only records; it reports what would happen.
func (s *Server) dryRun(ctx context.Context, u model.User, account, text, name string, raw []byte, req adminapi.SieveCheck) (*adminapi.SieveActions, error) {
	hooks := s.scriptHooks(u, name, text)
	if s.o.Delivery != nil {
		store := s.o.Delivery.StoreHooks(u, account)
		store.Include, store.Config = hooks.Include, hooks.Config
		hooks = store
	}

	acts := &adminapi.SieveActions{Fileinto: []adminapi.SieveFileInto{}, Redirect: []string{}, Notify: []string{}, Programs: []string{}, Suppressed: []string{}}

	hooks.Program = func(_ context.Context, kind interp.ProgramKind, prog string, _ []string, input []byte) (interp.ProgramResult, bool, error) {
		acts.Programs = append(acts.Programs, string(kind)+" "+prog)
		acts.Suppressed = append(acts.Suppressed, fmt.Sprintf("%s %s (not run, assumed exit 0)", kind, prog))

		return interp.ProgramResult{Output: input}, true, nil
	}

	ctx, cancel := context.WithTimeout(ctx, sieveCheckTimeout)
	defer cancel()

	to := deref(req.EnvelopeTo)
	if to == "" {
		to = account
	}

	// What a delivery runs: the global scripts around the checked one.
	scripts := []sieve.Script{{Name: sieve.PersonalScript, Text: text}}
	if s.o.Delivery != nil {
		scripts = s.o.Delivery.Sequence(text, s.o.Log)
	}

	d, failed := sieve.RunSequence(ctx, scripts, deref(req.EnvelopeFrom), to, account, raw, hooks)

	for _, f := range failed {
		if f.Script == sieve.PersonalScript {
			return acts, f.Err
		}

		skipped := append(derefSlice(acts.FailedScripts), f.Error())
		acts.FailedScripts = &skipped
	}

	if len(scripts) > 1 {
		acts.Scripts = &d.Scripts
	}

	describeDelivery(acts, d, raw)

	return acts, nil
}

// describeDelivery fills the actions from a script outcome; every side
// effect is listed as suppressed.
func describeDelivery(acts *adminapi.SieveActions, d *sieve.Delivery, raw []byte) {
	for _, f := range d.FileInto {
		if f.Mailbox == inboxName && f.SpecialUse == "" {
			acts.Keep = true
		}

		acts.Fileinto = append(acts.Fileinto, adminapi.SieveFileInto{Flags: apiFlags(f.Flags), Mailbox: f.Mailbox})
	}

	// Discard: the message ends nowhere (not stored, not forwarded, not
	// rejected).
	discard := len(d.FileInto) == 0 && d.Rejected == "" && len(d.Redirects) == 0
	acts.Discard = &discard

	for _, a := range d.Redirects {
		acts.Redirect = append(acts.Redirect, a)
		acts.Suppressed = append(acts.Suppressed, "redirect to "+a)
	}

	if d.Rejected != "" {
		acts.Reject = &d.Rejected
		acts.Suppressed = append(acts.Suppressed, "reject (no bounce sent)")
	}

	if v := d.Vacation; v != nil {
		days := v.Seconds / 86400
		acts.Vacation = &adminapi.SieveVacation{Days: days, From: optional(v.From), Subject: v.Subject}
		acts.Suppressed = append(acts.Suppressed, "vacation reply (not sent, not recorded)")
	}

	for _, n := range d.Notifications {
		to := append(append(append([]string{}, n.To...), n.Cc...), n.Bcc...)
		acts.Notify = append(acts.Notify, fmt.Sprintf("mailto %v", to))
		acts.Suppressed = append(acts.Suppressed, "notification (not sent)")
	}

	for _, p := range d.Pipes {
		acts.Programs = append(acts.Programs, "pipe "+p.Program)
		acts.Suppressed = append(acts.Suppressed, "pipe "+p.Program+" (not run)")
	}

	edited := string(d.Raw) != string(raw)
	acts.HeaderEdited = &edited
}
