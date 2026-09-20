// Package imapsieve executes bounded Sieve programs for committed IMAP events.
package imapsieve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/membudget"
	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// Event is a value snapshot; no connection state or writer escapes the IMAP lock.
type Event struct {
	LearningPrepared                                    bool
	LearningAction                                      string
	Operation, Actor, Owner, Mailbox, MailboxID, Source string
	SourceUses, DestinationUses                         []string
	Message                                             model.Meta
	BeforeFlags                                         []string
}

// Store opens the mailbox owner's account.
type Store interface {
	User(string) (model.User, error)
}

// Learner queues a message for a learning program.
type Learner interface {
	Enqueue(context.Context, Event, string, []byte) error
}

// Sender relays outbound mail (redirect, notify) produced by a script.
type Sender interface {
	Send(context.Context, string, []string, []byte) error
}

// Engine runs the IMAPSieve scripts for committed IMAP events with a
// bounded number of parallel runs.
type Engine struct {
	Sender        Sender
	Config        config.IMAPSieve
	Store         Store
	GlobalScripts string
	SieveConfig   map[string]string
	Log           *slog.Logger
	Learner       Learner
	// SharedPrefix is the client-visible prefix of other accounts'
	// mailboxes; a fileinto below it addresses somebody else's mailbox
	// and is refused. "" means clients have no shared namespace.
	SharedPrefix string
	Programs     *extprograms.Runner
	slots        chan struct{}
}

const (
	// opFlag is the IMAPSieve cause for flag changes.
	opFlag = "FLAG"
	// phaseName is the Sieve environment item and the metric label
	// for the processing phase.
	phaseName = "phase"
)

// New builds an Engine with c.MaxParallel (at least 1) run slots.
func New(c config.IMAPSieve, store Store, log *slog.Logger) *Engine {
	return &Engine{Config: c, Store: store, Log: log, slots: make(chan struct{}, max(1, int(c.MaxParallel)))}
}

// Handle processes one event when IMAPSieve is enabled; it waits for a
// free slot and gives up when ctx ends first.
func (e *Engine) Handle(ctx context.Context, ev Event) {
	if !e.Config.Enabled {
		return
	}

	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return
	}

	e.handle(ctx, ev)
}

func (e *Engine) handle(ctx context.Context, ev Event) {
	u, err := e.Store.User(ev.Owner)
	if err != nil {
		e.failed("open", err)
		return
	}
	defer u.Close()

	current, ok := currentMessage(u, ev)
	if !ok {
		return
	}

	action := e.learningAction(ev)

	release, err := membudget.Acquire(ev.Message.Size * 10)
	if err != nil {
		e.failed("memory", err)
		return
	}
	defer release()

	raw := e.enqueueLearning(ctx, u, &ev, action)
	// A later client mutation wins for script effects; it does not erase a learning event.
	if current.ModSeq != ev.Message.ModSeq {
		return
	}

	script, err := e.script(u, ev)
	if err != nil {
		e.failed("script_lookup", err)
		return
	}

	if script == "" {
		return
	}

	if raw == nil {
		raw, err = u.Read(&ev.Message)
		if err != nil {
			e.failed("read", err)
			return
		}
	}

	e.runScript(ctx, u, ev, script, raw)
}

// currentMessage returns the stored state of the event's message when
// the mailbox and the message are still the ones the event names.
func currentMessage(u model.User, ev Event) (model.Meta, bool) {
	info, err := u.Info(ev.Mailbox)
	if err != nil || info.ID != ev.MailboxID {
		return model.Meta{}, false
	}

	current, err := u.GetMessages(info.ID, []uint32{ev.Message.UID})
	if err != nil || len(current) != 1 || current[0].GUID != ev.Message.GUID {
		return model.Meta{}, false
	}

	return current[0], true
}

// enqueueLearning hands the message to the learner when a learning rule
// matched and the event was not prepared already. It returns the message
// it read (nil when nothing was read).
func (e *Engine) enqueueLearning(ctx context.Context, u model.User, ev *Event, action string) []byte {
	if action == "" || e.Learner == nil || ev.LearningPrepared {
		return nil
	}

	raw, err := u.Read(&ev.Message)
	if err != nil {
		e.failed("learning_read", err)
	} else if err = e.Learner.Enqueue(ctx, *ev, action, raw); err != nil {
		e.failed("learning_enqueue", err)
	}

	return raw
}

// runScript executes script for the event and applies its result; a
// flag change caused by an APPEND/COPY script is handled as a follow-up
// FLAG event.
func (e *Engine) runScript(ctx context.Context, u model.User, ev Event, script string, raw []byte) {
	runctx, cancel := context.WithTimeout(ctx, time.Duration(e.Config.TimeoutMS)*time.Millisecond)
	defer cancel()

	cause := ev.Operation
	if cause == "MOVE" {
		cause = "COPY"
	}

	env := scriptEnvironment(ev, cause)
	hooks := sieve.Hooks{Config: e.SieveConfig, Include: sieve.IncludeLoader(u, e.GlobalScripts), IMAP: &sieve.IMAPContext{Environment: env, Flags: ev.Message.Flags}, MailboxExists: func(name string) (bool, error) { _, err := u.Lookup(name); return err == nil, nil }}

	pctx := extprograms.Context{Account: ev.Owner, Mailbox: ev.Mailbox, Cause: cause, ChangedFlags: env["imap.changedflags"]}
	if ev.Actor == ev.Owner && e.Programs != nil && e.Programs.Enabled() {
		hooks.Program = e.Programs.SieveHook(pctx)
	}

	result, err := sieve.RunWithHooks(runctx, script, "", "", ev.Actor, raw, hooks)
	if err != nil {
		e.failed("script", err)
		return
	}

	if err := runctx.Err(); err != nil {
		e.failed("timeout", err)
		return
	}

	updated, err := e.apply(runctx, u, ev, result, raw)
	if err != nil {
		e.failed("apply", err)
		return
	}
	// RFC 6785: APPEND/COPY scripts can cause a FLAG event, FLAG scripts cannot recurse.
	if cause != opFlag && updated != nil && len(changedFlags(ev.Message.Flags, updated.Flags)) > 0 && !model.HasFlag(updated.Flags, `\Deleted`) {
		next := ev
		next.Operation = opFlag
		next.BeforeFlags = ev.Message.Flags
		next.Message = *updated
		// Reuse the slot for the bounded follow-up (also works with max_parallel=1).
		e.handle(ctx, next)
	}
}

// scriptEnvironment is the RFC 6785 environment for the event.
func scriptEnvironment(ev Event, cause string) map[string]string {
	env := map[string]string{"location": "MS", phaseName: "post", "imap.cause": cause, "imap.mailbox": ev.Mailbox, "imap.user": ev.Actor}
	env["imap.email"] = ""

	env["imap.changedflags"] = ""
	if strings.Contains(ev.Actor, "@") {
		env["imap.email"] = ev.Actor
	}

	if cause == opFlag {
		env["imap.changedflags"] = strings.Join(changedFlags(ev.BeforeFlags, ev.Message.Flags), " ")
	}

	return env
}

// failed counts a failure of phase and logs it without the error text.
func (e *Engine) failed(phase string, _ error) {
	scriptResults.WithLabelValues(phase).Inc()
	e.Log.Error("imapsieve processing failed", "phase", phase) /* interpreter errors can contain mail data */
}
func (e *Engine) script(u model.User, ev Event) (string, error) {
	if !e.Config.UserScripts {
		return "", nil
	}

	annotations, err := u.GetMetadata(ev.Mailbox, "")
	if err != nil {
		return "", err
	}

	name, exists := annotations["/shared/imapsieve/script"]
	if !exists {
		server, err := e.Store.User("~server")
		if err != nil {
			return "", err
		}
		defer server.Close()

		a, err := server.GetMetadata("", "")
		if err != nil {
			return "", err
		}

		name = a["/shared/imapsieve/script"]
	}

	if name == "" {
		return "", nil
	}

	s, err := u.GetScript(name)
	if errors.Is(err, model.ErrNoScript) {
		return "", nil
	}

	return s, err
}
func rights(u model.User, ev Event, mailbox string) (string, error) {
	if ev.Actor == ev.Owner {
		return model.AllRights, nil
	}

	acl, err := u.GetACL(mailbox)

	return acl[ev.Actor] + acl["anyone"], err
}
func (e *Engine) apply(ctx context.Context, u model.User, ev Event, d *sieve.Delivery, raw []byte) (*model.Meta, error) {
	if ev.Actor != ev.Owner && len(d.Pipes) > 0 {
		return nil, fmt.Errorf("external programs require mailbox owner")
	}

	desired := scriptFlags(d)

	r, err := rights(u, ev, ev.Mailbox)
	if err != nil {
		return nil, err
	}

	if !hasFlagRights(r, changedFlags(ev.Message.Flags, desired)) {
		return nil, fmt.Errorf("insufficient flag rights")
	}

	targets, err := e.fileIntoTargets(u, ev, d)
	if err != nil {
		return nil, err
	}

	rollback, err := e.deliverCopies(u, ev, d, targets, raw)
	if err != nil {
		return nil, err
	}
	// Validate and persist local destinations before any external side effect.
	// A relay failure leaves the original intact and durable local copies available.
	if err := e.sideEffects(ctx, ev, d); err != nil {
		return nil, err
	}

	if d.IMAPKeep {
		desired = append([]string{}, d.IMAPFlags...)
	}

	res, err := u.SetFlags(ev.Mailbox, []uint32{ev.Message.UID}, ev.Message.ModSeq, func(_ []string) []string { return desired })
	if err != nil {
		// Ambiguous write failure: keep durable copies, because flags may have committed.
		return nil, err
	}

	if len(res.Modified) > 0 {
		rollback()
		return nil, fmt.Errorf("message changed concurrently")
	}

	if len(res.Updated) == 1 {
		return &res.Updated[0], nil
	}

	return nil, nil
}

// scriptFlags are the flags the script leaves on the message; without
// keep the message is marked \Deleted.
func scriptFlags(d *sieve.Delivery) []string {
	desired := append([]string{}, d.IMAPFlags...)
	if !d.IMAPKeep && !model.HasFlag(desired, `\Deleted`) {
		desired = append(desired, `\deleted`)
	}

	return desired
}

// sideEffects sends redirects and notifications, then runs the pipes.
func (e *Engine) sideEffects(ctx context.Context, ev Event, d *sieve.Delivery) error {
	if err := e.send(ctx, ev, d); err != nil {
		return err
	}

	var hook = e.Programs.SieveHook(extprograms.Context{Account: ev.Owner, Mailbox: ev.Mailbox, Cause: ev.Operation})

	return d.RunPipes(ctx, hook)
}

// flagRight is the ACL right needed to set or clear flag.
func flagRight(flag string) byte {
	needed := byte('w')
	if strings.EqualFold(flag, `\Seen`) {
		needed = 's'
	}

	if strings.EqualFold(flag, `\Deleted`) {
		needed = 't'
	}

	return needed
}

// hasFlagRights reports whether rights r allow changing all flags.
func hasFlagRights(r string, flags []string) bool {
	for _, flag := range flags {
		if !model.HasRight(r, flagRight(flag)) {
			return false
		}
	}

	return true
}

// fileIntoTargets resolves and authorizes the fileinto destinations.
func (e *Engine) fileIntoTargets(u model.User, ev Event, d *sieve.Delivery) ([]string, error) {
	targets := make([]string, len(d.FileInto))
	for i, f := range d.FileInto {
		name, err := e.fileIntoTarget(u, ev, f)
		if err != nil {
			return nil, err
		}

		targets[i] = name
	}

	return targets, nil
}

// fileIntoTarget resolves one fileinto (special-use lookup included) to
// an existing mailbox of the owner the actor may insert into.
func (e *Engine) fileIntoTarget(u model.User, ev Event, f sieve.FileInto) (string, error) {
	name, err := specialUseTarget(u, f)
	if err != nil {
		return "", err
	}

	if e.SharedPrefix != "" && strings.HasPrefix(name, e.SharedPrefix) {
		return "", fmt.Errorf("cross-owner fileinto unavailable")
	}

	if _, err := u.Lookup(name); err != nil {
		return "", err
	} // no implicit creation in IMAP context

	r, err := rights(u, ev, name)
	if err != nil || !model.HasRight(r, 'i') {
		return "", fmt.Errorf("insufficient fileinto rights")
	}

	if !hasFlagRights(r, f.Flags) {
		return "", fmt.Errorf("insufficient target flag rights")
	}

	return name, nil
}

// specialUseTarget returns the mailbox carrying f's special-use flag, or
// f's mailbox when there is none or no special use was requested.
func specialUseTarget(u model.User, f sieve.FileInto) (string, error) {
	name := f.Mailbox
	if f.SpecialUse == "" {
		return name, nil
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return "", err
	}

	for _, b := range boxes {
		if model.HasFlag(b.SpecialUse, f.SpecialUse) {
			return b.Name, nil
		}
	}

	return name, nil
}

// deliverCopies stores the fileinto copies under one delivery intent and
// makes them durable. The returned rollback expunges the copies and ends
// the intent; on error it has run already.
func (e *Engine) deliverCopies(u model.User, ev Event, d *sieve.Delivery, targets []string, raw []byte) (func(), error) {
	intent, err := u.BeginDelivery()
	if err != nil {
		return nil, err
	}

	type saved struct {
		name string
		m    model.Meta
	}

	var copies []saved

	rollback := func() {
		for _, c := range copies {
			if err := u.Expunge(c.name, []model.Meta{c.m}); err != nil {
				e.failed("rollback", err)
				return
			}
		}

		if err := u.EndDelivery(intent); err != nil {
			e.failed("rollback_intent", err)
		}
	}

	for i, name := range targets {
		guid := model.NewGUID()
		if err := u.RecordDelivery(intent, name, guid); err != nil {
			rollback()
			return nil, err
		}

		copyRaw := d.FileInto[i].Raw
		if copyRaw == nil {
			copyRaw = raw
		}

		m, err := u.DeliverGUID(name, guid, copyRaw, d.FileInto[i].Flags, ev.Message.InternalDate)
		if err != nil {
			rollback()
			return nil, err
		}

		copies = append(copies, saved{name, *m})
	}
	// Make destination copies durable before the original can become Deleted.
	// A crash may leave an extra copy, but the scrubber cannot remove the sole survivor.
	if err := u.EndDelivery(intent); err != nil {
		rollback()
		return nil, err
	}

	return rollback, nil
}

func changedFlags(a, b []string) []string {
	out := []string{}

	for _, f := range a {
		if !model.HasFlag(b, f) {
			out = append(out, f)
		}
	}

	for _, f := range b {
		if !model.HasFlag(a, f) {
			out = append(out, f)
		}
	}

	slices.Sort(out)

	return out
}
func (e *Engine) learningAction(ev Event) string {
	if !e.Config.Learning.Enabled || ev.Actor != ev.Owner {
		return ""
	}

	for _, r := range e.Config.Learning.Rules {
		if ruleMatches(r, ev) {
			return r.Program
		}
	}

	return ""
}

// ruleMatches reports whether learning rule r applies to ev.
func ruleMatches(r config.LearningRule, ev Event) bool {
	if !slices.Contains(r.Operations, ev.Operation) {
		return false
	}

	if r.SourceSpecialUse != "" && !model.HasFlag(ev.SourceUses, r.SourceSpecialUse) {
		return false
	}

	if r.SourceNotSpecialUse != "" && model.HasFlag(ev.SourceUses, r.SourceNotSpecialUse) {
		return false
	}

	if r.DestinationSpecialUse != "" && !model.HasFlag(ev.DestinationUses, r.DestinationSpecialUse) {
		return false
	}

	for _, f := range r.DestinationNotSpecialUse {
		if model.HasFlag(ev.DestinationUses, f) {
			return false
		}
	}

	return true
}

// LearningAction returns the learning program the first matching rule
// names for ev, or "" when no rule applies.
func (e *Engine) LearningAction(ev Event) string { return e.learningAction(ev) }
