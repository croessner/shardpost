package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"unicode/utf8"

	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// sieveSetRequest is the arguments of a SieveScript/set (RFC 9661 2.4).
type sieveSetRequest struct {
	setRequest

	OnSuccessActivateScript   *string `json:"onSuccessActivateScript"`
	OnSuccessDeactivateScript bool    `json:"onSuccessDeactivateScript"`
}

// The descriptions of the errors a script change runs into.
const (
	descScriptExists   = "a script of that name exists"
	descScriptIsActive = "the active script is deactivated first"
)

// descReservedUpdate is the error of a change to the display copy of the
// VacationResponse: RFC 9661 4 lets a client read it, not change it.
const descReservedUpdate = sieve.DescReservedScript + "; change it with VacationResponse/set"

// scriptWriter carries out one SieveScript/set against the scripts as
// they were when it began, kept up to date as it goes, so that a creation
// sees the names an earlier one took.
type scriptWriter struct {
	sess *session
	o    *SieveOptions
	book *scriptBook
	out  *setResponse
	// created maps the id of a script created by this call to its
	// creation id, for the activation to report it in created.
	created map[string]string
}

// sieveScriptSet implements SieveScript/set (RFC 9661 2.4): creations,
// updates and destructions as ManageSieve's PUTSCRIPT, RENAMESCRIPT and
// DELETESCRIPT carry them out, then — only when all of them succeeded —
// the deactivation and the activation, as SETACTIVE does. The display
// copy of the VacationResponse can be read and not changed, and cannot
// become the active script.
func sieveScriptSet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	o, merr := sieveOf(sess)
	if merr != nil {
		return nil, merr
	}

	var req sieveSetRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if n := len(req.Create) + len(req.Update) + len(req.Destroy); n > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	b, err := loadScripts(sess)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	if req.IfInState != nil && *req.IfInState != b.state() {
		return nil, &methodError{Type: errStateMismatch}
	}

	if merr := refuseReservedActivation(sess, b, req.OnSuccessActivateScript); merr != nil {
		return nil, merr
	}

	w := &scriptWriter{sess: sess, o: o, book: b, out: newSetResponse(accountID(sess.account), b.state()),
		created: map[string]string{}}

	w.apply(&req.setRequest)

	if len(w.out.NotCreated)+len(w.out.NotUpdated)+len(w.out.NotDestroyed) == 0 {
		w.onSuccess(&req)
	}

	if after, err := loadScripts(sess); err == nil {
		w.out.NewState = after.state()
	} else {
		sess.log.Warn("jmap: the scripts could not be read back", "err", err)
	}

	return w.out, nil
}

// refuseReservedActivation refuses a /set that would activate the
// display copy of the VacationResponse, before anything is written:
// delivery does not read that copy, and making it the active script
// would replace the user's filter with a copy of the vacation.
func refuseReservedActivation(sess *session, b *scriptBook, ref *string) *methodError {
	if ref == nil {
		return nil
	}

	id, _ := resolveCreationID(sess, *ref)

	if sc := b.byID(id); sc != nil && model.ReservedScript(sc.name) {
		return &methodError{Type: errForbidden, Description: descReservedUpdate}
	}

	return nil
}

// apply carries out the creations, updates and destructions, in that
// order and each in id order.
func (w *scriptWriter) apply(req *setRequest) {
	cids := make([]string, 0, len(req.Create))
	for cid := range req.Create {
		cids = append(cids, cid)
	}

	sort.Strings(cids)

	for _, cid := range cids {
		w.create(cid, req.Create[cid])
	}

	ids := make([]string, 0, len(req.Update))
	for id := range req.Update {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	for _, id := range ids {
		if serr := w.update(id, req.Update[id]); serr != nil {
			w.out.NotUpdated[id] = serr
		}
	}

	for _, id := range req.Destroy {
		if serr := w.destroy(id); serr != nil {
			w.out.NotDestroyed[id] = serr
		}
	}
}

// scriptCreation is what a creation names.
type scriptCreation struct {
	Name     *string `json:"name"`
	BlobID   string  `json:"blobId"`
	IsActive *bool   `json:"isActive"`
}

// create stores a new script under a new id. Without a name the script
// is named after its id.
func (w *scriptWriter) create(cid string, raw json.RawMessage) {
	c, serr := readScriptCreation(raw)
	if serr == nil && c.Name != nil {
		serr = w.checkNewName(*c.Name, nil)
	}

	var text string
	if serr == nil {
		text, serr = scriptText(w.sess, w.o, c.BlobID)
	}

	if serr != nil {
		w.out.NotCreated[cid] = serr
		return
	}

	id := randomHex(8)

	name := "script-" + id
	if c.Name != nil {
		name = *c.Name
	}

	if serr := w.store(name, text, &id); serr != nil {
		w.out.NotCreated[cid] = serr
		return
	}

	w.book.list = append(w.book.list, &sieveScript{id: id, name: name, text: text})
	w.created[id] = cid
	w.sess.created[cid] = id
	w.out.Created[cid] = map[string]any{propID: id, propName: name, propBlobID: scriptBlobID(text), propIsActive: false}
}

// readScriptCreation decodes a creation and refuses what the server sets.
func readScriptCreation(raw json.RawMessage) (*scriptCreation, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	for k := range given {
		if !sieveProperties[k] || k == propID {
			return nil, invalidProperty(k, "a SieveScript is created with a name and a blobId")
		}
	}

	var c scriptCreation
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	switch {
	case c.BlobID == "":
		return nil, invalidProperty(propBlobID, "a SieveScript needs a blobId")
	case c.IsActive != nil && *c.IsActive:
		return nil, invalidProperty(propIsActive, "the server sets isActive; use onSuccessActivateScript")
	}

	return &c, nil
}

// store registers the id of a new script, then writes the script: a
// listing in between finds the id waiting for the name, never a script
// without one. A write that fails takes the id back. The id the script
// ends up with is returned in id; it differs from the one proposed only
// when the record had no room for it (see fitScriptIDs).
func (w *scriptWriter) store(name, text string, id *string) *setError {
	listed := w.book.names()
	listed[name] = true

	kept, err := changeScriptIDs(w.sess, listed, func(ids map[string]string) { ids[name] = *id })
	if err != nil {
		return &setError{Type: setServerFail, Description: err.Error()}
	}

	if err := w.sess.user.PutScript(name, text); err != nil {
		w.forgetID(name, *id)

		return &setError{Type: setServerFail, Description: err.Error()}
	}

	*id = effectiveScriptID(kept, name)

	return nil
}

// forgetID removes the entry of a name while it still holds id.
func (w *scriptWriter) forgetID(name, id string) {
	if _, err := changeScriptIDs(w.sess, w.book.names(), func(ids map[string]string) {
		if ids[name] == id {
			delete(ids, name)
		}
	}); err != nil {
		w.sess.log.Warn("jmap: a script id stays behind", "err", err)
	}
}

// checkNewName checks a name a script is to be given: the rules of RFC
// 9661 2.1 (which are ManageSieve's), not the reserved name, and not the
// name of another script (self, for a rename, is the script itself).
func (w *scriptWriter) checkNewName(name string, self *sieveScript) *setError {
	if !validScriptName(name) {
		return invalidProperty(propName, "a name is 1 to 128 bytes of UTF-8 without control characters or line separators")
	}

	if model.ReservedScript(name) {
		return &setError{Type: setForbidden, Description: sieve.DescReservedScript}
	}

	if other := w.book.byName(name); other != nil && other != self {
		return &setError{Type: setAlreadyExists, ExistingID: other.id, Description: descScriptExists}
	}

	return nil
}

// validScriptName applies the rules of RFC 9661 2.1 within the length
// ManageSieve allows here.
func validScriptName(name string) bool {
	if name == "" || len(name) > model.MaxScriptName || !utf8.ValidString(name) {
		return false
	}

	for _, r := range name {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}

	return true
}

// scriptPatch is what an update changes.
type scriptPatch struct {
	name   *string
	blobID string
}

// update renames a script, replaces its content, or both, and changes
// nothing when it fails (RFC 8620 5.3). Everything is checked first. The
// rename comes before the content, since it is what can still fail — a
// name taken meanwhile, a script renamed over ManageSieve — and a content
// write that fails after it renames the script back.
func (w *scriptWriter) update(id string, raw map[string]json.RawMessage) *setError {
	sc, p, text, serr := w.checkUpdate(id, raw)
	if serr != nil {
		return serr
	}

	old := sc.name

	if p.name != nil && *p.name != sc.name {
		if serr := w.rename(sc, *p.name); serr != nil {
			return serr
		}
	}

	if p.blobID != "" {
		if err := w.sess.user.PutScript(sc.name, text); err != nil {
			if sc.name != old {
				w.renameBack(sc, old)
			}

			return &setError{Type: setServerFail, Description: err.Error()}
		}

		sc.text = text
	}

	var changed map[string]any
	if p.blobID != "" {
		// The blob the client named is not the script's blob: that one
		// follows from the content, and RFC 8620 5.3 has the server say
		// so when it stores something other than it was given.
		changed = map[string]any{propBlobID: scriptBlobID(text)}
	}

	w.out.Updated[id] = changed

	return nil
}

// checkUpdate finds the script of an update and checks the change: the
// patch, the new name, and the new content, which it returns.
func (w *scriptWriter) checkUpdate(id string, raw map[string]json.RawMessage) (*sieveScript, *scriptPatch, string, *setError) {
	sc := w.book.byID(id)

	switch {
	case sc == nil:
		return nil, nil, "", &setError{Type: setNotFound}
	case model.ReservedScript(sc.name):
		return nil, nil, "", &setError{Type: setForbidden, Description: descReservedUpdate}
	}

	p, serr := readScriptPatch(sc, raw)
	if serr == nil && p.name != nil {
		serr = w.checkNewName(*p.name, sc)
	}

	text := sc.text
	if serr == nil && p.blobID != "" {
		text, serr = scriptText(w.sess, w.o, p.blobID)
	}

	return sc, p, text, serr
}

// readScriptPatch decodes an update: a name, a blobId, and the server-set
// properties only at the value they have.
func readScriptPatch(sc *sieveScript, raw map[string]json.RawMessage) (*scriptPatch, *setError) {
	p := &scriptPatch{}

	for k, v := range raw {
		switch k {
		case propName:
			var name string
			if json.Unmarshal(v, &name) != nil {
				return nil, invalidProperty(k, "a name is a string")
			}

			p.name = &name
		case propBlobID:
			if json.Unmarshal(v, &p.blobID) != nil || p.blobID == "" {
				return nil, invalidProperty(k, "a blobId is an id")
			}
		case propIsActive, propID:
			if serr := checkServerSet(sc, k, v); serr != nil {
				return nil, serr
			}
		default:
			return nil, invalidProperty(k, "a SieveScript has no property "+k)
		}
	}

	return p, nil
}

// checkServerSet accepts a server-set property in an update only at the
// value it has.
func checkServerSet(sc *sieveScript, k string, v json.RawMessage) *setError {
	if k == propIsActive {
		var active bool
		if json.Unmarshal(v, &active) != nil || active != sc.active {
			return invalidProperty(k, "the server sets isActive; use onSuccessActivateScript or onSuccessDeactivateScript")
		}

		return nil
	}

	var got string
	if json.Unmarshal(v, &got) != nil || got != sc.id {
		return invalidProperty(k, "the id of a script does not change")
	}

	return nil
}

// rename gives a script a new name and moves its id along: the id is
// registered for the new name first, so a listing during the rename
// finds it under either name, and the old entry goes afterwards. A
// rename that fails takes the new entry back.
func (w *scriptWriter) rename(sc *sieveScript, name string) *setError {
	listed := w.book.names()
	listed[name] = true

	if _, err := changeScriptIDs(w.sess, listed, func(ids map[string]string) { ids[name] = sc.id }); err != nil {
		return &setError{Type: setServerFail, Description: err.Error()}
	}

	err := w.sess.user.RenameScript(sc.name, name)

	switch {
	case errors.Is(err, model.ErrScriptExists):
		w.forgetID(name, sc.id)

		return &setError{Type: setAlreadyExists, Description: descScriptExists}
	case errors.Is(err, model.ErrNoScript):
		// Renamed or deleted over ManageSieve since the call began.
		w.forgetID(name, sc.id)

		return &setError{Type: setNotFound, Description: "the script changed meanwhile"}
	case err != nil:
		w.forgetID(name, sc.id)

		return &setError{Type: setServerFail, Description: err.Error()}
	}

	old := sc.name
	sc.name = name
	w.forgetID(old, sc.id)

	return nil
}

// renameBack undoes a rename whose content write failed.
func (w *scriptWriter) renameBack(sc *sieveScript, old string) {
	if serr := w.rename(sc, old); serr != nil {
		w.sess.log.Error("jmap: a failed script update could not rename the script back", "script", sc.name, "err", serr.Description)
	}
}

// destroy removes a script. The active one stays (RFC 9661 2.4: it is
// deactivated first, in a /set of its own).
func (w *scriptWriter) destroy(id string) *setError {
	sc := w.book.byID(id)

	switch {
	case sc == nil:
		return &setError{Type: setNotFound}
	case model.ReservedScript(sc.name):
		return &setError{Type: setForbidden, Description: descReservedUpdate}
	case sc.active:
		return &setError{Type: setSieveIsActive, Description: descScriptIsActive}
	}

	err := w.sess.user.DeleteScript(sc.name)

	switch {
	case errors.Is(err, model.ErrScriptActive):
		return &setError{Type: setSieveIsActive, Description: descScriptIsActive}
	case err != nil && !errors.Is(err, model.ErrNoScript):
		return &setError{Type: setServerFail, Description: err.Error()}
	}

	w.forgetID(sc.name, sc.id)

	for i, s := range w.book.list {
		if s == sc {
			w.book.list = append(w.book.list[:i], w.book.list[i+1:]...)
			break
		}
	}

	w.out.Destroyed = append(w.out.Destroyed, id)

	return nil
}

// onSuccess deactivates and activates, in that order (RFC 9661 2.4). An
// id that names no script is ignored, as the RFC asks; so is a script
// that does not compile for delivery — ManageSieve's SETACTIVE refuses
// it, and here there is no error to report it with, so the client sees
// it stay inactive. What changed is reported as isActive.
func (w *scriptWriter) onSuccess(req *sieveSetRequest) {
	if req.OnSuccessDeactivateScript {
		if a := w.book.active(); a != nil {
			if err := w.sess.user.SetActiveScript(""); err != nil {
				w.sess.log.Error("jmap: deactivate the Sieve script", "err", err)

				return
			}

			w.markActive(a, false)
		}
	}

	if req.OnSuccessActivateScript == nil {
		return
	}

	id, _ := resolveCreationID(w.sess, *req.OnSuccessActivateScript)

	sc := w.book.byID(id)
	if sc == nil || sc.active {
		return
	}

	if err := sieve.CompileForDelivery(sc.text, sieveHooks(w.sess, w.o)); err != nil {
		w.sess.log.Info("jmap: a script that does not compile for delivery was not activated", "err", err)

		return
	}

	prev := w.book.active()

	if err := w.sess.user.SetActiveScript(sc.name); err != nil {
		w.sess.log.Error("jmap: activate the Sieve script", "err", err)

		return
	}

	if prev != nil {
		w.markActive(prev, false)
	}

	w.markActive(sc, true)
}

// markActive records a change of activity in the script and in the
// response: in created for a script this call created, in updated
// otherwise.
func (w *scriptWriter) markActive(sc *sieveScript, active bool) {
	sc.active = active

	if cid, ok := w.created[sc.id]; ok {
		w.out.Created[cid][propIsActive] = active

		return
	}

	changed, _ := w.out.Updated[sc.id].(map[string]any)
	if changed == nil {
		changed = map[string]any{}
	}

	changed[propIsActive] = active
	w.out.Updated[sc.id] = changed
}
