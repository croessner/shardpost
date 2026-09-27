package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"

	"shardpost/internal/adminapi"
	"shardpost/internal/archive"
	"shardpost/internal/model"
)

// Restore of an account archive (roadmap 8e) through the import API: the
// mailboxes with their UIDVALIDITY and ids, the messages with UIDs,
// flags, MODSEQ, dates and POP3 UIDLs, the ACLs, annotations and Sieve
// scripts. A restore is repeatable (messages already there are skipped).

// Restore batch bounds (below the import limits).
const (
	restoreBatchBytes    = 16 << 20
	restoreBatchMessages = 200
)

// importID is a mailbox id the import API takes as the preferred id.
var importID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// RestoreOptions control a restore.
type RestoreOptions struct {
	// Account is the target ("" = the exported account). Into another
	// account ACLs are not restored and no Sieve script is activated
	// unless ACL or ActivateSieve say so (an isolated restore must not
	// grant rights or send vacation replies).
	Account       string
	ACL           bool
	ActivateSieve bool
	Log           *slog.Logger
}

// RestoreReport is the outcome of a restore.
type RestoreReport struct {
	Source    string                 `json:"source"`
	Account   string                 `json:"account"`
	Mailboxes []RestoreMailboxReport `json:"mailboxes"`
	Scripts   []string               `json:"scripts"`
	Active    string                 `json:"active_sieve"`
	// ACLRestored and SieveActivated say what was applied.
	ACLRestored    bool     `json:"acl_restored"`
	SieveActivated bool     `json:"sieve_activated"`
	Skipped        []string `json:"skipped_at_export,omitempty"`
}

// RestoreMailboxReport is one restored mailbox.
type RestoreMailboxReport struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	UIDValidity uint32 `json:"uidvalidity"`
	Messages    int    `json:"messages"`
	Imported    int    `json:"imported"`
	Existing    int    `json:"existing"`
	Verified    bool   `json:"verified"`
}

// restore is one run.
type restore struct {
	tgt     Target
	o       RestoreOptions
	rep     *RestoreReport
	cur     *restoreMailbox
	scripts []adminapi.ImportSieveScript
}

// restoreMailbox is the mailbox being restored.
type restoreMailbox struct {
	desc     *archive.Mailbox
	id       string
	batch    []adminapi.ImportPart
	bytes    int
	sizes    map[uint32]int64
	lastUID  uint32
	maxMod   uint64
	reportAt int
}

// Restore reads the archive and imports it. The archive is verified while
// it is read; call it on an archive checked with archive.Reader first to
// avoid a partial restore of a damaged archive.
func Restore(ctx context.Context, r io.Reader, tgt Target, o RestoreOptions) (*RestoreReport, error) {
	ar, err := archive.NewReader(r)
	if err != nil {
		return nil, err
	}

	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	rep := &RestoreReport{Source: ar.Header.Account, Account: o.Account}
	if rep.Account == "" {
		rep.Account = ar.Header.Account
	}

	same := rep.Account == ar.Header.Account
	o.ACL = o.ACL || same
	o.ActivateSieve = o.ActivateSieve || same
	rep.ACLRestored = o.ACL

	if _, err := tgt.Open(ctx, rep.Account, ""); err != nil {
		return nil, fmt.Errorf("open import: %w", err)
	}

	x := &restore{tgt: tgt, o: o, rep: rep}
	err = x.run(ctx, ar)

	if cerr := tgt.Close(context.WithoutCancel(ctx)); err == nil && cerr != nil {
		err = fmt.Errorf("close import: %w", cerr)
	}

	return rep, err
}

func (x *restore) run(ctx context.Context, ar *archive.Reader) error {
	for {
		e, err := ar.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		if err := x.entry(ctx, e); err != nil {
			return err
		}
	}
}

func (x *restore) entry(ctx context.Context, e *archive.Entry) error {
	switch e.Kind {
	case archive.KindMailbox:
		if err := x.finish(ctx); err != nil {
			return err
		}

		return x.mailbox(ctx, e.Mailbox)
	case archive.KindMessage:
		return x.message(ctx, e)
	case archive.KindSieve:
		// An archive of an older version may hold the JMAP vacation
		// script, which the target refuses: it is the server's copy.
		if model.ReservedScript(e.Name) {
			return nil
		}

		x.scripts = append(x.scripts, adminapi.ImportSieveScript{Name: e.Name, Script: string(e.Raw)})
		x.rep.Scripts = append(x.rep.Scripts, e.Name)

		return nil
	case archive.KindManifest:
		if err := x.finish(ctx); err != nil {
			return err
		}

		x.rep.Skipped = e.Manifest.Skipped

		return x.sieve(ctx, e.Manifest.ActiveSieve)
	default:
		return fmt.Errorf("unknown archive entry %d", e.Kind)
	}
}

// mailbox creates (or takes over) the mailbox with its identity.
func (x *restore) mailbox(ctx context.Context, m *archive.Mailbox) error {
	spec := adminapi.ImportMailboxSpec{Name: m.Name, Uidvalidity: int64(m.UIDValidity), Subscribed: &m.Subscribed}
	if importID.MatchString(m.ID) {
		spec.Id = &m.ID
	}

	if len(m.SpecialUse) > 0 {
		spec.SpecialUse = &m.SpecialUse
	}

	st, err := x.tgt.PutMailbox(ctx, spec)
	if err != nil {
		return fmt.Errorf("mailbox %q: %w", m.Name, err)
	}

	x.rep.Mailboxes = append(x.rep.Mailboxes, RestoreMailboxReport{Name: m.Name, ID: st.Id, UIDValidity: m.UIDValidity})
	x.cur = &restoreMailbox{desc: m, id: st.Id, sizes: map[uint32]int64{}, reportAt: len(x.rep.Mailboxes) - 1}

	x.o.Log.Info("restoring mailbox", "mailbox", m.Name)

	return nil
}

func (x *restore) message(ctx context.Context, e *archive.Entry) error {
	c := x.cur
	if c == nil || e.MailboxID != c.desc.ID {
		return fmt.Errorf("%w: message outside its mailbox", archive.ErrFormat)
	}

	m := e.Message
	c.batch = append(c.batch, adminapi.ImportPart{Raw: e.Raw, Meta: adminapi.ImportMessageMeta{UID: m.UID, ModSeq: m.ModSeq,
		Flags: storableFlags(m.Flags), InternalDate: m.InternalDate, SaveDate: m.SaveDate, GUID: m.GUID, POP3UIDL: m.POP3UIDL}})
	c.bytes += len(e.Raw)
	c.sizes[m.UID] = m.Size
	c.lastUID = max(c.lastUID, m.UID)
	c.maxMod = max(c.maxMod, m.ModSeq)

	if len(c.batch) >= restoreBatchMessages || c.bytes >= restoreBatchBytes {
		return x.flush(ctx, c)
	}

	return nil
}

// flush imports the pending batch of c.
func (x *restore) flush(ctx context.Context, c *restoreMailbox) error {
	if len(c.batch) == 0 {
		return nil
	}

	res, err := x.tgt.Import(ctx, c.id, c.batch)
	if err != nil {
		return fmt.Errorf("mailbox %q: import: %w", c.desc.Name, err)
	}

	r := &x.rep.Mailboxes[c.reportAt]
	r.Messages += len(c.batch)
	r.Imported += res.Imported
	r.Existing += res.Existing
	c.batch, c.bytes = nil, 0

	return nil
}

// finish completes the current mailbox: the last batch, UIDNEXT and
// HIGHESTMODSEQ, ACL, annotations and the check against the inventory.
func (x *restore) finish(ctx context.Context) error {
	c := x.cur
	if c == nil {
		return nil
	}

	x.cur = nil

	if err := x.flush(ctx, c); err != nil {
		return err
	}

	uidNext := max(c.desc.UIDNext, c.lastUID+1)
	if _, err := x.tgt.Finalize(ctx, c.id, uidNext, max(c.desc.HighestModSeq, c.maxMod)); err != nil {
		return fmt.Errorf("mailbox %q: finalize: %w", c.desc.Name, err)
	}

	if err := x.extras(ctx, c); err != nil {
		return err
	}

	have, err := inventory(ctx, x.tgt, c.id)
	if err != nil {
		return fmt.Errorf("mailbox %q: inventory: %w", c.desc.Name, err)
	}

	verified := len(have) == len(c.sizes)
	for uid, size := range c.sizes {
		verified = verified && have[uid].Size == size
	}

	x.rep.Mailboxes[c.reportAt].Verified = verified
	if !verified {
		return fmt.Errorf("mailbox %q: %d messages in the target, %d in the archive or sizes differ", c.desc.Name, len(have), len(c.sizes))
	}

	return nil
}

// extras restores ACL (if allowed) and annotations.
func (x *restore) extras(ctx context.Context, c *restoreMailbox) error {
	if x.o.ACL {
		if err := x.tgt.PutACL(ctx, c.id, orEmpty(c.desc.ACL)); err != nil {
			return fmt.Errorf("mailbox %q: acl: %w", c.desc.Name, err)
		}
	}

	if mt, ok := x.tgt.(MetadataTarget); ok && len(c.desc.Metadata) > 0 {
		if err := mt.PutMetadata(ctx, c.id, c.desc.Metadata); err != nil {
			return fmt.Errorf("mailbox %q: metadata: %w", c.desc.Name, err)
		}
	}

	return nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}

	return m
}

// sieve stores the scripts; the active one only when allowed.
func (x *restore) sieve(ctx context.Context, active string) error {
	if len(x.scripts) == 0 {
		return nil
	}

	// An archive from before the reserved name could name the JMAP
	// vacation copy as active; it is never restored, so nothing is.
	if !x.o.ActivateSieve || model.ReservedScript(active) {
		active = ""
	}

	x.rep.Active, x.rep.SieveActivated = active, active != ""

	if err := x.tgt.PutSieve(ctx, x.scripts, active); err != nil {
		return fmt.Errorf("sieve: %w", err)
	}

	return nil
}
