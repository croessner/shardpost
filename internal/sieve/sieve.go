// Package sieve filters incoming mail with the user's active Sieve script
// (RFC 5228 with fileinto, envelope, imap4flags, variables via
// foxcpp/go-sieve) and serves ManageSieve (RFC 5804) for editing scripts.
package sieve

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/textproto"
	"strings"
	"time"

	gosieve "github.com/foxcpp/go-sieve"
	"github.com/foxcpp/go-sieve/interp"
)

const (
	// settingYes is the true value of a Sieve setting.
	settingYes = "yes"
	// valueNo is the false value of a setting or an Auto-Submitted field.
	valueNo = "no"
	// inboxName is the mailbox of an implicit keep.
	inboxName = "INBOX"
)

// IMAPRedirect is a redirect from an IMAPSieve script with the message
// as it was when the action ran.
type IMAPRedirect struct {
	Address string
	Raw     []byte
}

// Delivery is the outcome of running a script over a message.
type Delivery struct {
	IMAPRedirects []IMAPRedirect
	IMAPKeep      bool
	IMAPFlags     []string
	// Mailboxes to file the message into with their flags. Empty means
	// the message was discarded.
	FileInto []FileInto
	// Raw is the message to store: the original, or the edited copy when
	// the script used addheader/deleteheader (RFC 5293).
	Raw []byte
	// Redirects are addresses to forward a copy to.
	Redirects []string
	// Rejected is non-empty for a reject or ereject with this reason;
	// EReject tells which (RFC 5429: ereject refuses in the protocol,
	// reject may send a rejection message instead).
	Rejected string
	EReject  bool
	// Vacation is the requested auto-reply, if any.
	Vacation *Vacation
	// Tracked are the duplicate-test keys the script consulted; the
	// caller records them once the delivery is final (RFC 7352 3: a
	// failed attempt must not turn the retry into a duplicate).
	Tracked []Tracked
	// Notifications are the notify actions (RFC 5435, mailto).
	Notifications []Notification
	// Pipes are the pipe actions (vnd.dovecot.pipe): the caller runs them
	// once the delivery is durable.
	Pipes []Pipe
	// Scripts names the scripts of a sequence that ran, in order (see
	// RunSequence); empty for a single script.
	Scripts  []string
	pipeKeep bool
	imap     bool
}

// Pipe is one pipe action.
type Pipe struct {
	Raw     []byte
	Copy    bool
	Program string
	Args    []string
	Try     bool // failure is not an error
}

// Notification is one notify action with its parsed mailto URI.
type Notification struct {
	Raw         []byte
	From        string // :from (validated) or ""
	Importance  string // "1", "2", "3"
	Message     string
	To, Cc, Bcc []string
	Subject     string
	Body        string
	Headers     [][2]string
	// Fcc is where a copy of the sent notification is stored (RFC 8580).
	Fcc *Fcc
}

// Fcc is the :fcc argument of vacation or notify (RFC 8580): the
// generated message is also stored in this mailbox of the user.
type Fcc struct {
	Mailbox string
	// Create creates a missing mailbox (:create); without it a missing
	// mailbox means no copy.
	Create bool
	// Flags are the flags of the copy (lower case, as for fileinto).
	Flags []string
	// SpecialUse is :specialuse: the mailbox with this use wins.
	SpecialUse string
}

// fccOf converts the interpreter's :fcc target.
func fccOf(t *interp.FccTarget) *Fcc {
	if t == nil {
		return nil
	}

	return &Fcc{Mailbox: t.Mailbox, Create: t.Create, Flags: lower(t.Flags), SpecialUse: t.SpecialUse}
}

// Tracked is one duplicate-test key with its tracking period.
type Tracked struct {
	Key string
	TTL time.Duration
}

// FileInto is one delivery target.
type FileInto struct {
	Raw        []byte
	Mailbox    string
	Flags      []string
	Create     bool   // :create (RFC 5490); the store always creates, kept for callers
	SpecialUse string // :specialuse (RFC 8579): a mailbox with this use wins over Mailbox
	// Keep marks the keep (implicit or explicit) as opposed to a
	// fileinto "INBOX": in a sequence of scripts the keep is handed to
	// the next script instead of being carried out (see RunSequence).
	Keep bool
	// scriptFlags are the flags of a keep as the script wrote them
	// (Flags is lower-case): what the next script of a sequence starts
	// with, so that its removeflag finds what an earlier addflag set.
	scriptFlags []string
}

// IMAPContext marks an IMAPSieve run: the RFC 6785 environment and the
// message's current flags.
type IMAPContext struct {
	Environment map[string]string
	Flags       []string
}

// Hooks give the script access to the store (all optional).
type Hooks struct {
	IMAP *IMAPContext
	// MailboxExists backs the mailboxexists test.
	MailboxExists func(name string) (bool, error)
	// Duplicate backs the duplicate test: was key seen (a query, nothing
	// is recorded; see Delivery.Tracked).
	Duplicate func(key string) (bool, error)
	// Metadata backs mboxmetadata/servermetadata (mailbox "" = server).
	Metadata func(mailbox, annotation string) (string, bool, error)
	// SpecialUseExists backs specialuse_exists (mailbox "" = any).
	SpecialUseExists func(mailbox string, uses []string) (bool, error)
	// Include loads scripts for the include extension (ok false: no such
	// script). nil disables include.
	Include func(name string, global bool) ([]byte, bool, error)
	// Config holds implementation settings (Pigeonhole key names, see
	// DefaultConfig).
	Config map[string]string
	// Flags is what the script's flag variable (imap4flags) starts with
	// in a delivery: the flags of the keep a previous script of the
	// sequence handed on (see RunSequence). An IMAPSieve run starts with
	// the message's flags instead (IMAP).
	Flags []string
	// Program runs external programs (execute/pipe/filter); nil disables
	// the extensions at runtime (they still compile when configured).
	Program interp.ProgramRunner
}

// DefaultConfig are the settings used when the server sets none:
// spamtest reads rspamd's X-Spam-Score (max 15), Return-Path and
// Delivered-To stay protected against editheader, redirects keep the
// original sender.
func DefaultConfig() map[string]string {
	return map[string]string{
		"sieve_spamtest_status_header":                         "X-Spam-Score:(-?[0-9]+\\.?[0-9]*)",
		"sieve_spamtest_status_type":                           "score",
		"sieve_spamtest_score_max_value":                       "15",
		"sieve_editheader_header":                              "received auto-submitted return-path delivered-to",
		"sieve_editheader_header/received/forbid_add":          settingYes,
		"sieve_editheader_header/received/forbid_delete":       settingYes,
		"sieve_editheader_header/auto-submitted/forbid_add":    settingYes,
		"sieve_editheader_header/auto-submitted/forbid_delete": settingYes,
		"sieve_editheader_header/return-path/forbid_add":       settingYes,
		"sieve_editheader_header/return-path/forbid_delete":    settingYes,
		"sieve_editheader_header/delivered-to/forbid_add":      settingYes,
		"sieve_editheader_header/delivered-to/forbid_delete":   settingYes,
	}
}

// ParseSettings reads "key=value;key=value" (an environment variable)
// on top of DefaultConfig.
func ParseSettings(s string) map[string]string {
	cfg := DefaultConfig()

	for _, kv := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}

		cfg[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}

	return cfg
}

func options(h Hooks) gosieve.Options {
	opts := gosieve.DefaultOptions()
	opts.Interp.MaxRedirects = MaxRedirects

	opts.Interp.Config = h.Config
	if opts.Interp.Config == nil {
		opts.Interp.Config = DefaultConfig()
	}

	if h.IMAP != nil {
		cfg := map[string]string{}
		for k, v := range opts.Interp.Config {
			cfg[k] = v
		}

		cfg["imapsieve_enabled"] = settingYes
		cfg["imapsieve_context"] = settingYes
		opts.Interp.MaxActions = 8
		opts.Interp.Config = cfg
	}

	if h.Include != nil {
		opts.Interp.IncludeLoader = h.Include
	}

	return opts
}

// CompileWith validates a script with the include loader of the user
// (PUTSCRIPT/CHECKSCRIPT resolve includes against the script store).
func CompileWith(text string, h Hooks) error {
	_, err := gosieve.Load(strings.NewReader(text), options(h))
	return err
}

// Compile validates a script (used by ManageSieve PUTSCRIPT and CHECKSCRIPT).
func Compile(text string) error {
	return CompileWith(text, Hooks{})
}

// Run executes the script against a message. An empty script keeps the
// message in INBOX.
func Run(ctx context.Context, script string, from, to, user string, raw []byte) (*Delivery, error) {
	return RunWithHooks(ctx, script, from, to, user, raw, Hooks{})
}

// RunWithHooks is Run with store access for mailboxexists and duplicate.
func RunWithHooks(ctx context.Context, script string, from, to, user string, raw []byte, h Hooks) (*Delivery, error) {
	d := &Delivery{Raw: raw}

	if strings.TrimSpace(script) == "" {
		return emptyScriptDelivery(d, h), nil // in a delivery: a keep with h.Flags
	}

	opts := options(h)

	s, err := gosieve.Load(strings.NewReader(script), opts)
	if err != nil {
		return nil, fmt.Errorf("sieve: %w", err)
	}

	hdr, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	msg := &message{raw: raw, hdr: hdr}
	data := gosieve.NewRuntimeData(s, interp.DummyPolicy{}, envelope{from: from, to: to, user: user}, msg)

	data.Env = interp.MapEnv{"imap.user": "", "imap.email": "", "imap.cause": "", "imap.mailbox": "", "imap.changedflags": ""}
	flags := h.Flags

	if h.IMAP != nil {
		data.Env = interp.MapEnv(h.IMAP.Environment)
		flags = h.IMAP.Flags
	}

	data.Flags = append([]string{}, flags...)

	installHooks(data, h, d)

	actionRaw := snapshotActions(data, msg)
	if err := s.Execute(ctx, data); err != nil {
		return nil, fmt.Errorf("sieve: %w", err)
	}

	d.Raw = msg.Raw()
	d.pipeKeep = data.KeepWithoutPipes()

	d.imap = h.IMAP != nil
	if h.IMAP != nil {
		d.IMAPFlags = lower(data.Flags)
	}

	for i, a := range data.AppliedActions {
		var snapshot []byte
		if i < len(*actionRaw) {
			snapshot = (*actionRaw)[i]
		}

		d.addAction(a, snapshot, h.IMAP != nil, opts.Interp.Config)
	}

	return d, nil
}

// emptyScriptDelivery is the result of an empty script: keep the
// message (IMAP: with its flags unchanged; delivery: in INBOX).
func emptyScriptDelivery(d *Delivery, h Hooks) *Delivery {
	if h.IMAP != nil {
		d.IMAPKeep = true

		d.IMAPFlags = append([]string{}, h.IMAP.Flags...)

		return d
	}

	d.FileInto = []FileInto{{Mailbox: inboxName, Keep: true, Flags: lower(h.Flags), scriptFlags: h.Flags}}

	return d
}

// installHooks connects the optional store hooks to the runtime data.
// Duplicate answers are cached per run and the consulted keys are
// recorded in d.Tracked.
func installHooks(data *interp.RuntimeData, h Hooks, d *Delivery) {
	if h.MailboxExists != nil {
		data.MailboxExists = func(_ context.Context, name string) (bool, error) { return h.MailboxExists(name) }
	}

	data.Program = h.Program
	if h.Metadata != nil {
		data.Metadata = func(_ context.Context, mailbox, annotation string) (string, bool, error) {
			return h.Metadata(mailbox, annotation)
		}
	}

	if h.SpecialUseExists != nil {
		data.SpecialUseExists = func(_ context.Context, mailbox string, uses []string) (bool, error) {
			return h.SpecialUseExists(mailbox, uses)
		}
	}

	if h.Duplicate != nil {
		seen := map[string]bool{} // one answer per key within a run
		data.Duplicate = func(_ context.Context, key string, seconds int) (bool, error) {
			if v, ok := seen[key]; ok {
				return v, nil
			}

			already, err := h.Duplicate(key)
			if err != nil {
				return false, err
			}

			seen[key] = already
			d.Tracked = append(d.Tracked, Tracked{Key: key, TTL: time.Duration(seconds) * time.Second})

			return already, nil
		}
	}
}

// snapshotActions wraps data.OnAction to record the message as it was
// when each applied action ran (editheader can change it later).
func snapshotActions(data *interp.RuntimeData, msg *message) *[][]byte {
	var actionRaw [][]byte

	originalAction := data.OnAction

	data.OnAction = func(ctx context.Context, a interp.AppliedAction, rd *interp.RuntimeData) error {
		before := len(rd.AppliedActions)
		if err := originalAction(ctx, a, rd); err != nil {
			return err
		}

		if len(rd.AppliedActions) > before {
			actionRaw = append(actionRaw, msg.Raw())
		}

		return nil
	}

	return &actionRaw
}

// addAction records one applied action with the message snapshot taken
// when it ran. imap selects the IMAPSieve meaning of keep and redirect;
// cfg is the settings the vacation reply uses. Discard records nothing.
func (d *Delivery) addAction(a interp.AppliedAction, snapshot []byte, imap bool, cfg map[string]string) {
	switch act := a.(type) {
	case interp.ActionKeep:
		if imap {
			d.IMAPKeep = true
			return
		}

		d.FileInto = append(d.FileInto, FileInto{Raw: snapshot, Mailbox: inboxName, Flags: lower(act.Flags), Keep: true, scriptFlags: act.Flags})
	case interp.ActionFileInto:
		d.FileInto = append(d.FileInto, FileInto{Raw: snapshot, Mailbox: act.Mailbox, Flags: lower(act.Flags), Create: act.Create, SpecialUse: act.SpecialUse})
	case interp.ActionPipe:
		d.Pipes = append(d.Pipes, Pipe{Raw: snapshot, Copy: act.Copy, Program: act.Program, Args: act.Args, Try: act.Try})
	case interp.ActionNotify:
		d.addNotification(act, snapshot)
	case interp.ActionRedirect:
		if imap {
			d.IMAPRedirects = append(d.IMAPRedirects, IMAPRedirect{Address: act.Address, Raw: snapshot})
		}

		d.Redirects = append(d.Redirects, act.Address)
	case interp.ActionReject:
		d.Rejected, d.EReject = act.Reason, false
	case interp.ActionEReject:
		d.Rejected, d.EReject = act.Reason, true
	case interp.ActionVacation:
		d.Vacation = &Vacation{Reason: act.Reason, Subject: act.Subject, From: act.From,
			Addresses: act.Addresses, Handle: act.Handle, Mime: act.Mime, Seconds: act.Seconds, PeriodSet: act.PeriodSet,
			Fcc: fccOf(act.Fcc), Config: cfg}
	}
}

// addNotification records a notify action with a mailto method.
func (d *Delivery) addNotification(act interp.ActionNotify, snapshot []byte) {
	if act.Mailto == nil {
		return
	}

	d.Notifications = append(d.Notifications, Notification{Raw: snapshot, From: act.From, Importance: act.Importance, Message: act.Message,
		To: act.Mailto.To, Cc: act.Mailto.Cc, Bcc: act.Mailto.Bcc, Subject: act.Mailto.Subject, Body: act.Mailto.Body, Headers: act.Mailto.Headers,
		Fcc: fccOf(act.Fcc)})
}

func lower(f interp.Flags) []string {
	out := make([]string, 0, len(f))
	for _, x := range f {
		out = append(out, strings.ToLower(x))
	}

	return out
}

type envelope struct{ from, to, user string }

func (e envelope) EnvelopeFrom() string { return e.from }
func (e envelope) EnvelopeTo() string   { return e.to }
func (e envelope) AuthUsername() string { return e.user }

type message struct {
	raw []byte
	hdr textproto.MIMEHeader
}

// Raw implements interp.RawMessage (the MIME extensions parse the message).
func (m *message) Raw() []byte { return m.raw }

// Replace swaps the message (filter program output).
func (m *message) Replace(raw []byte) {
	m.raw = raw
	m.hdr, _ = textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
}

func (m *message) HeaderGet(key string) ([]string, error) {
	return m.hdr.Values(key), nil
}

// ApplyHeaderEdit lets later tests in the script see addheader and
// deleteheader results.
func (m *message) ApplyHeaderEdit(e interp.HeaderEdit) {
	m.raw = applyHeaderEdits(m.raw, []interp.HeaderEdit{e})

	key := textproto.CanonicalMIMEHeaderKey(e.Name)
	if e.Add {
		if e.Last {
			m.hdr[key] = append(m.hdr[key], e.Value)
		} else {
			m.hdr[key] = append([]string{e.Value}, m.hdr[key]...)
		}

		return
	}

	m.hdr[key] = deleteValues(m.hdr[key], e)
	if len(m.hdr[key]) == 0 {
		delete(m.hdr, key)
	}
}

// deleteValues applies a deleteheader edit to the values of one field.
func deleteValues(vals []string, e interp.HeaderEdit) []string {
	if e.Index > 0 {
		i := e.Index - 1
		if e.Last {
			i = len(vals) - e.Index
		}

		if i < 0 || i >= len(vals) || (e.Match != nil && !e.Match(vals[i])) {
			return vals
		}

		return append(append([]string{}, vals[:i]...), vals[i+1:]...)
	}

	out := vals[:0:0]
	for _, v := range vals {
		if e.Match != nil && !e.Match(v) {
			out = append(out, v)
		}
	}

	return out
}

// applyHeaderEdits rewrites the header block of raw (RFC 5293). Folded
// lines belong to their field. New fields go to the top (or the end with
// :last); deleted ones vanish with their continuation lines.
func applyHeaderEdits(raw []byte, edits []interp.HeaderEdit) []byte {
	return interp.ApplyHeaderEdits(raw, edits)
}
func (m *message) MessageSize() int    { return len(m.raw) }
func (m *message) ContentType() string { return m.hdr.Get("Content-Type") }
func (m *message) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.raw)), nil
}
func (m *message) BodyRaw(context.Context) (io.Reader, error) {
	if i := bytes.Index(m.raw, []byte("\r\n\r\n")); i >= 0 {
		return bytes.NewReader(m.raw[i+4:]), nil
	}

	if i := bytes.Index(m.raw, []byte("\n\n")); i >= 0 {
		return bytes.NewReader(m.raw[i+2:]), nil
	}

	return bytes.NewReader(nil), nil
}

// RunPipes commits deferred pipe actions before the original can be discarded.
// A failed :try acts as if that pipe did not occur. Other failures let the
// caller retain the original (IMAP) or fall back to INBOX (LMTP).
func (d *Delivery) RunPipes(ctx context.Context, runner interp.ProgramRunner) error {
	cancelled := false

	for _, p := range d.Pipes {
		if !runPipe(ctx, runner, p) {
			if !p.Try {
				return fmt.Errorf("pipe action failed")
			}

			continue
		}

		if !p.Copy {
			cancelled = true
		}
	}

	if len(d.Pipes) > 0 && !cancelled && d.pipeKeep {
		d.keepAfterPipes()
	}

	return nil
}

// runPipe runs one pipe action and reports whether it succeeded (the
// program ran and exited 0). A nil runner fails every pipe.
func runPipe(ctx context.Context, runner interp.ProgramRunner, p Pipe) bool {
	var (
		res interp.ProgramResult
		ok  bool
		err error
	)
	if runner != nil {
		res, ok, err = runner(ctx, interp.ProgramPipe, p.Program, p.Args, p.Raw)
	}

	return err == nil && ok && res.ExitCode == 0
}

// keepAfterPipes restores the implicit keep the pipes left in place:
// IMAP keeps the message, delivery files it into INBOX once.
func (d *Delivery) keepAfterPipes() {
	if d.imap {
		d.IMAPKeep = true
		return
	}

	for _, f := range d.FileInto {
		if f.Mailbox == inboxName {
			return
		}
	}

	d.FileInto = append(d.FileInto, FileInto{Mailbox: inboxName, Raw: d.Raw, Keep: true})
}
