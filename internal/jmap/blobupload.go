package jmap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"shardpost/internal/model"
)

// Blob management (RFC 9404). Blob/upload builds a blob out of text,
// base64 and ranges of other blobs; Blob/get reads blobs back, in ranges
// and as digests; Blob/lookup names the objects a blob belongs to. The
// blobs are the ones the upload endpoint makes and the download endpoint
// serves: the same store, the same lifetime — nothing references a new
// blob, so the scrubber collects it unless something comes to — and the
// same bounds.

// CapBlob is the capability of the blob methods (RFC 9404 3.1).
const CapBlob = "urn:ietf:params:jmap:blob"

// The properties of an UploadObject and its DataSourceObjects, and of
// what Blob/get returns.
const (
	propData              = "data"
	propDataAsText        = "data:asText"
	propDataAsBase64      = "data:asBase64"
	propOffset            = "offset"
	propLength            = "length"
	propIsEncodingProblem = "isEncodingProblem"
	propIsTruncated       = "isTruncated"
	propAccountID         = "accountId"
)

// blobCapability is the value of the blob capability in an account (RFC
// 9404 3.1).
type blobCapability struct {
	SupportedTypeNames        []string `json:"supportedTypeNames"`
	SupportedDigestAlgorithms []string `json:"supportedDigestAlgorithms"`
	MaxSizeBlobSet            int64    `json:"maxSizeBlobSet"`
	MaxDataSources            int      `json:"maxDataSources"`
}

// blobCapability is what the account says about the blob methods.
func (s *Server) blobCapability() *blobCapability {
	return &blobCapability{
		MaxSizeBlobSet:            s.o.Limits.MaxSizeBlobSet,
		MaxDataSources:            s.o.Limits.MaxDataSources,
		SupportedTypeNames:        s.lookupTypeNames(),
		SupportedDigestAlgorithms: digestNames,
	}
}

// blobUploadResponse is what Blob/upload answers: a /set response with
// creations only, and without a state, since blobs have none.
type blobUploadResponse struct {
	Created    map[string]map[string]any `json:"created"`
	NotCreated map[string]*setError      `json:"notCreated"`
	AccountID  string                    `json:"accountId"`
}

// dataSource is one piece of a blob to be built: octets the client sent,
// or a range of a blob it names.
type dataSource struct {
	offset *int64
	length *int64
	blobID string
	// literal holds the octets of data:asText and data:asBase64.
	literal []byte
}

// uploadSpec is one creation of a Blob/upload.
type uploadSpec struct {
	typ     *string
	sources []dataSource
}

// blobUpload implements Blob/upload (RFC 9404 4.1). Each creation is
// built in memory, bounded by maxSizeBlobSet, and stored the way an
// upload is; its id goes into createdIds, so the calls after it — and
// the creations after it in this call — can name it by "#creationId".
func blobUpload(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req struct {
		Create    map[string]json.RawMessage `json:"create"`
		AccountID string                     `json:"accountId"`
	}

	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if len(req.Create) > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	blobs, ok := sess.user.(model.Blobs)
	if !ok {
		return nil, &methodError{Type: errServerFail, Description: descNoBlobs}
	}

	out := &blobUploadResponse{AccountID: accountID(sess.account),
		Created: map[string]map[string]any{}, NotCreated: map[string]*setError{}}

	specs := map[string]*uploadSpec{}

	for cid, raw := range req.Create {
		spec, serr := parseUpload(raw, sess.limits.MaxDataSources)
		if serr != nil {
			out.NotCreated[cid] = serr
			continue
		}

		specs[cid] = spec
	}

	for len(specs) > 0 {
		for _, cid := range readyUploads(specs) {
			uploadOne(sess, blobs, cid, specs[cid], out)
			delete(specs, cid)
		}
	}

	return out, nil
}

// descNoBlobs is the error of a store that keeps no blobs.
const descNoBlobs = "this store does not serve blobs"

// readyUploads are the creations that name no other creation still to be
// made, in order. When every creation left waits for another — a cycle —
// all of them are ready: what they name is not there, and each fails
// with blobNotFound rather than the call running forever.
func readyUploads(specs map[string]*uploadSpec) []string {
	var ready, all []string

	for cid, spec := range specs {
		all = append(all, cid)

		if !waitsForCreation(spec, specs) {
			ready = append(ready, cid)
		}
	}

	if len(ready) == 0 {
		ready = all
	}

	sort.Strings(ready)

	return ready
}

// waitsForCreation reports whether a creation takes data from a creation
// of the same call that is not made yet.
func waitsForCreation(spec *uploadSpec, pending map[string]*uploadSpec) bool {
	for _, src := range spec.sources {
		if ref, ok := strings.CutPrefix(src.blobID, "#"); ok && pending[ref] != nil {
			return true
		}
	}

	return false
}

// uploadOne builds and stores one blob.
func uploadOne(sess *session, blobs model.Blobs, cid string, spec *uploadSpec, out *blobUploadResponse) {
	data, serr := buildBlob(sess, spec)
	if serr != nil {
		out.NotCreated[cid] = serr
		return
	}

	size := int64(len(data))

	if !withinQuota(sess.user, size) {
		out.NotCreated[cid] = &setError{Type: setOverQuota, Description: "the account is out of quota"}
		return
	}

	guid, err := blobs.PutBlob(data)
	if err != nil {
		sess.log.Error("jmap: Blob/upload", "err", err)

		out.NotCreated[cid] = &setError{Type: setServerFail, Description: "the blob could not be stored"}

		return
	}

	typ := defaultBlobType
	if spec.typ != nil {
		typ = blobType(*spec.typ)
	}

	// The upload endpoint's answer, and the id RFC 9404 4.1 adds.
	out.Created[cid] = map[string]any{propID: guid, propBlobID: guid, partType: typ, propSize: size,
		propAccountID: accountID(sess.account)}
	sess.created[cid] = guid
}

// parseUpload reads one UploadObject. Everything that can be checked
// without reading a blob is checked here, so a creation that is wrong
// fails before anything is read.
func parseUpload(raw json.RawMessage, maxSources int) (*uploadSpec, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	if _, ok := given[propData]; !ok {
		return nil, invalidProperty(propData, "an UploadObject needs data")
	}

	spec := &uploadSpec{}

	for k, v := range given {
		switch k {
		case propData:
			sources, serr := parseSources(v, maxSources)
			if serr != nil {
				return nil, serr
			}

			spec.sources = sources
		case partType:
			t, err := nullableString(v)
			if err != nil {
				return nil, invalidProperty(k, "the type is a string or null")
			}

			spec.typ = t
		default:
			return nil, invalidProperty(k, "an UploadObject has no property "+k)
		}
	}

	return spec, nil
}

// parseSources reads the list of DataSourceObjects.
func parseSources(raw json.RawMessage, maxSources int) ([]dataSource, *setError) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil || list == nil {
		return nil, invalidProperty(propData, "data is a list of data sources")
	}

	if len(list) > maxSources {
		return nil, invalidProperty(propData, "more data sources than maxDataSources ("+strconv.Itoa(maxSources)+")")
	}

	out := make([]dataSource, 0, len(list))

	for i, item := range list {
		src, serr := parseSource(propData+"/"+strconv.Itoa(i), item)
		if serr != nil {
			return nil, serr
		}

		out = append(out, *src)
	}

	return out, nil
}

// parseSource reads one DataSourceObject: exactly one of data:asText,
// data:asBase64 and blobId, the last with an optional range. A property
// that is null counts as absent. The error names the source by its path.
func parseSource(name string, raw json.RawMessage) (*dataSource, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, invalidProperty(name, "a data source is an object")
	}

	src := &dataSource{}
	kinds := 0

	for k, v := range given {
		if string(v) == jsonNull {
			continue
		}

		if serr := src.set(name, k, v); serr != nil {
			return nil, serr
		}

		if k != propOffset && k != propLength {
			kinds++
		}
	}

	switch {
	case kinds != 1:
		return nil, invalidProperty(name, "a data source is exactly one of data:asText, data:asBase64 and blobId")
	case src.blobID == "" && (src.offset != nil || src.length != nil):
		return nil, invalidProperty(name, "offset and length belong to a blobId")
	}

	return src, nil
}

// set reads one property of a data source.
func (src *dataSource) set(name, k string, v json.RawMessage) *setError {
	switch k {
	case propDataAsText:
		text, ok := exactText(v)
		if !ok {
			return invalidProperty(name, "data:asText is a string of valid UTF-8")
		}

		src.literal = []byte(text)
	case propDataAsBase64:
		var enc string
		if json.Unmarshal(v, &enc) != nil {
			return invalidProperty(name, "data:asBase64 is a string")
		}

		b, ok := decodeBase64Text(enc)
		if !ok {
			return invalidProperty(name, "data:asBase64 is not base64")
		}

		src.literal = b
	case propBlobID:
		if json.Unmarshal(v, &src.blobID) != nil || src.blobID == "" {
			return invalidProperty(name, "blobId is an id")
		}
	case propOffset, propLength:
		n, ok := unsignedInt(v)
		if !ok {
			return invalidProperty(name, k+" is an UnsignedInt")
		}

		if k == propOffset {
			src.offset = &n
		} else {
			src.length = &n
		}
	default:
		return invalidProperty(name, "a data source has no property "+k)
	}

	return nil
}

// exactText decodes a JSON string and reports whether it stands for the
// octets the client sent. encoding/json replaces invalid UTF-8 and lone
// surrogates by U+FFFD without a word, which would store something the
// client never sent; RFC 9404 4.1 asks for a refusal instead. A U+FFFD
// the client wrote itself, literally or escaped, is kept.
func exactText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil || !utf8.Valid(raw) {
		return "", false
	}

	replaced := strings.Count(s, string(utf8.RuneError))
	if replaced == 0 {
		return s, true
	}

	written := strings.Count(string(raw), string(utf8.RuneError)) + strings.Count(strings.ToLower(string(raw)), escapedRuneError)

	return s, replaced == written
}

// escapedRuneError is U+FFFD as a JSON escape, in lower case.
const escapedRuneError = "\x5c" + "ufffd"

// decodeBase64Text reads base64 with or without its padding; anything else
// in it — a line break, a character of another alphabet — is refused.
func decodeBase64Text(s string) ([]byte, bool) {
	enc := base64.StdEncoding
	if len(s)%4 != 0 {
		enc = base64.RawStdEncoding
	}

	b, err := enc.DecodeString(s)

	return b, err == nil
}

// unsignedInt reads a JSON UnsignedInt (RFC 8620 1.3): an integer from 0
// to 2^53-1.
func unsignedInt(raw json.RawMessage) (int64, bool) {
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 || n > maxUnsignedInt {
		return 0, false
	}

	return n, true
}

// maxUnsignedInt is the largest UnsignedInt JSON can carry exactly.
const maxUnsignedInt = 1<<53 - 1

// buildBlob puts the pieces of a creation together. Every blob it names
// is looked up first, so a creation with several missing blobs names all
// of them; then the pieces are read, and the creation stops as soon as it
// would grow beyond maxSizeBlobSet.
func buildBlob(sess *session, spec *uploadSpec) ([]byte, *setError) {
	handles, serr := sourceBlobs(sess, spec)
	if serr != nil {
		return nil, serr
	}

	limit := sess.limits.MaxSizeBlobSet
	out := []byte{}

	for i, src := range spec.sources {
		piece := src.literal

		if h := handles[i]; h != nil {
			var serr *setError

			piece, serr = readSource(h, &src, propData+"/"+strconv.Itoa(i), limit-int64(len(out)))
			if serr != nil {
				return nil, serr
			}
		}

		if int64(len(out))+int64(len(piece)) > limit {
			return nil, &setError{Type: setTooLarge, Description: descBlobSetTooLarge}
		}

		out = append(out, piece...)
	}

	return out, nil
}

// descBlobSetTooLarge is the error of a creation beyond the limit.
const descBlobSetTooLarge = "the blob would be larger than maxSizeBlobSet"

// sourceBlobs opens the blobs the sources of a creation name, by the
// index of the source.
func sourceBlobs(sess *session, spec *uploadSpec) (map[int]*blobHandle, *setError) {
	handles := map[int]*blobHandle{}

	var missing []string

	for i, src := range spec.sources {
		if src.blobID == "" {
			continue
		}

		h, err := openBlob(sess, src.blobID)

		switch {
		case errors.Is(err, model.ErrNoBlob):
			missing = append(missing, src.blobID)
		case err != nil:
			return nil, &setError{Type: setServerFail, Description: err.Error()}
		default:
			handles[i] = h
		}
	}

	if len(missing) > 0 {
		return nil, &setError{Type: setBlobNotFound, NotFound: missing}
	}

	return handles, nil
}

// readSource reads the range a source names. A range that does not lie
// within the blob is refused (RFC 9404 4.1), and one larger than room is
// tooLarge before it is read.
func readSource(h *blobHandle, src *dataSource, name string, room int64) ([]byte, *setError) {
	var off int64
	if src.offset != nil {
		off = *src.offset
	}

	if off > h.size {
		return nil, invalidProperty(name, "the range begins after the end of the blob")
	}

	n := h.size - off

	if src.length != nil {
		if *src.length > n {
			return nil, invalidProperty(name, "the range extends past the end of the blob")
		}

		n = *src.length
	}

	if n > room {
		return nil, &setError{Type: setTooLarge, Description: descBlobSetTooLarge}
	}

	b, err := h.read(off, n)
	if err != nil {
		return nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	return b, nil
}

// blobHandle is a blob a client named, ready to be read in ranges.
type blobHandle struct {
	read func(offset, length int64) ([]byte, error)
	id   string
	size int64
}

// openBlob looks up a blob the account can read — an upload, a message,
// a body part of one — by its id or by the creation id of a Blob/upload
// earlier in the request. A stored blob is read in ranges; a body part
// exists only inside its message, which is read and parsed once. The
// blob store is the account's own, so another account's blob is not
// found rather than refused.
func openBlob(sess *session, ref string) (*blobHandle, error) {
	id, pending := resolveCreationID(sess, ref)
	if pending {
		return nil, model.ErrNoBlob
	}

	blobs, ok := sess.user.(model.Blobs)
	if !ok {
		return nil, errNoIndex
	}

	if model.ValidGUID(id) {
		size, err := blobs.BlobSize(id)
		if err != nil {
			return nil, err
		}

		return &blobHandle{id: id, size: size, read: func(off, n int64) ([]byte, error) {
			return blobs.ReadBlob(id, size, off, n)
		}}, nil
	}

	content, err := blobContent(sess, id, 0)
	if err != nil {
		return nil, err
	}

	return &blobHandle{id: id, size: int64(len(content)), read: func(off, n int64) ([]byte, error) {
		return content[off : off+n], nil
	}}, nil
}
