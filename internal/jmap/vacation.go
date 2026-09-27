package jmap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// VacationResponse (RFC 8621 8). The setting is kept as JSON in a
// METADATA entry of the account that only the server writes
// (sieve.JMAPVacationEntry), and that entry is what delivery answers
// from: it generates the Sieve vacation from it and runs it after the
// active script, when the active script did not answer the message
// itself. A vacation the user keeps in the active script — Roundcube's —
// therefore wins, and no user script is ever rewritten.
//
// Every change also stores the generated script under a reserved name
// (sieve.JMAPVacationScript), never active, so that a ManageSieve client
// can show the setting. Delivery does not read that copy.
//
// The reply goes out through the relay of the delivery path, so the
// capability is only offered when there is one.

// CapVacationResponse is the capability of the VacationResponse data
// type (RFC 8621 1.3.3).
const CapVacationResponse = "urn:ietf:params:jmap:vacationresponse"

// vacationID is the id of the one VacationResponse of an account.
const vacationID = "singleton"

// setSingleton is the set error of a creation or destruction of the
// singleton (RFC 8621 8).
const setSingleton = "singleton"

// Bounds of what a client may keep. The subject becomes a header field
// of the reply; a body is at most what a signature may be. The record
// has to fit a metadata entry, like the identities.
const (
	maxVacationSubject = 998
	maxVacationBody    = maxSignatureBytes
)

// vacationRetries bounds how often a /set without ifInState starts over
// when another one changed the setting under it.
const vacationRetries = 5

// The properties of a VacationResponse.
const (
	propIsEnabled = "isEnabled"
	propFromDate  = "fromDate"
	propToDate    = "toDate"
)

// vacationProperties are the properties of a VacationResponse.
var vacationProperties = map[string]bool{
	propID: true, propIsEnabled: true, propFromDate: true, propToDate: true,
	propSubject: true, propTextBody: true, propHTMLBody: true,
}

// vacationResponse is the setting, as it is kept and as it is returned
// (without the id).
type vacationResponse struct {
	sieve.JMAPVacation
}

// vacationOf refuses the methods when the server cannot send a reply.
func vacationOf(sess *session) *methodError {
	if sess.srv == nil || !sess.srv.o.VacationResponse {
		return &methodError{Type: errUnknownMethod}
	}

	return nil
}

// loadVacation reads the account's setting, and the stored record it
// came from ("" for none); none is the default, which is off.
func loadVacation(sess *session) (*vacationResponse, string, *methodError) {
	md, err := sess.user.GetMetadata("", sess.account)
	if err != nil {
		return nil, "", &methodError{Type: errServerFail, Description: err.Error()}
	}

	raw := md[sieve.JMAPVacationEntry]
	v := &vacationResponse{}

	if raw != "" {
		// A record that does not read is treated as none: the client
		// sees the default and can write a new one.
		if err := json.Unmarshal([]byte(raw), v); err != nil {
			sess.log.Warn("jmap: the stored vacation response does not read", "err", err)

			v = &vacationResponse{}
		}
	}

	return v, raw, nil
}

// record is the setting as it is stored. HTML is not escaped: the
// record has to fit a metadata entry, and a body full of markup would
// grow sixfold.
func (v *vacationResponse) record() string {
	var b bytes.Buffer

	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)

	return string(bytes.TrimSpace(b.Bytes()))
}

// state is a hash of the setting, which is all there is of it.
func (v *vacationResponse) state() string {
	sum := sha256.Sum256([]byte(v.record()))

	return hex.EncodeToString(sum[:12])
}

// render is the object with the properties asked for.
func (v *vacationResponse) render(props *[]string) map[string]any {
	all := map[string]any{
		propID: vacationID, propIsEnabled: v.IsEnabled, propFromDate: v.FromDate, propToDate: v.ToDate,
		propSubject: v.Subject, propTextBody: v.TextBody, propHTMLBody: v.HTMLBody,
	}

	out := map[string]any{propID: vacationID}

	for p, val := range all {
		if selected(props, p) {
			out[p] = val
		}
	}

	return out
}

// vacationGet implements VacationResponse/get (RFC 8621 8.1).
func vacationGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	if merr := vacationOf(sess); merr != nil {
		return nil, merr
	}

	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if req.IDs != nil && len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	if merr := checkRequested(req.Properties, vacationProperties); merr != nil {
		return nil, merr
	}

	v, _, merr := loadVacation(sess)
	if merr != nil {
		return nil, merr
	}

	out := &getResponse{AccountID: accountID(sess.account), State: v.state(), List: []map[string]any{}, NotFound: []string{}}

	ids := []string{vacationID}
	if req.IDs != nil {
		ids = *req.IDs
	}

	for _, id := range ids {
		if id != vacationID {
			out.NotFound = append(out.NotFound, id)
			continue
		}

		out.List = append(out.List, v.render(req.Properties))
	}

	return out, nil
}

// vacationSet implements VacationResponse/set (RFC 8621 8.2): the
// singleton can be updated, not created or destroyed. The state check
// and the write are one step: the record is replaced only while it is
// still the one the state was computed from. A /set that loses that race
// is stateMismatch when it named a state, and starts over when it did
// not.
func vacationSet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	if merr := vacationOf(sess); merr != nil {
		return nil, merr
	}

	var req setRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if n := len(req.Create) + len(req.Update) + len(req.Destroy); n > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	for range vacationRetries {
		out, lost, merr := vacationSetOnce(sess, &req)

		switch {
		case merr != nil:
			return nil, merr
		case !lost:
			return out, nil
		case req.IfInState != nil:
			return nil, &methodError{Type: errStateMismatch}
		}
	}

	return nil, &methodError{Type: errServerFail, Description: "the vacation response keeps changing; try again"}
}

// vacationSetOnce runs a /set against the setting as it is now; lost
// reports that the setting changed before the update could be written.
func vacationSetOnce(sess *session, req *setRequest) (*setResponse, bool, *methodError) {
	v, raw, merr := loadVacation(sess)
	if merr != nil {
		return nil, false, merr
	}

	state := v.state()
	if req.IfInState != nil && *req.IfInState != state {
		return nil, false, &methodError{Type: errStateMismatch}
	}

	out := newSetResponse(accountID(sess.account), state)

	refuseSingleton(req, out)

	for ref, patch := range req.Update {
		if ref != vacationID {
			out.NotUpdated[ref] = &setError{Type: setNotFound}
			continue
		}

		next, serr := applyVacation(*v, patch)
		if serr == nil {
			var swapped bool

			swapped, serr = commitVacation(sess, raw, next)
			if serr == nil && !swapped {
				return nil, true, nil
			}
		}

		if serr != nil {
			out.NotUpdated[ref] = serr
			continue
		}

		v = next
		out.Updated[vacationID] = nil
	}

	out.NewState = v.state()

	return out, false, nil
}

// refuseSingleton answers the creations and destructions of a /set:
// there is one VacationResponse, and it stays.
func refuseSingleton(req *setRequest, out *setResponse) {
	for cid := range req.Create {
		out.NotCreated[cid] = &setError{Type: setSingleton, Description: "an account has exactly one vacation response"}
	}

	for _, ref := range req.Destroy {
		out.NotDestroyed[ref] = &setError{Type: setSingleton, Description: "the vacation response cannot be destroyed"}
	}
}

// applyVacation applies a patch to a copy of the setting and checks the
// result.
func applyVacation(v vacationResponse, patch map[string]json.RawMessage) (*vacationResponse, *setError) {
	texts := map[string]**string{propFromDate: &v.FromDate, propToDate: &v.ToDate, propSubject: &v.Subject,
		propTextBody: &v.TextBody, propHTMLBody: &v.HTMLBody}

	for k, raw := range patch {
		var err error

		switch into, isText := texts[k]; {
		case isText:
			*into, err = nullableString(raw)
		case k == propIsEnabled:
			// A Boolean, not Boolean|null: null would read as no change.
			if string(raw) == jsonNull {
				return nil, invalidProperty(k, "isEnabled is true or false, not null")
			}

			err = json.Unmarshal(raw, &v.IsEnabled)
		case k == propID:
			var id string
			if json.Unmarshal(raw, &id) != nil || id != vacationID {
				return nil, invalidProperty(k, "the server sets this property")
			}
		default:
			return nil, invalidProperty(k, "a vacation response has no property "+k)
		}

		if err != nil {
			return nil, invalidProperty(k, err.Error())
		}
	}

	if serr := checkVacation(&v); serr != nil {
		return nil, serr
	}

	return &v, nil
}

// nullableString decodes a String|null.
func nullableString(raw json.RawMessage) (*string, error) {
	if string(raw) == jsonNull {
		return nil, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}

	return &s, nil
}

// checkVacation bounds the setting and brings the dates into the form
// this server writes them in.
func checkVacation(v *vacationResponse) *setError {
	if serr := normalizeDates(v); serr != nil {
		return serr
	}

	switch {
	case v.Subject != nil && (len(*v.Subject) > maxVacationSubject || hasControl(*v.Subject, false)):
		return invalidProperty(propSubject, "the subject is one line of at most 998 bytes")
	case v.TextBody != nil && (len(*v.TextBody) > maxVacationBody || hasControl(*v.TextBody, true)):
		return invalidProperty(propTextBody, "the text is at most 16 KiB, without control characters")
	case v.HTMLBody != nil && (len(*v.HTMLBody) > maxVacationBody || hasControl(*v.HTMLBody, true)):
		return invalidProperty(propHTMLBody, "the HTML is at most 16 KiB, without control characters")
	case len(v.record()) > maxIdentityRecord:
		return &setError{Type: setTooLarge, Description: "the vacation response would not fit what an account may keep"}
	}

	return nil
}

// normalizeDates checks the two dates, writes them in UTC and refuses a
// window that ends before it begins.
func normalizeDates(v *vacationResponse) *setError {
	var at [2]time.Time

	for i, d := range []struct {
		name string
		val  **string
	}{{propFromDate, &v.FromDate}, {propToDate, &v.ToDate}} {
		if *d.val == nil {
			continue
		}

		t, ok := parseUTCDate(**d.val)
		if !ok {
			return invalidProperty(d.name, "not a UTCDate")
		}

		s := utcDate(t)
		*d.val, at[i] = &s, t
	}

	if v.FromDate != nil && v.ToDate != nil && at[1].Before(at[0]) {
		return invalidProperty(propToDate, "the vacation ends before it begins")
	}

	return nil
}

// hasControl reports a control character other than a tab, and other
// than line breaks when lines are allowed.
func hasControl(s string, lines bool) bool {
	for _, r := range s {
		switch {
		case r == '\t', lines && (r == '\r' || r == '\n'):
		case r < 0x20 || r == 0x7f:
			return true
		}
	}

	return false
}

// commitVacation writes a new setting. Its script is generated and
// compiled first, so a setting that would not run is never stored. The
// record is then swapped in, only while the stored one is still raw;
// swapped is false when it is not. Last comes the copy of the script in
// the script store, which is for display only: a failure there is logged
// and does not undo the change.
func commitVacation(sess *session, raw string, next *vacationResponse) (bool, *setError) {
	script, err := vacationScript(next)
	if err != nil {
		return false, &setError{Type: setServerFail, Description: err.Error()}
	}

	rec := next.record()

	swapped, err := sess.user.SwapMetadata("", sess.account, sieve.JMAPVacationEntry, raw, &rec)
	if err != nil {
		return false, &setError{Type: setServerFail, Description: err.Error()}
	}

	if !swapped {
		return false, nil
	}

	if err := writeVacationScript(sess.user, script); err != nil {
		sess.log.Warn("jmap: the displayed vacation script was not updated", "err", err)
	}

	return true, nil
}

// vacationScript is the script of a setting, compiled; "" while it is
// off.
func vacationScript(v *vacationResponse) (string, error) {
	if !v.IsEnabled {
		return "", nil
	}

	settings, err := v.Settings()
	if err != nil {
		return "", err
	}

	script := sieve.VacationScript(settings)
	if err := sieve.Compile(script); err != nil {
		return "", errors.New("the vacation script does not compile: " + err.Error())
	}

	return script, nil
}

// writeVacationScript stores the displayed copy of the script, or
// removes it when there is none.
func writeVacationScript(u model.User, script string) error {
	if script != "" {
		return u.PutScript(sieve.JMAPVacationScript, script)
	}

	err := u.DeleteScript(sieve.JMAPVacationScript)

	switch {
	case errors.Is(err, model.ErrNoScript):
		return nil
	case errors.Is(err, model.ErrScriptActive):
		// Only an old import could have made it the active script. An
		// empty script keeps the message and answers nothing.
		return u.PutScript(sieve.JMAPVacationScript, "# The JMAP VacationResponse of this account is off.\n")
	}

	return err
}
