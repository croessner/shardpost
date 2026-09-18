package interp

import (
	"context"
	"encoding/gob"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

type CmdStop struct{}

func (c CmdStop) Execute(_ context.Context, _ *RuntimeData) error {
	return ErrStop
}

type CmdFileInto struct {
	Mailbox    string
	Flags      Flags
	Copy       bool
	Create     bool   // :create (RFC 5490)
	SpecialUse string // :specialuse (RFC 8579)
}

func (c CmdFileInto) Execute(ctx context.Context, d *RuntimeData) error {
	mailbox := expandVars(d, c.Mailbox)
	if !utf8.ValidString(mailbox) {
		return fmt.Errorf("fileinto: mailbox name is not valid UTF-8")
	}
	flags := c.Flags
	if flags == nil {
		flags = d.Flags
	}
	flags = canonicalFlags(expandVarsList(d, flags), nil, d.FlagAliases)
	if d.Keep && strings.EqualFold(mailbox, "INBOX") {
		// fileinto the default mailbox is a duplicate of keep
		for i, a := range d.AppliedActions {
			if k, ok := a.(ActionKeep); ok {
				k.Flags = flags
				d.AppliedActions[i] = k
			}
		}
		return nil
	}
	for _, m := range d.Mailboxes {
		if m == mailbox {
			// duplicate delivery: one copy, the last flag list wins
			// (RFC 5232 5.1)
			for i, a := range d.AppliedActions {
				if fi, ok := a.(ActionFileInto); ok && fi.Mailbox == mailbox {
					fi.Flags = flags
					d.AppliedActions[i] = fi
				}
			}
			return nil
		}
	}

	if err := d.OnAction(ctx, ActionFileInto{
		Mailbox:    mailbox,
		Flags:      flags,
		Copy:       c.Copy,
		Create:     c.Create,
		SpecialUse: expandVars(d, c.SpecialUse),
	}, d); err != nil {
		return err
	}

	d.Mailboxes = append(d.Mailboxes, mailbox)
	if !c.Copy {
		d.ImplicitKeep = false
	}
	return nil
}

type CmdRedirect struct {
	Addr string
	Copy bool
}

func (c CmdRedirect) Execute(ctx context.Context, d *RuntimeData) error {
	value := expandVars(d, c.Addr)
	if !validRedirectAddress(value) {
		return fmt.Errorf("invalid redirect address")
	}
	addr := normalizeAddress(value)

	ok, err := d.Policy.RedirectAllowed(ctx, d, addr)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	for _, prev := range d.RedirectAddr {
		if strings.EqualFold(prev, addr) {
			return nil // duplicate redirect: one copy
		}
	}
	if len(d.RedirectAddr) >= d.Script.opts.MaxRedirects {
		return fmt.Errorf("number of redirect actions exceeds policy limit")
	}

	if err := d.OnAction(ctx, ActionRedirect{
		Address: addr,
		Copy:    c.Copy,
	}, d); err != nil {
		return err
	}

	d.RedirectAddr = append(d.RedirectAddr, addr)
	if !c.Copy {
		d.ImplicitKeep = false
	}
	return nil
}

type CmdKeep struct {
	Flags Flags
}

func (c CmdKeep) Execute(ctx context.Context, d *RuntimeData) error {
	flags := c.Flags
	if flags == nil {
		flags = d.Flags
	}
	flags = canonicalFlags(expandVarsList(d, flags), nil, d.FlagAliases)

	for _, m := range d.Mailboxes {
		if strings.EqualFold(m, "INBOX") { // keep after fileinto "INBOX": one copy
			d.Keep = true
			for i, a := range d.AppliedActions {
				if fi, ok := a.(ActionFileInto); ok && strings.EqualFold(fi.Mailbox, "INBOX") {
					fi.Flags = flags
					d.AppliedActions[i] = fi
				}
			}
			return nil
		}
	}
	if d.Keep {
		for i, a := range d.AppliedActions {
			if k, ok := a.(ActionKeep); ok {
				k.Flags = flags
				d.AppliedActions[i] = k
			}
		}
		return nil
	}
	if err := d.OnAction(ctx, ActionKeep{
		Implicit: false,
		Flags:    flags,
	}, d); err != nil {
		return err
	}

	d.Keep = true
	return nil
}

type CmdDiscard struct{}

func (c CmdDiscard) Execute(ctx context.Context, d *RuntimeData) error {
	if err := d.OnAction(ctx, ActionDiscard{}, d); err != nil {
		return err
	}

	d.ImplicitKeep = false
	d.Flags = make([]string, 0)
	return nil
}

type CmdSetFlag struct {
	Variable string
	Flags    Flags
}

func (c CmdSetFlag) Execute(_ context.Context, d *RuntimeData) error {
	if c.Flags == nil {
		return nil
	}

	flags := canonicalFlags(expandVarsList(d, c.Flags), nil, d.FlagAliases)

	if c.Variable != "" {
		if err := d.SetVar(c.Variable, strings.Join(flags, " ")); err != nil {
			return err
		}
	} else {
		d.Flags = flags
	}

	return nil
}

type CmdAddFlag struct {
	Variable string
	Flags    Flags
}

func (c CmdAddFlag) Execute(_ context.Context, d *RuntimeData) error {
	if c.Flags == nil {
		return nil
	}

	flags := expandVarsList(d, c.Flags)

	var srcFlags []string
	if c.Variable != "" {
		val, err := d.Var(c.Variable)
		if err != nil {
			return err
		}
		srcFlags = strings.Fields(val)
	} else {
		srcFlags = d.Flags
	}

	if srcFlags == nil {
		srcFlags = make([]string, len(flags))
		copy(srcFlags, flags)
	} else {
		// Use canonicalFlags to remove duplicates
		srcFlags = canonicalFlags(append(srcFlags, flags...), nil, d.FlagAliases)
	}

	if c.Variable != "" {
		if err := d.SetVar(c.Variable, strings.Join(srcFlags, " ")); err != nil {
			return err
		}
	} else {
		d.Flags = srcFlags
	}

	return nil
}

type CmdRemoveFlag struct {
	Variable string
	Flags    Flags
}

func (c CmdRemoveFlag) Execute(_ context.Context, d *RuntimeData) error {
	if c.Flags == nil {
		return nil
	}

	flags := expandVarsList(d, c.Flags)

	var srcFlags []string
	if c.Variable != "" {
		val, err := d.Var(c.Variable)
		if err != nil {
			return err
		}
		srcFlags = strings.Fields(val)
	} else {
		srcFlags = d.Flags
	}

	if srcFlags != nil {
		// Use canonicalFlags to remove duplicates
		srcFlags = canonicalFlags(srcFlags, flags, d.FlagAliases)
	}

	if c.Variable != "" {
		if err := d.SetVar(c.Variable, strings.Join(srcFlags, " ")); err != nil {
			return err
		}
	} else {
		d.Flags = srcFlags
	}

	return nil
}

type CmdReject struct {
	Reason string
}

func (c CmdReject) Execute(ctx context.Context, d *RuntimeData) error {
	if err := d.OnAction(ctx, ActionReject{Reason: expandVars(d, c.Reason)}, d); err != nil {
		return err
	}
	d.ImplicitKeep = false
	return nil
}

type CmdEReject struct {
	Reason string
}

func (c CmdEReject) Execute(ctx context.Context, d *RuntimeData) error {
	if err := d.OnAction(ctx, ActionEReject{Reason: expandVars(d, c.Reason)}, d); err != nil {
		return err
	}
	d.ImplicitKeep = false
	return nil
}

func init() {
	gob.Register(CmdStop{})
	gob.Register(CmdFileInto{})
	gob.Register(CmdRedirect{})
	gob.Register(CmdKeep{})
	gob.Register(CmdDiscard{})
	gob.Register(CmdSetFlag{})
	gob.Register(CmdAddFlag{})
	gob.Register(CmdRemoveFlag{})
	gob.Register(CmdReject{})
	gob.Register(CmdEReject{})
}

// normalizeAddress reduces a redirect argument to the bare address
// (display name and comments dropped, quoting kept only where needed).
func normalizeAddress(s string) string {
	a, err := mail.ParseAddress(addressWhitespace(s))
	if err != nil {
		return strings.TrimSpace(s)
	}
	return strings.Trim((&mail.Address{Address: a.Address}).String(), "<>")
}
