package adminapi

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"time"
)

// ImportMetaHeader is the part header of an importMessages body that
// carries the identity of the message in the part (ImportMessageMeta as
// JSON).
const ImportMetaHeader = "X-Import-Meta"

// ImportMessageMeta is the identity of one imported message (see the
// importMessages operation). It travels in a part header, which the
// contract cannot type; server and client share this definition.
type ImportMessageMeta struct {
	UID          uint32    `json:"uid"`
	ModSeq       uint64    `json:"modseq"`
	Flags        []string  `json:"flags"`
	InternalDate time.Time `json:"internal_date"`
	SaveDate     time.Time `json:"save_date,omitzero"`
	GUID         string    `json:"guid,omitempty"`
	POP3UIDL     string    `json:"pop3_uidl,omitempty"`
}

// ImportPart is one message of an importMessages body.
type ImportPart struct {
	Meta ImportMessageMeta
	Raw  []byte
}

// WriteImportBody writes parts as the multipart/mixed body of
// importMessages and returns its content type.
func WriteImportBody(w io.Writer, parts []ImportPart) (string, error) {
	mw := multipart.NewWriter(w)

	for i := range parts {
		meta, err := json.Marshal(&parts[i].Meta)
		if err != nil {
			return "", err
		}

		h := textproto.MIMEHeader{}
		h.Set("Content-Type", "message/rfc822")
		h.Set(ImportMetaHeader, string(meta))

		pw, err := mw.CreatePart(h)
		if err != nil {
			return "", err
		}

		if _, err := pw.Write(parts[i].Raw); err != nil {
			return "", fmt.Errorf("write part: %w", err)
		}
	}

	if err := mw.Close(); err != nil {
		return "", err
	}

	return "multipart/mixed; boundary=" + mw.Boundary(), nil
}
