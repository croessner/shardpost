package admin

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"sort"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/archive"
	"shardpost/internal/model"
)

// Account export (roadmap 8e): the account as a versioned archive (see
// internal/archive), restored through the import API.

// RoleExport exports whole accounts.
const RoleExport = "export"

// exportWriteWindow is how long one archive entry may take to send; the
// deadline moves with the progress, so a long export is not cut off by
// the listener's write timeout.
const exportWriteWindow = 5 * time.Minute

// exportStats are the counts of an export (audit line).
type exportStats struct {
	mailboxes, messages, skipped int
	bytes                        int64
}

// ExportAccount streams the account as an archive.
func (s *Server) ExportAccount(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	u, ok := s.openAccount(w, r, RoleExport, account, true)
	if !ok {
		return
	}
	defer u.Close()

	boxes, err := u.ListMailboxes()
	if err != nil {
		s.importError(w, r, err)
		return
	}

	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Name < boxes[j].Name })

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": account + ".tar"}))
	w.WriteHeader(http.StatusOK)

	aw, err := archive.NewWriter(w, archive.Header{Account: account, Created: time.Now().UTC(), ServerVersion: s.o.Version})
	if err != nil {
		return // the client went away
	}

	x := &exporter{u: u, account: account, aw: aw, extend: func() { _ = rc.SetWriteDeadline(time.Now().Add(exportWriteWindow)) }}

	failure := x.mailboxes(boxes)

	active := ""
	if failure == nil {
		active, failure = x.sieve()
	}

	if failure != nil {
		s.o.Log.Error("account export failed", "account", account, "err", failure)
	}

	x.extend()

	_ = aw.Close(active, failure)

	auditf(r, "mailboxes", x.stats.mailboxes, "messages", x.stats.messages, "bytes", x.stats.bytes, "skipped", x.stats.skipped,
		"complete", failure == nil)
}

// exporter writes one account.
type exporter struct {
	u       model.User
	account string
	aw      *archive.Writer
	extend  func()
	stats   exportStats
}

func (x *exporter) mailboxes(boxes []model.Mailbox) error {
	for i := range boxes {
		if err := x.mailbox(&boxes[i]); err != nil {
			return fmt.Errorf("mailbox %q: %w", boxes[i].Name, err)
		}
	}

	return nil
}

// mailbox writes one mailbox with its messages; a mailbox deleted
// meanwhile is left out.
func (x *exporter) mailbox(mb *model.Mailbox) error {
	x.extend()

	info, err := x.u.Info(mb.Name)
	if errors.Is(err, model.ErrNoMailbox) {
		x.aw.SkipMailbox(mb.Name)
		return nil
	}

	if err != nil {
		return err
	}

	desc, err := x.describe(info)
	if err != nil {
		return err
	}

	msgs, err := x.u.ListMessages(info.ID)
	if err != nil {
		return err
	}

	if err := x.aw.Mailbox(desc); err != nil {
		return err
	}

	x.stats.mailboxes++

	for i := range msgs {
		if err := x.message(info.ID, &msgs[i]); err != nil {
			return err
		}
	}

	return nil
}

// describe returns the archive description of a mailbox.
func (x *exporter) describe(info *model.MailboxInfo) (archive.Mailbox, error) {
	acl, err := x.u.GetACL(info.Name)
	if err != nil {
		return archive.Mailbox{}, err
	}

	meta, err := x.u.GetMetadata(info.Name, x.account)
	if err != nil {
		return archive.Mailbox{}, err
	}

	return archive.Mailbox{ID: info.ID, Name: info.Name, UIDValidity: info.UIDValidity, UIDNext: info.UIDNext,
		HighestModSeq: info.HighestModSeq, Subscribed: info.Subscribed, SpecialUse: info.SpecialUse, ACL: acl, Metadata: meta}, nil
}

// message writes one message; one expunged meanwhile is recorded as
// skipped, any other read failure ends the export.
func (x *exporter) message(id string, m *model.Meta) error {
	x.extend()

	raw, err := x.u.Read(m)
	if err != nil {
		still, gerr := x.u.GetMessages(id, []uint32{m.UID})
		if gerr == nil && len(still) == 0 {
			x.aw.Skip(id, m.UID)
			x.stats.skipped++

			return nil
		}

		return fmt.Errorf("uid %d: %w", m.UID, err)
	}

	x.stats.messages++
	x.stats.bytes += int64(len(raw))

	return x.aw.Message(id, archive.Message{UID: m.UID, GUID: m.GUID, Flags: m.Flags, ModSeq: m.ModSeq,
		InternalDate: m.InternalDate.UTC(), SaveDate: m.SaveDate.UTC(), POP3UIDL: m.POP3UIDL}, raw)
}

// sieve writes the scripts and returns the active one.
func (x *exporter) sieve() (string, error) {
	names, active, err := x.u.ListScripts()
	if err != nil {
		return "", err
	}

	sort.Strings(names)

	for _, name := range names {
		// The JMAP vacation script is a copy the server writes; it is
		// not the user's and a restore could not store it.
		if model.ReservedScript(name) {
			continue
		}

		text, err := x.u.GetScript(name)
		if err != nil {
			return "", fmt.Errorf("sieve %q: %w", name, err)
		}

		if err := x.aw.Sieve(name, text); err != nil {
			return "", err
		}
	}

	return active, nil
}

// serverManaged is an entry the JMAP front-end keeps, which is the
// server's to write, or "".
func serverManaged(entries map[string]string) string {
	for k := range entries {
		if model.ServerManagedEntry(k) {
			return k
		}
	}

	return ""
}

// metadataChange is what turns the entries a mailbox has into the ones
// wanted: the others removed — except those the server keeps — and the
// new or different ones set.
func metadataChange(have, want map[string]string) map[string]*string {
	change := map[string]*string{}

	for k := range have {
		if _, keep := want[k]; !keep && !model.ServerManagedEntry(k) {
			change[k] = nil
		}
	}

	for k, v := range want {
		if have[k] != v {
			change[k] = &v
		}
	}

	return change
}

// PutImportMetadata replaces the annotations of a mailbox.
func (s *Server) PutImportMetadata(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID,
	p adminapi.PutImportMetadataParams,
) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	var req adminapi.ImportMetadata
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid metadata: "+err.Error())
		return
	}

	if k := serverManaged(req.Entries); k != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "metadata: "+k+" is kept by the server")
		return
	}

	mb, err := c.imp.MailboxByID(id)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "mailbox", mb.Name, "entries", len(req.Entries))

	have, err := c.u.GetMetadata(mb.Name, account)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	if change := metadataChange(have, req.Entries); len(change) > 0 {
		if err := c.u.SetMetadata(mb.Name, account, change); err != nil {
			writeError(w, http.StatusBadRequest, adminapi.BadRequest, "metadata: "+err.Error())
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
