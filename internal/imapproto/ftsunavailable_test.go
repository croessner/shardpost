package imapproto

import (
	"fmt"
	"strings"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// unavailableStore opens accounts whose full-text index answers
// model.ErrIndexUnavailable (a conversion holds it frozen, an object of
// its generation is missing).
type unavailableStore struct{ *memstore.Store }

func (s unavailableStore) User(name string) (model.User, error) {
	u, err := s.Store.User(name)
	if err != nil {
		return nil, err
	}

	return unavailableUser{u}, nil
}

type unavailableUser struct{ model.User }

func (u unavailableUser) SearchCandidates([]string, string) (map[string]bool, error) {
	return nil, fmt.Errorf("%w: object missing", model.ErrIndexUnavailable)
}

// TestSearchIndexUnavailable: a search whose index is temporarily
// unavailable answers NO [UNAVAILABLE], not a storage error (B14b §4.5).
func TestSearchIndexUnavailable(t *testing.T) {
	c, _ := newTestServer(t, func(o *Options, s *memstore.Store) { o.Store = unavailableStore{s} })
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")
	c.ok("SELECT INBOX")

	lines := c.cmd("SEARCH BODY hello")
	if last := lines[len(lines)-1]; !strings.Contains(last, "NO [UNAVAILABLE]") {
		t.Fatalf("got %q", lines)
	}
}
