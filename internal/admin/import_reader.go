package admin

import (
	"math"
	"net/http"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// readerChunk bounds the UIDs of one change of a reader's marks, the
// way a client bounds a STORE: one change record names them all.
const readerChunk = 1000

// PutImportReader makes an account's own \Seen in a public folder what
// the source had for that account, and optionally sets its subscription.
// The import itself stores the source user's \Seen as the shared
// default (see docs/development/public-folders.md, decision 6); here
// only the difference to what the reader sees is written, as private
// marks under a MODSEQ of the mailbox.
func (s *Server) PutImportReader(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, reader adminapi.Reader,
	p adminapi.PutImportReaderParams,
) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	auditf(r, "mailbox_id", id, "reader", reader)

	req, want, ok := s.readerRequest(w, r, account, reader)
	if !ok {
		return
	}

	mb, err := c.imp.MailboxByID(id)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	if int64(mb.UIDValidity) != req.Uidvalidity {
		s.importError(w, r, model.ImportConflict("the mailbox has UIDVALIDITY %d, not %d", mb.UIDValidity, req.Uidvalidity))
		return
	}

	ru, err := s.o.Import.User(reader)
	if err != nil {
		s.importError(w, r, err)
		return
	}
	defer ru.Close()

	changed, err := setReaderSeen(c, ru, mb, want)
	if err == nil && req.Subscribed != nil {
		err = ru.SetPublicSubscribed(mb.ID, *req.Subscribed)
	}

	auditf(r, "mailbox", mb.Name, "changed", changed)

	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, adminapi.ImportReaderResult{Changed: changed})
}

// readerRequest reads and checks a reader state and the reader: only
// the public namespace has readers, and the reader is a canonical
// account within the caller's scope. ok false means the response is
// written.
func (s *Server) readerRequest(w http.ResponseWriter, r *http.Request, account, reader string) (*adminapi.ImportReader, map[uint32]bool, bool) {
	if !model.IsPublic(account) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "only folders of the public namespace have readers")
		return nil, nil, false
	}

	var req adminapi.ImportReader
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid reader state: "+err.Error())
		return nil, nil, false
	}

	want, msg := readerSeen(&req)
	if msg == "" && (!validAccount(reader) || model.ReservedNamespace(reader)) {
		msg = "invalid reader"
	}

	if msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
		return nil, nil, false
	}

	if !requireAccount(w, r, RoleImport, reader) || !s.canonical(w, r, reader) {
		return nil, nil, false
	}

	return &req, want, true
}

// readerSeen validates a reader state and returns the set of seen UIDs,
// or the problem.
func readerSeen(req *adminapi.ImportReader) (map[uint32]bool, string) {
	if req.Uidvalidity < 1 || req.Uidvalidity > math.MaxUint32 {
		return nil, msgInvalidUIDValidity
	}

	want := make(map[uint32]bool, len(req.Seen))

	for _, uid := range req.Seen {
		if uid < 1 || uid > math.MaxUint32 {
			return nil, msgInvalidUID
		}

		want[uint32(uid)] = true
	}

	return want, ""
}

// setReaderSeen writes the reader's marks where what the reader sees —
// its own mark, or the shared \Seen without one — differs from want.
// It returns how many messages changed.
func setReaderSeen(c *importCall, reader model.User, mb *model.Mailbox, want map[uint32]bool) (int, error) {
	marks, err := reader.PrivateSeen(mb.ID, mb.UIDValidity)
	if err != nil {
		return 0, err
	}

	var set, unset []uint32

	for after := uint32(0); ; {
		metas, err := c.imp.ListMessagesAfter(mb.ID, after, defaultPageSize)
		if err != nil {
			return 0, err
		}

		for i := range metas {
			m := &metas[i]

			seen := model.HasFlag(m.Flags, flagSeen)
			if mark, ok := marks[m.UID]; ok {
				seen = mark.Seen
			}

			switch {
			case seen == want[m.UID]:
			case seen:
				unset = append(unset, m.UID)
			default:
				set = append(set, m.UID)
			}
		}

		if len(metas) < defaultPageSize {
			break
		}

		after = metas[len(metas)-1].UID
	}

	for _, change := range []struct {
		uids []uint32
		seen bool
	}{{set, true}, {unset, false}} {
		for uids := change.uids; len(uids) > 0; {
			chunk := uids[:min(len(uids), readerChunk)]
			uids = uids[len(chunk):]

			if _, err := c.u.TouchModSeq(mb.Name, chunk, func(modseq uint64) error {
				return reader.SetPrivateSeen(mb.ID, mb.UIDValidity, chunk, change.seen, modseq)
			}); err != nil {
				return 0, err
			}
		}
	}

	return len(set) + len(unset), nil
}
