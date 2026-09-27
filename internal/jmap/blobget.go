package jmap

import (
	"context"
	"crypto/sha1" // RFC 9404 names "sha"; a checksum here, not a signature
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// The digest algorithms Blob/get computes, by their names in the HTTP
// Digest Algorithm Values registry, lower-cased as RFC 9404 3.1 asks.
// The stronger one comes first: a client should prefer the earlier.
const (
	digestSHA256 = "sha-256"
	digestSHA    = "sha"
	digestPrefix = "digest:"
)

// digestNames are the algorithms in the order the capability lists them.
var digestNames = []string{digestSHA256, digestSHA}

// digests make the hash of each algorithm.
var digests = map[string]func() hash.Hash{digestSHA256: sha256.New, digestSHA: sha1.New}

// digestChunk is how much of a blob is read at a time for a digest
// without its data.
const digestChunk = 1 << 20

// blobProps are the properties a Blob/get asks for.
type blobProps struct {
	digests []string
	data    bool
	text    bool
	base64  bool
	size    bool
}

// wantsData reports whether the octets themselves go into the response.
func (p *blobProps) wantsData() bool { return p.data || p.text || p.base64 }

// blobPropertyList reads the properties of a Blob/get; none asks for
// data and size (RFC 9404 4.2).
func blobPropertyList(props *[]string) (*blobProps, *methodError) {
	if props == nil {
		return &blobProps{data: true, size: true}, nil
	}

	out := &blobProps{}

	for _, p := range *props {
		switch p {
		case propID:
		case propData:
			out.data = true
		case propDataAsText:
			out.text = true
		case propDataAsBase64:
			out.base64 = true
		case propSize:
			out.size = true
		default:
			alg, ok := strings.CutPrefix(p, digestPrefix)
			if !ok || digests[alg] == nil {
				return nil, &methodError{Type: errInvalidArguments, Arguments: []string{argProperties},
					Description: "no such property: " + p}
			}

			out.digests = append(out.digests, alg)
		}
	}

	return out, nil
}

// blobGetResponse is what Blob/get answers: a /get response without a
// state, since blobs have none.
type blobGetResponse struct {
	AccountID string           `json:"accountId"`
	List      []map[string]any `json:"list"`
	NotFound  []string         `json:"notFound"`
}

// blobGetRequest is the arguments of a Blob/get.
type blobGetRequest struct {
	getRequest

	Offset *int64 `json:"offset"`
	Length *int64 `json:"length"`
}

// blobGet implements Blob/get (RFC 9404 4.2). Every blob id this server
// hands out can be read: uploads, messages and their body parts. The
// data one call returns is bounded by maxSizeBlobGet — the whole call,
// not each blob, because it all ends up in one response; a call beyond
// it is requestTooLarge, and the client asks for a range or downloads
// the blob. A digest without the data is computed a chunk at a time and
// is not counted.
func blobGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req blobGetRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	props, merr := req.check(sess)
	if merr != nil {
		return nil, merr
	}

	out := &blobGetResponse{AccountID: accountID(sess.account), List: []map[string]any{}, NotFound: []string{}}
	budget := sess.limits.MaxSizeBlobGet
	seen := map[string]bool{}

	for _, ref := range *req.IDs {
		if seen[ref] {
			continue
		}

		seen[ref] = true

		h, err := openBlob(sess, ref)

		switch {
		case errors.Is(err, model.ErrNoBlob):
			out.NotFound = append(out.NotFound, ref)
			continue
		case err != nil:
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		}

		obj, merr := renderBlob(h, props, &req, &budget)
		if merr != nil {
			return nil, merr
		}

		out.List = append(out.List, obj)
	}

	return out, nil
}

// check refuses what a Blob/get cannot answer and returns the
// properties it asks for.
func (req *blobGetRequest) check(sess *session) (*blobProps, *methodError) {
	if req.IDs == nil || len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: "Blob/get needs ids, at most maxObjectsInGet of them"}
	}

	if (req.Offset != nil && *req.Offset < 0) || (req.Length != nil && *req.Length < 0) {
		return nil, &methodError{Type: errInvalidArguments, Description: "offset and length are UnsignedInts"}
	}

	return blobPropertyList(req.Properties)
}

// renderBlob is one blob of a Blob/get.
func renderBlob(h *blobHandle, props *blobProps, req *blobGetRequest, budget *int64) (map[string]any, *methodError) {
	lo, hi, truncated := blobRange(h.size, req.Offset, req.Length)

	obj := map[string]any{propID: h.id}

	if props.size {
		obj[propSize] = h.size
	}

	if truncated {
		obj[propIsTruncated] = true
	}

	var data []byte

	if props.wantsData() {
		if hi-lo > *budget {
			return nil, &methodError{Type: errRequestTooLarge, Description: "the data asked for is more than maxSizeBlobGet (" +
				strconv.FormatInt(*budget, 10) + " bytes left in this call); ask for a range, or download the blob"}
		}

		*budget -= hi - lo

		var err error
		if data, err = h.read(lo, hi-lo); err != nil {
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		}

		putBlobData(obj, props, data)
	}

	if len(props.digests) > 0 {
		sums, err := digestRange(h, props.digests, lo, hi, data, props.wantsData())
		if err != nil {
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		}

		for alg, sum := range sums {
			obj[digestPrefix+alg] = sum
		}
	}

	return obj, nil
}

// blobRange is the part of a blob a Blob/get selects, and whether the
// request asked for more than there is: a start past the end, or a
// length that runs past it. Without a length the rest of the blob is
// selected, which is never more than there is.
func blobRange(size int64, offset, length *int64) (lo, hi int64, truncated bool) {
	if offset != nil {
		lo = *offset
	}

	if lo > size {
		lo, truncated = size, true
	}

	hi = size

	if length != nil {
		if *length > size-lo {
			truncated = true
		} else {
			hi = lo + *length
		}
	}

	return lo, hi, truncated
}

// putBlobData writes the selected octets in the forms asked for. Octets
// that are not UTF-8 cannot be text: data then falls back to base64,
// data:asText is null, and either way the object says isEncodingProblem.
func putBlobData(obj map[string]any, props *blobProps, data []byte) {
	valid := utf8.Valid(data)
	encoded := base64.StdEncoding.EncodeToString(data)

	if props.data {
		if valid {
			obj[propDataAsText] = string(data)
		} else {
			obj[propDataAsBase64] = encoded
		}
	}

	if props.text {
		if valid {
			obj[propDataAsText] = string(data)
		} else {
			obj[propDataAsText] = nil
		}
	}

	if props.base64 {
		obj[propDataAsBase64] = encoded
	}

	if !valid && (props.data || props.text) {
		obj[propIsEncodingProblem] = true
	}
}

// digestRange computes the digests of the selected octets, base64 as RFC
// 9404 4.2 writes them. The octets are hashed from data when the call
// read them anyway, otherwise read a chunk at a time.
func digestRange(h *blobHandle, algs []string, lo, hi int64, data []byte, have bool) (map[string]string, error) {
	hashes := map[string]hash.Hash{}
	writers := make([]io.Writer, 0, len(algs))

	for _, alg := range algs {
		if hashes[alg] == nil {
			hashes[alg] = digests[alg]()
			writers = append(writers, hashes[alg])
		}
	}

	w := io.MultiWriter(writers...)

	if have {
		_, _ = w.Write(data)
	} else {
		for pos := lo; pos < hi; pos += digestChunk {
			piece, err := h.read(pos, min(digestChunk, hi-pos))
			if err != nil {
				return nil, err
			}

			_, _ = w.Write(piece)
		}
	}

	out := make(map[string]string, len(hashes))
	for alg, sum := range hashes {
		out[alg] = base64.StdEncoding.EncodeToString(sum.Sum(nil))
	}

	return out, nil
}

// lookupTypes are the data types Blob/lookup answers for, with the
// capability a request has to use for each. A message's blob belongs to
// the message, to its thread and to the mailboxes it is in; a body part
// belongs to what its message belongs to; a script's content to the
// scripts that have it.
var lookupTypes = map[string]string{typeEmail: CapMail, typeMailbox: CapMail, typeThread: CapMail, typeSieveScript: CapSieve}

// lookupCapability is the capability a type name of Blob/lookup needs,
// when this server answers for it.
func (s *Server) lookupCapability(name string) (string, bool) {
	capability, ok := lookupTypes[name]
	if !ok || (capability == CapSieve && s.o.Sieve == nil) {
		return "", false
	}

	return capability, true
}

// lookupTypeNames are the type names the blob capability lists.
func (s *Server) lookupTypeNames() []string {
	out := make([]string, 0, len(lookupTypes))

	for name := range lookupTypes {
		if _, ok := s.lookupCapability(name); ok {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}

// blobLookupRequest is the arguments of a Blob/lookup.
type blobLookupRequest struct {
	TypeNames []string `json:"typeNames"`
	IDs       []string `json:"ids"`
	AccountID string   `json:"accountId"`
}

// check refuses what a Blob/lookup cannot answer: a type this server
// does not look up, or one whose capability the request did not use.
func (req *blobLookupRequest) check(sess *session) *methodError {
	if req.TypeNames == nil || req.IDs == nil {
		return &methodError{Type: errInvalidArguments, Description: "Blob/lookup needs typeNames and ids"}
	}

	if len(req.IDs) > sess.limits.MaxObjectsInGet {
		return &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	for _, name := range req.TypeNames {
		if capability, ok := sess.srv.lookupCapability(name); !ok || !sess.using[capability] {
			return &methodError{Type: errUnknownDataType, Description: "no lookup for the type " + name}
		}
	}

	return nil
}

// blobInfo is one entry of a Blob/lookup response.
type blobInfo struct {
	MatchedIDs map[string][]string `json:"matchedIds"`
	ID         string              `json:"id"`
}

// blobLookup implements Blob/lookup (RFC 9404 4.3). A blob id names its
// message — the message's own blob is its GUID, a body part's blob the
// GUID with the part — so the lookup is a locator lookup, not a search:
// what it finds is the message, its thread and its mailboxes. A body
// part is matched by its message without parsing it. A blob no message
// of the account has, or that does not exist, gets empty lists, which
// RFC 9404 asks for so that nothing is learnt about other accounts.
func blobLookup(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req blobLookupRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if merr := req.check(sess); merr != nil {
		return nil, merr
	}

	messages, merr := lookupMessages(sess, req.IDs)
	if merr != nil {
		return nil, merr
	}

	scripts, merr := lookupScripts(sess, req.TypeNames, req.IDs)
	if merr != nil {
		return nil, merr
	}

	list := make([]blobInfo, 0, len(req.IDs))

	for _, ref := range req.IDs {
		matched := make(map[string][]string, len(req.TypeNames))
		for _, name := range req.TypeNames {
			matched[name] = []string{}
		}

		if f := messages[ref]; f != nil {
			fillMatches(matched, f)
		}

		if ids := scripts[ref]; ids != nil {
			matched[typeSieveScript] = ids
		}

		list = append(list, blobInfo{ID: ref, MatchedIDs: matched})
	}

	return &blobLookupResponse{AccountID: accountID(sess.account), List: list, NotFound: []string{}}, nil
}

// blobLookupResponse is what Blob/lookup answers. notFound stays empty:
// every blob is answered, found or not.
type blobLookupResponse struct {
	AccountID string     `json:"accountId"`
	List      []blobInfo `json:"list"`
	NotFound  []string   `json:"notFound"`
}

// lookupMessages finds the messages the blob ids belong to, by the id as
// the client wrote it.
func lookupMessages(sess *session, refs []string) (map[string]*found, *methodError) {
	byRef := map[string]string{}

	var guids []string

	for _, ref := range refs {
		id, _ := resolveCreationID(sess, ref)

		guid := id
		if g, _, ok := parsePartBlob(id); ok {
			guid = g
		}

		if model.ValidGUID(guid) {
			byRef[ref] = guid
			guids = append(guids, guid)
		}
	}

	out := map[string]*found{}

	if len(guids) == 0 {
		return out, nil
	}

	_, loc, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	hits, err := loc.resolve(guids)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	for ref, guid := range byRef {
		if f := hits[guid]; f != nil && len(f.metas) > 0 {
			out[ref] = f
		}
	}

	return out, nil
}

// fillMatches fills in the ids of a message for the types asked for.
func fillMatches(matched map[string][]string, f *found) {
	m := &f.metas[0]

	if _, ok := matched[typeEmail]; ok {
		matched[typeEmail] = []string{m.GUID}
	}

	if _, ok := matched[typeThread]; ok {
		matched[typeThread] = []string{mailmeta.ThreadID(m)}
	}

	if _, ok := matched[typeMailbox]; ok {
		boxes := make([]string, 0, len(f.boxes))
		for id := range f.boxes {
			boxes = append(boxes, id)
		}

		sort.Strings(boxes)
		matched[typeMailbox] = boxes
	}
}

// lookupScripts finds the scripts whose content is one of the blobs, by
// the blob id as the client wrote it; only when the lookup asks for them.
func lookupScripts(sess *session, types, refs []string) (map[string][]string, *methodError) {
	out := map[string][]string{}

	if !containsString(types, typeSieveScript) {
		return out, nil
	}

	b, err := loadScripts(sess)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	for _, ref := range refs {
		id, _ := resolveCreationID(sess, ref)

		for _, sc := range b.list {
			if scriptBlobID(sc.text) == id {
				out[ref] = append(out[ref], sc.id)
			}
		}
	}

	return out, nil
}
