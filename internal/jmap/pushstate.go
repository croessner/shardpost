package jmap

import (
	"encoding/json"
	"strconv"

	"shardpost/internal/model"
)

// The state the push worker keeps per account (jmap/push/seen/<acct>),
// stage B18a: what the worker last saw of the account (seq, digest and
// the states of the shared accounts), and per subscription an
// acknowledgement (ack) of the position it was last delivered up to. A
// subscription has a backlog when its ack is behind seq or its digest
// differs; the types of the backlog are derived from the account's log,
// so nothing has to be stored for "what is still to be sent", and two
// writers merge by max instead of having to agree on a set. A counter
// that jumped back (the account was purged and created again) raises
// the epoch; a record of a higher epoch wins a merge whole.

// pushSeen is what the worker keeps per account of what it last pushed:
// the personal state, the states of the shared accounts and the share
// notifications with their digest, the acknowledgement of every
// subscription and the epoch. An older worker kept the personal state
// alone, as a bare number; Shared is nil then, and Ack and Epoch are
// absent.
type pushSeen struct {
	Seq    uint64                       `json:"seq"`
	Digest string                       `json:"digest,omitempty"`
	Shared map[string]map[string]string `json:"shared,omitempty"`
	Ack    map[string]pushAck           `json:"ack,omitempty"`
	Epoch  uint64                       `json:"epoch,omitempty"`
}

// pushAck is where one subscription stands: the account position and
// the digest of the shared states as of its last delivery.
type pushAck struct {
	Seq    uint64 `json:"seq"`
	Digest string `json:"digest,omitempty"`
}

// decodeSeen reads a stored state; a record an older worker wrote as a
// bare number is the personal state alone. ok is false for a record
// that is neither.
func decodeSeen(raw []byte) (pushSeen, bool) {
	var seen pushSeen

	if n, err := strconv.ParseUint(string(raw), 10, 64); err == nil {
		return pushSeen{Seq: n}, true
	}

	if json.Unmarshal(raw, &seen) != nil {
		return pushSeen{}, false
	}

	return seen, true
}

// behind reports whether the subscription id has a backlog: no ack, an
// ack behind the account, or one with another digest of the shared
// states.
func (s *pushSeen) behind(id string) bool {
	a, ok := s.Ack[id]

	return !ok || a.Seq < s.Seq || a.Digest != s.Digest
}

// mergeSeen merges the state a writer wants to store (mine, with the
// subscriptions it sees as alive in live) into the stored one (cur,
// nil: none) and reports whether the writer has to adopt the result
// instead of its own view (the stored record is of a higher epoch).
//
// The rules: the higher epoch wins whole; within an epoch seq is the
// maximum, digest and shared come from the side with the greater seq
// (the writer at a tie: startFollow does not trust the stored digest
// anyway, it diffs the stored shared states against the live ones), and
// per subscription the ack with the greater seq (the writer's at a
// tie); an ack present on one side alone stays. Acks of subscriptions
// the writer no longer sees alive are left out; another writer may put
// them back, which is harmless (nothing is sent without a
// subscription), and the next writer leaves them out again. Max is
// commutative and idempotent: two holders that overlap cost at most a
// duplicate, never a loss.
func mergeSeen(cur *pushSeen, mine pushSeen, live map[string]bool) (pushSeen, bool) {
	switch {
	case cur == nil || cur.Epoch < mine.Epoch:
		return mine, false
	case cur.Epoch > mine.Epoch:
		return *cur, true
	}

	out := mine
	if cur.Seq > mine.Seq {
		out.Seq, out.Digest, out.Shared = cur.Seq, cur.Digest, cur.Shared
	}

	out.Ack = mergeAcks(cur.Ack, mine.Ack, live)

	return out, false
}

// mergeAcks merges the acknowledgements of the stored record and the
// writer's: per subscription the one with the greater seq (the
// writer's at a tie), only for the subscriptions alive (nil: all).
func mergeAcks(cur, mine map[string]pushAck, live map[string]bool) map[string]pushAck {
	alive := func(id string) bool { return live == nil || live[id] }
	out := map[string]pushAck{}

	for id, a := range mine {
		if alive(id) {
			out[id] = a
		}
	}

	for id, a := range cur {
		if m, ok := out[id]; alive(id) && (!ok || a.Seq > m.Seq) {
			out[id] = a
		}
	}

	return out
}

// pushTypesFrom derives the backlog of a subscription from the account:
// the types that changed in the log from since (every type when the log
// no longer reaches back that far, as changedTypes does), up to the
// state the watcher stands at; and, when the digest of the shared
// states differs, every type of every shared account the watcher knows
// (coarse, but rare: a backoff across a change of a shared account).
func pushTypesFrom(u model.User, account string, since uint64, upto uint64, digestDiffers bool,
	shared map[string]map[string]string,
) map[string]map[string]string {
	changed := map[string]map[string]string{}
	own := accountID(account)

	for since < upto {
		types, next, err := changedTypes(u, since, nil)
		if err != nil || next == since {
			break
		}

		since = next

		for t := range types {
			if changed[own] == nil {
				changed[own] = map[string]string{}
			}

			changed[own][t] = stateString(upto)
		}
	}

	if digestDiffers {
		for id, ts := range shared {
			if id == own {
				continue
			}

			for t, s := range ts {
				if changed[id] == nil {
					changed[id] = map[string]string{}
				}

				changed[id][t] = s
			}
		}
	}

	return changed
}
