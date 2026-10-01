package sieve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// scriptSuffix is the file name suffix of a script in a directory.
const scriptSuffix = ".sieve"

// Script is one script of a delivery sequence.
type Script struct {
	// Name identifies the script in logs and reports ("before/10-spam",
	// PersonalScript).
	Name string
	Text string
}

// Names of the scripts of a delivery sequence: the account's active
// script, and the labels of the global ones before and after it.
const (
	PersonalScript = "personal"
	LabelBefore    = "before"
	LabelAfter     = "after"
)

// Sequence puts the account's active script between the global scripts
// that run before and after it.
func Sequence(before []Script, personal string, after []Script) []Script {
	out := make([]Script, 0, len(before)+1+len(after))
	out = append(out, before...)
	out = append(out, Script{Name: PersonalScript, Text: personal})

	return append(out, after...)
}

// ScriptError is a script of a sequence that did not run.
type ScriptError struct {
	Script string
	Err    error
}

func (e ScriptError) Error() string { return e.Script + ": " + e.Err.Error() }

func (e ScriptError) Unwrap() error { return e.Err }

// LoadDir reads the scripts of a directory: every regular file named
// <name>.sieve, in name order. label prefixes the script names
// ("before", "after"). An empty dir has no scripts. Files are read on
// every call, so a changed directory (a remounted ConfigMap) takes
// effect with the next delivery. A file that cannot be read does not
// cost the others: the scripts that could be read come back together
// with the error.
func LoadDir(dir, label string) ([]Script, error) {
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var names []string

	for _, e := range entries {
		// Hidden entries are not scripts (a ConfigMap mount keeps its
		// data below "..data").
		if name := e.Name(); strings.HasSuffix(name, scriptSuffix) && !strings.HasPrefix(name, ".") {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	var (
		out  = make([]Script, 0, len(names))
		errs []error
	)

	for _, name := range names {
		path := filepath.Join(dir, name)

		// A ConfigMap entry is a symlink: follow it, skip directories.
		st, err := os.Stat(path)
		if err == nil && !st.Mode().IsRegular() {
			continue
		}

		var text []byte
		if err == nil {
			text, err = os.ReadFile(path)
		}

		if err != nil {
			errs = append(errs, err)
			continue
		}

		out = append(out, Script{Name: label + "/" + strings.TrimSuffix(name, scriptSuffix), Text: string(text)})
	}

	return out, errors.Join(errs...)
}

// CompileDir validates every script of a directory with the given hooks;
// it returns the first problem.
func CompileDir(dir, label string, h Hooks) error {
	scripts, err := LoadDir(dir, label)
	if err != nil {
		return err
	}

	for _, sc := range scripts {
		if err := CompileWith(sc.Text, h); err != nil {
			return fmt.Errorf("%s: %w", sc.Name, err)
		}
	}

	return nil
}

// RunSequence runs scripts one after the other over one message, the
// way Pigeonhole runs sieve_before, the personal script and sieve_after:
// the next script runs only while the message is still kept, that keep
// is not carried out but handed on, and the actions of all scripts add
// up. So a script that files the message somewhere or discards it ends
// the sequence, and the keep of the last script that ran is the one
// that stores the message in INBOX. A header edit of one script is what
// the next one sees, and so are the flags of the keep handed on: they
// are what the next script's flag variable (imap4flags) starts with, so
// a mark survives a later script that just keeps the message, and a
// later script can still take it away.
//
// A script that does not compile or fails at runtime is skipped as if
// it had kept the message — a broken global script must not lose mail,
// and a broken personal script must not stop the global ones after it.
// Its actions are dropped; the failures are returned with the result.
//
// A script whose only non-keeping action is a pipe ends the sequence
// too: whether that pipe leaves the keep in place is known only after
// it ran (see RunPipes).
func RunSequence(ctx context.Context, scripts []Script, from, to, user string, raw []byte, h Hooks) (*Delivery, []ScriptError) {
	out := &Delivery{Raw: raw}
	keep := &FileInto{Mailbox: inboxName, Keep: true}

	var failed []ScriptError

	for _, sc := range scripts {
		// The flags of the keep so far are what the next script starts
		// with: it may add to them, take one away or replace them.
		h.Flags = keep.scriptFlags

		d, err := RunWithHooks(ctx, sc.Text, from, to, user, out.Raw, h)
		if err != nil {
			failed = append(failed, ScriptError{Script: sc.Name, Err: err})
			continue
		}

		out.Scripts = append(out.Scripts, sc.Name)

		if keep = out.merge(d); keep == nil {
			break
		}
	}

	if keep != nil {
		out.FileInto = append(out.FileInto, *keep)
	}

	return out, failed
}

// merge adds the outcome of the next script of a sequence and returns
// its keep, which the caller hands on (nil: the script did not keep the
// message).
func (d *Delivery) merge(next *Delivery) *FileInto {
	var keep *FileInto

	for i := range next.FileInto {
		if next.FileInto[i].Keep {
			keep = &next.FileInto[i]
			continue
		}

		d.FileInto = append(d.FileInto, next.FileInto[i])
	}

	d.Raw = next.Raw
	d.Redirects = append(d.Redirects, next.Redirects...)
	d.Tracked = append(d.Tracked, next.Tracked...)
	d.Notifications = append(d.Notifications, next.Notifications...)
	d.Pipes = append(d.Pipes, next.Pipes...)
	d.pipeKeep = next.pipeKeep

	if next.Rejected != "" {
		d.Rejected, d.EReject = next.Rejected, next.EReject
	}

	// One reply per message: the first script that asks for one wins.
	if d.Vacation == nil {
		d.Vacation = next.Vacation
	}

	return keep
}
