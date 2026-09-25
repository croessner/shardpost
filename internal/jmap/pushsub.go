package jmap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/webpush"
)

// PushSubscription (RFC 8620 7.2). The subscriptions are kept in the
// shared admin store, because the process that sends Web Push — the push
// worker, not the front-end that took the subscription — has to find
// them. A subscription is written here and served there; the worker
// sends the verification, and only a verified subscription is pushed to.
//
// RFC 8620 ties a subscription to the credentials that made it. This
// server ties it to the account: a password change does not end it, and
// the worker drops the subscriptions of an account that no longer
// resolves (see pushworker.go).

// The key layout in the admin store. The subscriptions of an account are
// one record, so a request reads one key; the worker finds the accounts
// by listing the prefix, and the verifications it has to send in one
// queue record.
const (
	pushAcctPrefix  = "jmap/push/acct/"   // + account -> pushAccount
	pushVerifyQueue = "jmap/push/verifyq" // -> []verifyItem
	pushSeenPrefix  = "jmap/push/seen/"   // + account -> the state last pushed
)

// pushAcctKey and pushSeenKey are the admin store keys of an account.
func pushAcctKey(account string) string { return pushAcctPrefix + url.PathEscape(account) }

func pushSeenKey(account string) string { return pushSeenPrefix + url.PathEscape(account) }

// pushRecord is a subscription as the admin store keeps it. Types nil
// means every type; an empty list means none.
type pushRecord struct {
	Keys           *webpush.Keys `json:"keys,omitempty"`
	Expires        time.Time     `json:"expires"`
	Created        time.Time     `json:"created"`
	ID             string        `json:"id"`
	Account        string        `json:"account"`
	DeviceClientID string        `json:"deviceClientId"`
	URL            string        `json:"url"`
	Code           string        `json:"code"`
	VAPIDKey       string        `json:"vapidKey,omitempty"`
	Types          []string      `json:"types"`
	Verified       bool          `json:"verified"`
}

// wants is the type filter of a subscription (nil: all types).
func (rec *pushRecord) wants() map[string]bool {
	if rec.Types == nil {
		return nil
	}

	out := map[string]bool{}
	for _, t := range rec.Types {
		out[t] = true
	}

	return out
}

// pushAccount is the record of an account's subscriptions.
type pushAccount struct {
	Account string        `json:"account"`
	Subs    []*pushRecord `json:"subs"`
}

// find is the subscription with the id.
func (pa *pushAccount) find(id string) *pushRecord {
	for _, rec := range pa.Subs {
		if rec.ID == id {
			return rec
		}
	}

	return nil
}

// verifyItem is a verification the worker has to send.
type verifyItem struct {
	Account  string `json:"account"`
	ID       string `json:"id"`
	Attempts int    `json:"attempts"`
}

// The properties of a PushSubscription.
const (
	propDeviceClientID   = "deviceClientId"
	propURL              = "url"
	propPushKeys         = "keys"
	propVerificationCode = "verificationCode"
	propExpires          = "expires"
	propTypes            = "types"
)

// pushDefaultProperties are what PushSubscription/get returns: everything
// except url and keys, which RFC 8620 7.2.1 keeps private to the device.
var pushDefaultProperties = []string{propID, propDeviceClientID, propVerificationCode, propExpires, propTypes}

// Bounds of what a subscription may carry.
const (
	maxPushURL   = 2048
	maxPushTypes = 32
	maxTypeName  = 64
)

// errNoPush is what a server without push subscriptions answers.
var errNoPush = errors.New("push subscriptions are not configured")

// pushStore is the admin store of the session's server.
func pushStore(sess *session) (model.AdminKV, *methodError) {
	if sess.srv == nil || sess.srv.o.Push.KV == nil {
		return nil, &methodError{Type: errUnknownMethod, Description: errNoPush.Error()}
	}

	return sess.srv.o.Push.KV, nil
}

// loadPushAccount reads the subscriptions of an account.
func loadPushAccount(kv model.AdminKV, account string) (*pushAccount, error) {
	raw, err := kv.Get(pushAcctKey(account))

	switch {
	case errors.Is(err, model.ErrNotFound):
		return &pushAccount{Account: account}, nil
	case err != nil:
		return nil, err
	}

	pa := &pushAccount{}
	if err := json.Unmarshal(raw, pa); err != nil || pa.Account != account {
		return &pushAccount{Account: account}, nil
	}

	return pa, nil
}

// updatePushAccount changes the subscriptions of an account atomically;
// fn returning an error writes nothing. An account left without
// subscriptions loses its record.
func updatePushAccount(kv model.AdminKV, account string, fn func(*pushAccount) error) error {
	return kv.Update(pushAcctKey(account), func(cur []byte) ([]byte, error) {
		pa := &pushAccount{Account: account}
		if cur != nil {
			if err := json.Unmarshal(cur, pa); err != nil || pa.Account != account {
				pa = &pushAccount{Account: account}
			}
		}

		if err := fn(pa); err != nil {
			return nil, err
		}

		if len(pa.Subs) == 0 {
			return nil, nil
		}

		return json.Marshal(pa)
	})
}

// queueVerify adds a verification for the worker to send.
func queueVerify(kv model.AdminKV, account, id string) error {
	return kv.Update(pushVerifyQueue, func(cur []byte) ([]byte, error) {
		var q []verifyItem
		if cur != nil {
			_ = json.Unmarshal(cur, &q)
		}

		return json.Marshal(append(q, verifyItem{Account: account, ID: id}))
	})
}

// pushGetRequest is the argument shape of PushSubscription/get: a /get
// without an account.
type pushGetRequest struct {
	IDs        *[]string `json:"ids"`
	Properties *[]string `json:"properties"`
}

// pushSubscriptionGet implements PushSubscription/get.
func pushSubscriptionGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	kv, merr := pushStore(sess)
	if merr != nil {
		return nil, merr
	}

	var req pushGetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &methodError{Type: errInvalidArguments, Description: err.Error()}
	}

	props, merr := pushProperties(req.Properties)
	if merr != nil {
		return nil, merr
	}

	if req.IDs != nil && len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	pa, err := loadPushAccount(kv, sess.account)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	list := []map[string]any{}
	notFound := []string{}

	var ids []string
	if req.IDs != nil {
		ids = *req.IDs
	} else {
		for _, rec := range pa.Subs {
			ids = append(ids, rec.ID)
		}
	}

	for _, id := range ids {
		rec := pa.find(id)
		if rec == nil {
			notFound = append(notFound, id)
			continue
		}

		list = append(list, rec.render(props))
	}

	return map[string]any{"list": list, "notFound": notFound}, nil
}

// pushProperties checks the properties a PushSubscription/get asks for:
// url and keys are forbidden (RFC 8620 7.2.1).
func pushProperties(props *[]string) ([]string, *methodError) {
	if props == nil {
		return pushDefaultProperties, nil
	}

	for _, p := range *props {
		switch p {
		case propURL, propPushKeys:
			return nil, &methodError{Type: errForbidden, Description: p + " is not returned (RFC 8620 7.2.1)"}
		case propID, propDeviceClientID, propVerificationCode, propExpires, propTypes:
		default:
			return nil, &methodError{Type: errInvalidArguments, Arguments: []string{argProperties},
				Description: "no such property: " + p}
		}
	}

	return *props, nil
}

// render is a subscription as PushSubscription/get returns it.
func (rec *pushRecord) render(props []string) map[string]any {
	out := map[string]any{}

	for _, p := range props {
		switch p {
		case propID:
			out[p] = rec.ID
		case propDeviceClientID:
			out[p] = rec.DeviceClientID
		case propVerificationCode:
			out[p] = nil
			if rec.Verified {
				out[p] = rec.Code
			}
		case propExpires:
			out[p] = utcDate(rec.Expires)
		case propTypes:
			out[p] = nil
			if rec.Types != nil {
				out[p] = rec.Types
			}
		}
	}

	return out
}

// pushSetRequest is the argument shape of PushSubscription/set: a /set
// without an account or states.
type pushSetRequest struct {
	Create  map[string]json.RawMessage            `json:"create"`
	Update  map[string]map[string]json.RawMessage `json:"update"`
	Destroy []string                              `json:"destroy"`
}

// pushSetResponse is what PushSubscription/set answers.
type pushSetResponse struct {
	Created      map[string]map[string]any `json:"created"`
	Updated      map[string]any            `json:"updated"`
	NotCreated   map[string]*setError      `json:"notCreated"`
	NotUpdated   map[string]*setError      `json:"notUpdated"`
	NotDestroyed map[string]*setError      `json:"notDestroyed"`
	Destroyed    []string                  `json:"destroyed"`
}

// pushSubscriptionSet implements PushSubscription/set.
func pushSubscriptionSet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	kv, merr := pushStore(sess)
	if merr != nil {
		return nil, merr
	}

	var req pushSetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &methodError{Type: errInvalidArguments, Description: err.Error()}
	}

	if n := len(req.Create) + len(req.Update) + len(req.Destroy); n > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	out := &pushSetResponse{Created: map[string]map[string]any{}, Updated: map[string]any{},
		NotCreated: map[string]*setError{}, NotUpdated: map[string]*setError{}, NotDestroyed: map[string]*setError{},
		Destroyed: []string{}}

	for cid, raw := range req.Create {
		created, serr := createPush(sess, kv, raw)
		if serr != nil {
			out.NotCreated[cid] = serr
			continue
		}

		out.Created[cid] = created
		sess.created[cid], _ = created[propID].(string)
	}

	for ref, patch := range req.Update {
		id, _ := resolveCreationID(sess, ref)

		changed, serr := updatePush(sess, kv, id, patch)
		if serr != nil {
			out.NotUpdated[ref] = serr
			continue
		}

		out.Updated[id] = changed
	}

	for _, ref := range req.Destroy {
		id, _ := resolveCreationID(sess, ref)
		if serr := destroyPush(sess, kv, id); serr != nil {
			out.NotDestroyed[ref] = serr
			continue
		}

		out.Destroyed = append(out.Destroyed, id)
	}

	return out, nil
}

// pushCreate is what a client may give a new subscription.
type pushCreate struct {
	Keys             *webpush.Keys `json:"keys"`
	VerificationCode *string       `json:"verificationCode"`
	Expires          *string       `json:"expires"`
	DeviceClientID   string        `json:"deviceClientId"`
	URL              string        `json:"url"`
	Types            []string      `json:"types"`
}

// pushCreatable are the properties a creation may carry.
var pushCreatable = map[string]bool{
	propDeviceClientID: true, propURL: true, propPushKeys: true, propVerificationCode: true,
	propExpires: true, propTypes: true,
}

// createPush checks and stores a new subscription, and queues its
// verification for the worker.
func createPush(sess *session, kv model.AdminKV, raw json.RawMessage) (map[string]any, *setError) {
	spec, serr := readPushCreate(raw)
	if serr != nil {
		return nil, serr
	}

	if serr := checkPushURL(spec.URL, sess.srv.o.Push.AllowInternalURLs); serr != nil {
		return nil, serr
	}

	expires, clamped := pushExpiry(sess, spec.Expires)

	rec := &pushRecord{ID: "P" + randomHex(16), Account: sess.account, DeviceClientID: spec.DeviceClientID,
		URL: spec.URL, Keys: spec.Keys, Code: randomHex(16), Expires: expires, Created: time.Now().UTC(),
		Types: spec.Types, VAPIDKey: sess.srv.o.Push.VAPIDPublicKey}

	err := updatePushAccount(kv, sess.account, func(pa *pushAccount) error {
		if len(pa.Subs) >= sess.srv.o.Push.MaxSubscriptions {
			return errTooManyPush
		}

		pa.Subs = append(pa.Subs, rec)

		return nil
	})

	switch {
	case errors.Is(err, errTooManyPush):
		return nil, &setError{Type: setForbidden, Description: "this account has as many push subscriptions as it may"}
	case err != nil:
		return nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	if err := queueVerify(kv, rec.Account, rec.ID); err != nil {
		return nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	created := map[string]any{propID: rec.ID}
	if clamped {
		created[propExpires] = utcDate(rec.Expires)
	}

	return created, nil
}

// errTooManyPush aborts a creation beyond the per-account limit.
var errTooManyPush = errors.New("too many push subscriptions")

// readPushCreate decodes and checks a creation.
func readPushCreate(raw json.RawMessage) (*pushCreate, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	for k := range given {
		if !pushCreatable[k] {
			return nil, invalidProperty(k, "a push subscription has no property "+k+" a client may set")
		}
	}

	spec := &pushCreate{}
	if err := json.Unmarshal(raw, spec); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	switch {
	case spec.DeviceClientID == "" || len(spec.DeviceClientID) > 255:
		return nil, invalidProperty(propDeviceClientID, "a push subscription needs a deviceClientId")
	case spec.VerificationCode != nil:
		return nil, invalidProperty(propVerificationCode, "the verification code is the server's to send")
	case spec.Keys != nil && webpush.CheckKeys(*spec.Keys) != nil:
		return nil, invalidProperty(propPushKeys, webpush.ErrBadKeys.Error())
	case !validTypes(spec.Types):
		return nil, invalidProperty(propTypes, "at most 32 type names")
	}

	if u, err := url.Parse(spec.URL); err == nil {
		spec.URL = u.String() // the scheme and host in lower case
	}

	return spec, nil
}

// validTypes bounds a list of type names.
func validTypes(types []string) bool {
	if len(types) > maxPushTypes {
		return false
	}

	for _, t := range types {
		if t == "" || len(t) > maxTypeName {
			return false
		}
	}

	return true
}

// checkPushURL refuses a push URL this server will not post to: anything
// but https, and a host that is an internal address. A name that resolves
// to one is refused by the worker when it connects.
func checkPushURL(raw string, allowInternal bool) *setError {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || len(raw) > maxPushURL || u.User != nil {
		return invalidProperty(propURL, "a push URL is an absolute https URL")
	}

	if allowInternal {
		return nil
	}

	if ip := net.ParseIP(u.Hostname()); ip != nil && webpush.Internal(ip) {
		return invalidProperty(propURL, "a push URL may not point into the server's network")
	}

	if strings.EqualFold(u.Hostname(), "localhost") {
		return invalidProperty(propURL, "a push URL may not point into the server's network")
	}

	return nil
}

// pushExpiry is the expiry of a subscription: what the client asked for,
// within the server's maximum, and the maximum when it asked for none.
// clamped reports that the result differs from the request.
func pushExpiry(sess *session, want *string) (time.Time, bool) {
	limit := time.Now().UTC().Add(sess.srv.o.Push.MaxExpiry).Truncate(time.Second)

	if want == nil {
		return limit, true
	}

	t, ok := parseUTCDate(*want)
	if !ok || t.After(limit) {
		return limit, true
	}

	return t.UTC(), false
}

// errNoSuchPush aborts an update of a subscription that is not there,
// errPatch one whose patch was refused.
var (
	errNoSuchPush = errors.New("no such push subscription")
	errPatch      = errors.New("the patch was refused")
)

// updatePush applies an update: the verification code, the expiry and
// the types may change; url, keys and deviceClientId may not.
func updatePush(sess *session, kv model.AdminKV, id string, patch map[string]json.RawMessage) (map[string]any, *setError) {
	var (
		changed map[string]any
		serr    *setError
	)

	err := updatePushAccount(kv, sess.account, func(pa *pushAccount) error {
		rec := pa.find(id)
		if rec == nil {
			return errNoSuchPush
		}

		next := *rec
		if changed, serr = patchPush(sess, &next, patch); serr != nil {
			return errPatch
		}

		*rec = next

		return nil
	})

	switch {
	case serr != nil:
		return nil, serr
	case errors.Is(err, errNoSuchPush):
		return nil, &setError{Type: setNotFound}
	case err != nil:
		return nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	return changed, nil
}

// patchPush applies a patch to a record.
func patchPush(sess *session, rec *pushRecord, patch map[string]json.RawMessage) (map[string]any, *setError) {
	var changed map[string]any

	for k, v := range patch {
		switch k {
		case propVerificationCode:
			var code string
			if json.Unmarshal(v, &code) != nil || code != rec.Code {
				return nil, invalidProperty(k, "not the verification code that was sent")
			}

			rec.Verified = true
		case propExpires:
			var s *string
			if json.Unmarshal(v, &s) != nil {
				return nil, invalidProperty(k, "expires is a UTCDate or null")
			}

			var clamped bool
			if rec.Expires, clamped = pushExpiry(sess, s); clamped {
				changed = map[string]any{propExpires: utcDate(rec.Expires)}
			}
		case propTypes:
			var types []string
			if json.Unmarshal(v, &types) != nil || !validTypes(types) {
				return nil, invalidProperty(k, "types is a list of at most 32 type names, or null")
			}

			rec.Types = types
		default:
			return nil, invalidProperty(k, k+" cannot change; destroy the subscription and create a new one")
		}
	}

	return changed, nil
}

// destroyPush removes a subscription, and with it its URL and keys.
func destroyPush(sess *session, kv model.AdminKV, id string) *setError {
	err := updatePushAccount(kv, sess.account, func(pa *pushAccount) error {
		for i, rec := range pa.Subs {
			if rec.ID == id {
				pa.Subs = append(pa.Subs[:i], pa.Subs[i+1:]...)

				return nil
			}
		}

		return errNoSuchPush
	})

	switch {
	case errors.Is(err, errNoSuchPush):
		return &setError{Type: setNotFound}
	case err != nil:
		return &setError{Type: setServerFail, Description: err.Error()}
	}

	return nil
}

// randomHex is n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
