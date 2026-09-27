package jmap

import (
	"errors"
	"strings"

	"shardpost/internal/model"
)

// blobHandle is a blob a client named, ready to be read in ranges.
type blobHandle struct {
	read func(offset, length int64) ([]byte, error)
	id   string
	size int64
}

// blobCache opens the blobs one method call reads. A stored blob is
// looked up once and read in ranges. A body part exists only inside its
// message, and a script's content only in the script list, so those are
// read whole; the cache keeps the parsed messages, the part contents and
// the script list so that a call naming several parts of one message
// reads it once — but only up to a bound in bytes, the larger of
// maxSizeBlobGet and maxSizeBlobSet. Beyond it the oldest entry goes, and
// what is named again is read again; something larger than the bound on
// its own is read each time and not kept. The blob store is the
// account's own, so another account's blob is not found rather than
// refused.
type blobCache struct {
	sess    *session
	blobs   model.Blobs
	handles map[string]*blobHandle
	// roots are the parsed messages by GUID, contents the part and script
	// contents by blob id, scripts the script list; each is an entry of
	// order, oldest first, and counts its size in held.
	roots    map[string]*mimePart
	contents map[string][]byte
	scripts  map[string][]byte
	order    []cacheEntry
	held     int64
	bound    int64
}

// cacheEntry is one thing the cache keeps, and what it costs.
type cacheEntry struct {
	key  string
	size int64
}

// The kinds of cache entries, as the prefix of their keys.
const (
	cacheRoot    = "m:"
	cacheContent = "c:"
	cacheScripts = "s:"
)

// newBlobCache starts the cache of one call.
func newBlobCache(sess *session) (*blobCache, error) {
	blobs, ok := sess.user.(model.Blobs)
	if !ok {
		return nil, errNoIndex
	}

	return &blobCache{sess: sess, blobs: blobs, handles: map[string]*blobHandle{}, roots: map[string]*mimePart{},
		contents: map[string][]byte{}, bound: max(sess.limits.MaxSizeBlobGet, sess.limits.MaxSizeBlobSet)}, nil
}

// keep makes room for an entry and records it; it reports false, and
// keeps nothing, for one larger than the bound.
func (c *blobCache) keep(key string, size int64) bool {
	if size > c.bound {
		return false
	}

	for c.held+size > c.bound && len(c.order) > 0 {
		c.evict()
	}

	c.order = append(c.order, cacheEntry{key: key, size: size})
	c.held += size

	return true
}

// evict drops the oldest entry.
func (c *blobCache) evict() {
	e := c.order[0]
	c.order = c.order[1:]
	c.held -= e.size

	switch {
	case strings.HasPrefix(e.key, cacheRoot):
		delete(c.roots, e.key[len(cacheRoot):])
	case strings.HasPrefix(e.key, cacheContent):
		delete(c.contents, e.key[len(cacheContent):])
	default:
		c.scripts = nil
	}
}

// open looks up a blob the account can read — an upload, a message, a
// body part of one, a script's content — by its id or by the creation id
// of a Blob/upload earlier in the request.
func (c *blobCache) open(ref string) (*blobHandle, error) {
	id, pending := resolveCreationID(c.sess, ref)
	if pending {
		return nil, model.ErrNoBlob
	}

	if h := c.handles[id]; h != nil {
		return h, nil
	}

	if model.ValidGUID(id) {
		return c.stored(id)
	}

	content, ok := c.contents[id]
	if !ok {
		var err error

		switch guid, path, isPart := parsePartBlob(id); {
		case isPart:
			content, err = c.part(guid, path)
		case isScriptBlob(id):
			content, err = c.script(id)
		default:
			err = model.ErrNoBlob
		}

		if err != nil {
			return nil, err
		}

		if c.keep(cacheContent+id, int64(len(content))) {
			c.contents[id] = content
		}
	}

	return &blobHandle{id: id, size: int64(len(content)), read: func(off, n int64) ([]byte, error) {
		return content[off : off+n], nil
	}}, nil
}

// stored opens a blob of the store, which is read in ranges; the handle
// holds no content and is kept for the call.
func (c *blobCache) stored(id string) (*blobHandle, error) {
	size, err := c.blobs.BlobSize(id)
	if err != nil {
		return nil, err
	}

	h := &blobHandle{id: id, size: size, read: func(off, n int64) ([]byte, error) {
		return c.blobs.ReadBlob(id, size, off, n)
	}}
	c.handles[id] = h

	return h, nil
}

// part is the decoded content of a body part (see readPart), from the
// message as the cache keeps it or as it is read now.
func (c *blobCache) part(guid, path string) ([]byte, error) {
	root := c.roots[guid]
	if root == nil {
		size, err := c.blobs.BlobSize(guid)
		if err != nil {
			return nil, err
		}

		raw, err := c.blobs.ReadBlob(guid, size, 0, -1)
		if err != nil {
			return nil, err
		}

		if root, err = parseMessage(raw); err != nil {
			return nil, err
		}

		if c.keep(cacheRoot+guid, size) {
			c.roots[guid] = root
		}
	}

	var body []byte

	for i, section := range strings.Split(path, "_") {
		if i > 0 {
			var err error
			if root, err = parseMessage(body); err != nil {
				return nil, err
			}
		}

		part := root.find(section)
		if part == nil {
			return nil, model.ErrNoBlob
		}

		body, _ = part.decodedBody()
	}

	return body, nil
}

// script is the content of a script by its blob id, from the script list
// as the cache keeps it or as it is read now.
func (c *blobCache) script(id string) ([]byte, error) {
	scripts := c.scripts
	if scripts == nil {
		names, _, err := c.sess.user.ListScripts()
		if err != nil {
			return nil, err
		}

		scripts = map[string][]byte{}

		var size int64

		for _, name := range names {
			text, err := c.sess.user.GetScript(name)
			if errors.Is(err, model.ErrNoScript) {
				continue
			}

			if err != nil {
				return nil, err
			}

			scripts[scriptBlobID(text)] = []byte(text)
			size += int64(len(text))
		}

		if c.keep(cacheScripts, size) {
			c.scripts = scripts
		}
	}

	if text, ok := scripts[id]; ok {
		return text, nil
	}

	return nil, model.ErrNoBlob
}
