package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message/textproto"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/sendqueue"
)

// sortSubmissions orders the submissions by the comparators, then by
// sendAt and id so that equal keys keep one order.
func sortSubmissions(list []*submission, by []comparator) {
	sort.SliceStable(list, func(i, j int) bool {
		for _, c := range by {
			a, b := list[i].sortKeyOf(c.Property), list[j].sortKeyOf(c.Property)
			if a == b {
				continue
			}

			if c.ascending() {
				return a < b
			}

			return a > b
		}

		if !list[i].SendAt.Equal(list[j].SendAt) {
			return list[i].SendAt.Before(list[j].SendAt)
		}

		return list[i].ID < list[j].ID
	})
}

// submissionSetRequest is EmailSubmission/set: a /set plus the two
// arguments of RFC 8621 7.5.
type submissionSetRequest struct {
	setRequest
	OnSuccessUpdateEmail  map[string]map[string]json.RawMessage `json:"onSuccessUpdateEmail"`
	OnSuccessDestroyEmail []string                              `json:"onSuccessDestroyEmail"`
}

// submissionSetResponse is the /set response with the response of the
// implicit Email/set that follows it.
type submissionSetResponse struct {
	*setResponse
	then []invocation
}

func (r *submissionSetResponse) followUps() []invocation { return r.then }

// MarshalJSON writes the /set response alone.
func (r *submissionSetResponse) MarshalJSON() ([]byte, error) { return json.Marshal(r.setResponse) }

// submissionRun is one EmailSubmission/set being carried out.
type submissionRun struct {
	sess   *session
	opts   *SubmissionOptions
	kv     model.AdminKV
	idents *identityBook
	loc    *locator
	out    *setResponse
	// emailOf is the email of every submission the call touched, by id,
	// for onSuccessUpdateEmail and onSuccessDestroyEmail.
	emailOf map[string]string
}

// emailSubmissionSet implements EmailSubmission/set.
func emailSubmissionSet(ctx context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	opts, kv, merr := submissionStore(sess)
	if merr != nil {
		return nil, merr
	}

	var req submissionSetRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if n := len(req.Create) + len(req.Update) + len(req.Destroy); n > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	run, merr := newSubmissionRun(ctx, sess, opts, kv, req.IfInState)
	if merr != nil {
		return nil, merr
	}

	for cid, raw := range req.Create {
		run.create(ctx, cid, raw)
	}

	for ref, patch := range req.Update {
		run.update(ref, patch)
	}

	for _, ref := range req.Destroy {
		run.destroy(ref)
	}

	if b, err := loadSubmissions(kv, sess.account, opts); err == nil {
		run.out.NewState = stateString(b.State)
	}

	res := &submissionSetResponse{setResponse: run.out}
	res.then = run.onSuccess(ctx, &req)

	return res, nil
}

// newSubmissionRun reads what a /set needs: the state, the identities
// and the locator.
func newSubmissionRun(ctx context.Context, sess *session, opts *SubmissionOptions, kv model.AdminKV,
	ifInState *string,
) (*submissionRun, *methodError) {
	b, err := loadSubmissions(kv, sess.account, opts)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	state := stateString(b.State)
	if ifInState != nil && *ifInState != state {
		return nil, &methodError{Type: errStateMismatch}
	}

	idents, merr := loadIdentities(ctx, sess)
	if merr != nil {
		return nil, merr
	}

	_, loc, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	return &submissionRun{sess: sess, opts: opts, kv: kv, idents: idents, loc: loc,
		out: newSetResponse(accountID(sess.account), state), emailOf: map[string]string{}}, nil
}

// submissionCreate is what a client gives a new submission.
type submissionCreate struct {
	Envelope   *envelope `json:"envelope"`
	IdentityID string    `json:"identityId"`
	EmailID    string    `json:"emailId"`
}

// create sends one message and records the submission.
func (r *submissionRun) create(ctx context.Context, cid string, raw json.RawMessage) {
	s, serr := r.send(ctx, raw)
	if serr != nil {
		r.out.NotCreated[cid] = serr

		return
	}

	err := updateSubmissions(r.kv, r.sess.account, r.opts, func(b *submissionBook) error {
		b.Subs = append(b.Subs, s)
		b.note(s.ID, model.OpCreated)

		return nil
	})
	if err != nil {
		// The message is sent; only its record is missing. The client is
		// told it was created, since sending it again would send it twice.
		r.sess.log.Warn("jmap: submission sent but not recorded", "id", s.ID, "err", err)
	}

	r.out.Created[cid] = s.render([]string{propThreadID, propEnvelope, propSendAt, propUndoStatus,
		propDeliveryStatus, propDSNBlobIDs, propMDNBlobIDs})
	r.sess.created[cid] = s.ID
	r.emailOf[s.ID] = s.EmailID
}

// send checks a creation and hands the message to the relay.
func (r *submissionRun) send(ctx context.Context, raw json.RawMessage) (*submission, *setError) {
	spec, serr := readSubmissionCreate(raw)
	if serr != nil {
		return nil, serr
	}

	ident := r.idents.find(spec.IdentityID)
	if ident == nil {
		return nil, invalidProperty(propIdentityID, "no identity "+spec.IdentityID)
	}

	emailID, _ := resolveCreationID(r.sess, spec.EmailID)

	msg, meta, serr := r.message(emailID)
	if serr != nil {
		return nil, serr
	}

	hdr, _ := splitHeader(msg)

	if serr := checkOriginator(hdr); serr != nil {
		return nil, serr
	}

	env, release, serr := r.envelopeOf(spec.Envelope, hdr, ident)
	if serr != nil {
		return nil, serr
	}

	if serr := r.checkFrom(hdr); serr != nil {
		return nil, serr
	}

	if !release.IsZero() {
		return r.schedule(ident, emailID, mailmeta.ThreadID(meta), env, release, stripBcc(msg))
	}

	rep, serr := r.relay(ctx, env, stripBcc(msg))
	if serr != nil {
		return nil, serr
	}

	s := &submission{ID: "S" + randomHex(12), IdentityID: ident.ID, EmailID: emailID, ThreadID: mailmeta.ThreadID(meta),
		Envelope: *env, SendAt: time.Now().UTC(), UndoStatus: undoFinal, DeliveryStatus: map[string]deliveryStatus{}}

	for _, rc := range env.RcptTo {
		s.DeliveryStatus[rc.Email] = deliveryStatus{SMTPReply: rep.Final.String(), Delivered: "queued", Displayed: lookupUnknown}
	}

	return s, nil
}

// schedule hands a submission with a release time to the delayed
// submission queue: the copy of the message, the queue entry and the
// record are written before the client hears of it.
func (r *submissionRun) schedule(ident *identity, emailID, threadID string, env *envelope, release time.Time, msg []byte) (*submission, *setError) {
	to := make([]string, 0, len(env.RcptTo))
	for _, rc := range env.RcptTo {
		to = append(to, rc.Email)
	}

	id := "S" + randomHex(12)
	sp := sendqueue.Spec{Account: r.sess.account, ID: id, Release: release, MailFrom: env.MailFrom.Email, RcptTo: to,
		XClient: r.opts.XClient, Raw: msg}

	if r.opts.XClient {
		sp.Login = r.sess.account
		if r.opts.XClientAddr {
			sp.ClientAddr = r.sess.remote
		}
	}

	if _, err := r.opts.Delayed.Queue.Schedule(r.sess.user, sp); err != nil {
		r.sess.log.Warn("jmap: delayed submission not scheduled", "err", err)

		return nil, &setError{Type: setServerFail, Description: "the submission could not be scheduled; try again later"}
	}

	return &submission{ID: id, IdentityID: ident.ID, EmailID: emailID, ThreadID: threadID, Envelope: *env,
		SendAt: release.UTC(), UndoStatus: undoPending}, nil
}

// submissionCreatable are the properties a creation may carry.
var submissionCreatable = map[string]bool{propIdentityID: true, propEmailID: true, propEnvelope: true}

// readSubmissionCreate decodes a creation.
func readSubmissionCreate(raw json.RawMessage) (*submissionCreate, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	for k := range given {
		if !submissionCreatable[k] {
			return nil, invalidProperty(k, "the server sets "+k)
		}
	}

	spec := &submissionCreate{}
	if err := json.Unmarshal(raw, spec); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	if spec.IdentityID == "" || spec.EmailID == "" {
		return nil, invalidProperty(propEmailID, "a submission names an identity and an email")
	}

	return spec, nil
}

// message reads the message to send.
func (r *submissionRun) message(emailID string) ([]byte, *model.Meta, *setError) {
	hits, err := r.loc.resolve([]string{emailID})
	if err != nil {
		return nil, nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	f, ok := hits[emailID]
	if !ok {
		// An email created earlier in the same request is not in the
		// locator yet; bring it up to date once and look again.
		if _, loc, merr := emailState(r.sess); merr == nil {
			r.loc = loc
			hits, _ = loc.resolve([]string{emailID})
			f, ok = hits[emailID]
		}
	}

	if !ok {
		return nil, nil, invalidProperty(propEmailID, "no email "+emailID)
	}

	meta := &f.metas[0]
	if meta.Size > r.opts.MaxSize {
		return nil, nil, &setError{Type: setTooLarge, MaxSize: r.opts.MaxSize, Description: "the message is larger than may be sent"}
	}

	msg, err := r.sess.user.Read(meta)
	if err != nil {
		return nil, nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	return msg, meta, nil
}

// envelopeOf is the envelope of a submission: the one given, checked, or
// the one RFC 8621 7 derives from the message; release is the time a
// FUTURERELEASE parameter asks for (zero: send now).
func (r *submissionRun) envelopeOf(given *envelope, hdr textproto.Header, ident *identity) (*envelope, time.Time, *setError) {
	env := given
	if env == nil {
		var serr *setError
		if env, serr = r.deriveEnvelope(hdr, ident); serr != nil {
			return nil, time.Time{}, serr
		}
	}

	release, serr := checkParameters(env, r.opts.delayed(), time.Now())
	if serr != nil {
		return nil, time.Time{}, serr
	}

	// A bare addr-spec only, as for the recipients: a display name or
	// comment would otherwise reach MAIL FROM verbatim.
	if a, err := mail.ParseAddress(env.MailFrom.Email); err != nil || a.Address != env.MailFrom.Email {
		return nil, time.Time{}, &setError{Type: setForbiddenMailFrom, Description: "not an address: " + env.MailFrom.Email}
	}

	if r.opts.CheckSender && !r.idents.allowed[strings.ToLower(env.MailFrom.Email)] {
		return nil, time.Time{}, &setError{Type: setForbiddenMailFrom, Description: "this account may not send as " + env.MailFrom.Email}
	}

	return env, release, checkRecipients(env, r.opts.MaxRecipients)
}

// deriveEnvelope builds the envelope from the message (RFC 8621 7): the
// sender is the Sender, else the From, if the identity may use it, else
// the identity's address; the recipients are To, Cc and Bcc, each once.
// checkOriginator has made sure there is at most one of each field.
func (r *submissionRun) deriveEnvelope(hdr textproto.Header, ident *identity) (*envelope, *setError) {
	env := &envelope{MailFrom: envAddress{Email: ident.Email}}

	for _, field := range []string{fieldSender, fieldFrom} {
		list, err := parseAddresses(hdr.Get(field))
		if err != nil || len(list) == 0 {
			continue
		}

		if r.idents.allowed[strings.ToLower(list[0].Address)] {
			env.MailFrom.Email = list[0].Address
		}

		break
	}

	seen := map[string]bool{}

	for _, field := range []string{fieldTo, fieldCc, fieldBcc} {
		for _, v := range hdr.Values(field) {
			list, err := parseAddresses(v)
			if err != nil {
				// A recipient field that does not parse would drop its
				// recipients without a word; the client is told instead.
				return nil, &setError{Type: setInvalidEmail, Properties: []string{strings.ToLower(field)},
					Description: "the " + field + " field does not parse"}
			}

			for _, a := range list {
				if key := strings.ToLower(a.Address); !seen[key] {
					seen[key] = true

					env.RcptTo = append(env.RcptTo, envAddress{Email: a.Address})
				}
			}
		}
	}

	return env, nil
}

// checkOriginator refuses a message with more than one From or Sender
// field, or with several addresses in From and no Sender (RFC 5322 3.6.2;
// RFC 8621 7 says a server SHOULD reject them): a second From is how a
// message would show a sender nobody checked.
func checkOriginator(hdr textproto.Header) *setError {
	froms, senders := hdr.Values(fieldFrom), hdr.Values(fieldSender)

	switch {
	case len(froms) > 1:
		return &setError{Type: setInvalidEmail, Properties: []string{propFrom}, Description: "the message has more than one From field"}
	case len(senders) > 1:
		return &setError{Type: setInvalidEmail, Properties: []string{propSender}, Description: "the message has more than one Sender field"}
	}

	if len(senders) == 1 {
		if list, err := parseAddresses(senders[0]); err != nil || len(list) != 1 {
			return &setError{Type: setInvalidEmail, Properties: []string{propSender}, Description: "the Sender field is not one address"}
		}
	}

	if len(froms) == 1 && len(senders) == 0 {
		if list, err := parseAddresses(froms[0]); err == nil && len(list) > 1 {
			return &setError{Type: setInvalidEmail, Properties: []string{propSender},
				Description: "a From with several addresses needs a Sender"}
		}
	}

	return nil
}

// checkParameters checks the SMTP parameters of an envelope and returns
// the release time a FUTURERELEASE parameter asks for (zero: send now).
// The only parameters offered are HOLDFOR and HOLDUNTIL of MAIL FROM,
// and only with delayed sending (submissionExtensions); everything else
// is refused, as is a hold beyond maxDelayedSend, a second hold
// parameter, or one without a valid value (RFC 4865 4.2). A HOLDUNTIL
// that has passed, and HOLDFOR=0, send now.
func checkParameters(env *envelope, delayed *DelayedOptions, now time.Time) (time.Time, *setError) {
	for _, rc := range env.RcptTo {
		if len(rc.Parameters) > 0 {
			return time.Time{}, invalidProperty(propEnvelope, "this server passes no RCPT TO parameters")
		}
	}

	if len(env.MailFrom.Parameters) == 0 {
		return time.Time{}, nil
	}

	if delayed == nil {
		return time.Time{}, invalidProperty(propEnvelope, "this server passes no SMTP parameters (submissionExtensions is empty)")
	}

	if len(env.MailFrom.Parameters) > 1 {
		return time.Time{}, invalidProperty(propEnvelope, "only one MAIL FROM parameter, HOLDFOR or HOLDUNTIL, is accepted")
	}

	for k, v := range env.MailFrom.Parameters {
		if v == nil {
			return time.Time{}, invalidProperty(propEnvelope, k+" needs a value")
		}

		switch strings.ToUpper(k) {
		case paramHoldFor:
			return holdFor(*v, delayed.MaxDelay, now)
		case paramHoldUntil:
			return holdUntil(*v, delayed.MaxDelay, now)
		}

		return time.Time{}, invalidProperty(propEnvelope, "unsupported MAIL FROM parameter "+k+" (only HOLDFOR and HOLDUNTIL)")
	}

	return time.Time{}, nil
}

// holdFor reads HOLDFOR: whole seconds, at most maxDelay.
func holdFor(v string, maxDelay time.Duration, now time.Time) (time.Time, *setError) {
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil || strings.TrimSpace(v) != v || v == "" {
		return time.Time{}, invalidProperty(propEnvelope, "HOLDFOR is not a number of seconds: "+v)
	}

	d := time.Duration(n) * time.Second
	if d > maxDelay {
		return time.Time{}, invalidProperty(propEnvelope, "HOLDFOR is longer than maxDelayedSend")
	}

	if n == 0 {
		return time.Time{}, nil
	}

	return now.Add(d).UTC(), nil
}

// holdUntilSlack is how far beyond now + maxDelay a HOLDUNTIL may lie: a
// client computes it from the advertised latest time, on its own clock.
const holdUntilSlack = time.Minute

// holdUntil reads HOLDUNTIL: an RFC 3339 date-time, at most maxDelay
// ahead.
func holdUntil(v string, maxDelay time.Duration, now time.Time) (time.Time, *setError) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, invalidProperty(propEnvelope, "HOLDUNTIL is not an RFC 3339 date-time: "+v)
	}

	if t.After(now.Add(maxDelay + holdUntilSlack)) {
		return time.Time{}, invalidProperty(propEnvelope, "HOLDUNTIL lies further ahead than maxDelayedSend")
	}

	if !t.After(now) {
		return time.Time{}, nil
	}

	return t.UTC(), nil
}

// checkRecipients bounds the recipients and checks their form.
func checkRecipients(env *envelope, maxRecipients int) *setError {
	switch {
	case len(env.RcptTo) == 0:
		return &setError{Type: setNoRecipients}
	case len(env.RcptTo) > maxRecipients:
		return &setError{Type: setTooManyRecipients, MaxRecipients: maxRecipients}
	}

	var bad []string

	for _, rc := range env.RcptTo {
		if a, err := mail.ParseAddress(rc.Email); err != nil || a.Address != rc.Email {
			bad = append(bad, rc.Email)
		}
	}

	if len(bad) > 0 {
		return &setError{Type: setInvalidRecipients, InvalidRecipients: bad}
	}

	return nil
}

// checkFrom refuses a message whose From or Sender names an address the
// account may not use; the relay checks the envelope, not the header.
func (r *submissionRun) checkFrom(hdr textproto.Header) *setError {
	if !r.opts.CheckSender {
		return nil
	}

	list, err := parseAddresses(hdr.Get(fieldFrom))
	if err != nil || len(list) == 0 {
		return &setError{Type: setInvalidEmail, Properties: []string{propFrom}, Description: "the message has no usable From"}
	}

	if v := hdr.Get(fieldSender); v != "" {
		sender, err := parseAddresses(v)
		if err != nil {
			return &setError{Type: setInvalidEmail, Properties: []string{propSender}, Description: "the Sender field does not parse"}
		}

		list = append(list, sender...)
	}

	for _, a := range list {
		if !r.idents.allowed[strings.ToLower(a.Address)] {
			return &setError{Type: setForbiddenFrom, Description: "this account may not send as " + a.Address}
		}
	}

	return nil
}

// relay hands the message to the relay and turns its answer into a set
// error where it refused.
func (r *submissionRun) relay(ctx context.Context, env *envelope, msg []byte) (*outbound.Report, *setError) {
	to := make([]string, 0, len(env.RcptTo))
	for _, rc := range env.RcptTo {
		to = append(to, rc.Email)
	}

	var as *outbound.XClient
	if r.opts.XClient {
		as = &outbound.XClient{Login: r.sess.account}
		if r.opts.XClientAddr {
			as.Addr = r.sess.remote
		}
	}

	rep, err := r.opts.Relay.Submit(ctx, as, env.MailFrom.Email, to, msg)

	switch {
	case errors.Is(err, outbound.ErrNoXClient) || errors.Is(err, outbound.ErrXClientRefused):
		r.sess.log.Error("jmap: the relay does not let this host name users with XCLIENT; nothing can be sent", "err", err)

		return nil, &setError{Type: setForbiddenToSend, Description: "sending is not set up on this server"}
	case err != nil:
		r.sess.log.Warn("jmap: the relay could not be reached", "err", err)

		return nil, &setError{Type: setServerFail, Description: "the relay could not be reached; try again later"}
	}

	if rep.Accepted() {
		return rep, nil
	}

	return nil, relayRefusal(rep, r.opts.MaxSize)
}

// relayRefusal is the set error of a refused transaction.
func relayRefusal(rep *outbound.Report, maxSize int64) *setError {
	if rep.Temporary() {
		return &setError{Type: setServerFail, Description: "the relay refused for now: " + firstReply(rep)}
	}

	switch rep.Stage {
	case "MAIL":
		return &setError{Type: setForbiddenMailFrom, Description: rep.Final.String()}
	case "RCPT":
		senderRefused := true

		var bad []string

		for rcpt, rp := range rep.Refused {
			bad = append(bad, rcpt)
			senderRefused = senderRefused && strings.Contains(rp.Message, "Sender address rejected")
		}

		sort.Strings(bad)

		if senderRefused {
			return &setError{Type: setForbiddenMailFrom, Description: firstReply(rep)}
		}

		return &setError{Type: setInvalidRecipients, InvalidRecipients: bad, Description: firstReply(rep)}
	case "DATA":
		if rep.Final.Code == 552 {
			return &setError{Type: setTooLarge, MaxSize: maxSize, Description: rep.Final.String()}
		}
	}

	return &setError{Type: setForbiddenToSend, Description: rep.Final.String()}
}

// firstReply is one reply of a refused transaction, for a description.
func firstReply(rep *outbound.Report) string {
	if rep.Final.Code != 0 {
		return rep.Final.String()
	}

	keys := make([]string, 0, len(rep.Refused))
	for k := range rep.Refused {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return rep.Refused[keys[0]].String()
}

// stripBcc removes the Bcc fields from a message (RFC 8621 7.5), with
// their continuation lines, and leaves every other byte as it is.
func stripBcc(msg []byte) []byte {
	end := headerEnd(msg)
	if end < 0 {
		// No empty line: all header if it starts like one, as splitHeader
		// reads it, so that its Bcc goes as well.
		first, _, _ := bytes.Cut(msg, []byte("\n"))
		if !bytes.Contains(first, []byte(":")) {
			return msg
		}

		end = len(msg)
	}

	var out bytes.Buffer

	skipping := false

	for _, line := range bytes.SplitAfter(msg[:end], []byte("\n")) {
		folded := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')

		if !folded {
			name, _, _ := bytes.Cut(line, []byte(":"))
			skipping = strings.EqualFold(strings.TrimSpace(string(name)), fieldBcc)
		}

		if !skipping {
			out.Write(line)
		}
	}

	out.Write(msg[end:])

	return out.Bytes()
}

// update changes a submission: only undoStatus, and only from pending to
// canceled. A delayed submission is canceled in the queue by
// compare-and-swap, which fails once the worker has begun to send it
// (cannotUnsend); every other submission has been handed to the relay.
func (r *submissionRun) update(ref string, patch map[string]json.RawMessage) {
	id, _ := resolveCreationID(r.sess, ref)

	b, err := loadSubmissions(r.kv, r.sess.account, r.opts)
	if err != nil {
		r.out.NotUpdated[ref] = &setError{Type: setServerFail, Description: err.Error()}

		return
	}

	s := b.find(id)
	if s == nil {
		r.out.NotUpdated[ref] = &setError{Type: setNotFound}

		return
	}

	for k, v := range patch {
		var status string
		if k != propUndoStatus || json.Unmarshal(v, &status) != nil || status != undoCanceled {
			r.out.NotUpdated[ref] = invalidProperty(k, "only undoStatus can change, and only to canceled")

			return
		}
	}

	if len(patch) == 0 || s.UndoStatus == undoCanceled {
		r.out.Updated[id] = nil
		r.emailOf[id] = s.EmailID

		return
	}

	if serr := r.cancel(s); serr != nil {
		r.out.NotUpdated[ref] = serr

		return
	}

	r.out.Updated[id] = nil
	r.emailOf[id] = s.EmailID
}

// cancel cancels a pending delayed submission in the queue.
func (r *submissionRun) cancel(s *submission) *setError {
	d := r.opts.delayed()
	if d == nil || s.UndoStatus != undoPending {
		return &setError{Type: setCannotUnsend, Description: "the message has been handed to the relay"}
	}

	_, err := d.Queue.Cancel(r.sess.account, s.ID, "jmap")

	var np *sendqueue.NotPendingError

	switch {
	case err == nil:
		return nil
	case errors.As(err, &np), errors.Is(err, sendqueue.ErrNotFound):
		return &setError{Type: setCannotUnsend, Description: "the message is being sent or has been sent"}
	default:
		return &setError{Type: setServerFail, Description: err.Error()}
	}
}

// destroy removes the record of a submission; the message stays sent.
func (r *submissionRun) destroy(ref string) {
	id, _ := resolveCreationID(r.sess, ref)

	var emailID string

	err := updateSubmissions(r.kv, r.sess.account, r.opts, func(b *submissionBook) error {
		for i, s := range b.Subs {
			if s.ID == id {
				emailID = s.EmailID

				b.Subs = append(b.Subs[:i], b.Subs[i+1:]...)
				b.note(id, model.OpDestroyed)

				return nil
			}
		}

		return errNoSuchPush
	})

	switch {
	case errors.Is(err, errNoSuchPush):
		r.out.NotDestroyed[ref] = &setError{Type: setNotFound}
	case err != nil:
		r.out.NotDestroyed[ref] = &setError{Type: setServerFail, Description: err.Error()}
	default:
		r.out.Destroyed = append(r.out.Destroyed, id)
		r.emailOf[id] = emailID
	}
}

// onSuccess makes the implicit Email/set of onSuccessUpdateEmail and
// onSuccessDestroyEmail (RFC 8621 7.5), for the submissions that
// succeeded, and returns its response.
func (r *submissionRun) onSuccess(ctx context.Context, req *submissionSetRequest) []invocation {
	update := map[string]map[string]json.RawMessage{}
	destroy := []string{}

	for ref, patch := range req.OnSuccessUpdateEmail {
		id, _ := resolveCreationID(r.sess, ref)
		if emailID, ok := r.emailOf[id]; ok {
			update[emailID] = patch
		}
	}

	for _, ref := range req.OnSuccessDestroyEmail {
		id, _ := resolveCreationID(r.sess, ref)
		if emailID, ok := r.emailOf[id]; ok {
			destroy = append(destroy, emailID)
		}
	}

	if len(update) == 0 && len(destroy) == 0 {
		return nil
	}

	args, _ := json.Marshal(map[string]any{"accountId": accountID(r.sess.account), "update": update, "destroy": destroy})

	res, merr := emailSet(ctx, r.sess, args)
	if merr != nil {
		return []invocation{{Name: kwError, Args: mustRaw(merr)}}
	}

	return []invocation{{Name: methodEmailSet, Args: mustRaw(res)}}
}

// addressParser reads the address fields a submission and a creation
// check. Only the addresses count there, so a display name in a charset
// no decoder knows is left undecoded rather than making the whole field
// unreadable — net/mail refuses such a field, and a From with a KOI8-R or
// windows-1252 name would have been "no usable From".
var addressParser = &mail.AddressParser{WordDecoder: &mime.WordDecoder{
	CharsetReader: func(charset string, r io.Reader) (io.Reader, error) {
		if dec, err := mailmeta.WordDecoder.CharsetReader(charset, r); err == nil {
			return dec, nil
		}

		return r, nil
	},
}}

// parseAddresses parses an address list with addressParser.
func parseAddresses(s string) ([]*mail.Address, error) { return addressParser.ParseList(s) }
