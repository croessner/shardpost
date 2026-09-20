package imapproto

import (
	"context"
	"shardpost/internal/imapsieve"
	"shardpost/internal/membudget"
	"shardpost/internal/model"
)

// sieveEvents runs the IMAPSIEVE scripts for the messages an operation
// (APPEND, COPY, MOVE, FLAG) placed into or changed in target t; before
// holds the messages' previous state. The command lock is released while
// the scripts run, then pending changes are polled.
func (c *conn) sieveEvents(operation string, t *target, metas []model.Meta, before []model.Meta) {
	if c.s.o.IMAPSieve == nil || !c.s.o.IMAPSieve.Config.Enabled || len(metas) == 0 {
		return
	}

	info, err := t.u.Info(t.name)
	if err != nil {
		return
	}

	owner := t.owner
	if owner == "" {
		owner = c.name
	}

	events := make([]imapsieve.Event, 0, len(metas))
	for _, m := range metas {
		events = append(events, c.sieveEvent(operation, t, owner, info, m, before))
	}
	c.cmdMu.Unlock()
	func() {
		defer c.cmdMu.Lock()
		for _, e := range events {
			c.s.o.IMAPSieve.Handle(context.Background(), e)
		}
	}()
	// Refresh through the usual notification path, preserving sequence numbers.
	if c.mbox != nil {
		_ = c.poll(false)
	}
}

// sieveEvent builds the IMAPSIEVE event of one message m.
func (c *conn) sieveEvent(operation string, t *target, owner string, info *model.MailboxInfo, m model.Meta, before []model.Meta) imapsieve.Event {
	_, durable := t.u.(interface {
		SetLearningHook(func(model.Mailbox, model.Meta) (string, error))
	})

	e := imapsieve.Event{LearningPrepared: durable && (operation == kwCopy || operation == kwMove), Operation: operation, Actor: c.name, Owner: owner, Mailbox: t.name, MailboxID: info.ID, Message: m, DestinationUses: append([]string{}, info.SpecialUse...)}
	if operation == kwCopy || operation == kwMove {
		e.Source = c.mbox.Name
		e.SourceUses = append([]string{}, c.mbox.SpecialUse...)
	}

	for _, old := range before {
		if old.UID == m.UID {
			e.BeforeFlags = append([]string{}, old.Flags...)
			break
		}
	}

	return e
}

func (c *conn) selectedTarget() *target {
	return &target{u: c.mu, owner: c.mowner, public: c.mpublic, prefix: c.mprefix, name: c.mbox.Name}
}

// prepareLearning installs a transaction hook only for owner-local COPY/MOVE.
func (c *conn) prepareLearning(operation string, t *target) func() {
	hook, ok := t.u.(interface {
		SetLearningHook(func(model.Mailbox, model.Meta) (string, error))
	})
	if !ok || c.s.o.IMAPSieve == nil {
		return func() {}
	}

	engine := c.s.o.IMAPSieve

	worker, ok := engine.Learner.(*imapsieve.LearningWorker)
	// Learning is about the account's own mail: not for another
	// account's mailbox, and not for a public folder either.
	if !ok || !engine.Config.Enabled || !engine.Config.Learning.Enabled || t.shared() {
		return func() {}
	}

	source := c.mbox.Name
	uses := append([]string{}, c.mbox.SpecialUse...)

	hook.SetLearningHook(func(mb model.Mailbox, m model.Meta) (string, error) {
		e := imapsieve.Event{Operation: operation, Actor: c.name, Owner: c.name, Mailbox: mb.Name, MailboxID: mb.ID, Source: source, SourceUses: uses, DestinationUses: mb.SpecialUse, Message: m}

		action := engine.LearningAction(e)
		if action == "" {
			return "", nil
		}

		e.LearningAction = action

		if m.Size > engine.Config.Learning.MaxMessageBytes {
			imapsieve.LearningOversize()
			return "", nil
		} // explicit configured size bound, counted by engine

		release, err := membudget.Acquire(m.Size)
		if err != nil {
			return "", err
		}
		defer release()

		raw, err := t.u.Read(&m)
		if err != nil {
			return "", err
		}

		return worker.Prepare(e, raw)
	})

	return func() { hook.SetLearningHook(nil) }
}
