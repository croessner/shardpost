package jmap

import (
	"context"
	"encoding/json"
	"sort"
)

// threadProperties are the properties of a Thread (RFC 8621 3).
var threadProperties = map[string]bool{propID: true, "emailIds": true}

// threadGet implements Thread/get (RFC 8621 3.1). A thread is the
// messages that share a conversation root; its emailIds are sorted by
// arrival, oldest first.
func threadGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if merr := checkRequested(req.Properties, threadProperties); merr != nil {
		return nil, merr
	}

	if req.IDs == nil || len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge,
			Description: "Thread/get needs ids, at most maxObjectsInGet of them"}
	}

	state, loc, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	members, err := loc.threadMembers(*req.IDs)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	out := &getResponse{AccountID: sess.accountID(), State: state, List: []map[string]any{}, NotFound: []string{}}

	for _, id := range *req.IDs {
		list := members[id]
		if len(list) == 0 {
			out.NotFound = append(out.NotFound, id)
			continue
		}

		obj := map[string]any{propID: id}
		if selected(req.Properties, "emailIds") {
			obj["emailIds"] = byArrival(list)
		}

		out.List = append(out.List, obj)
	}

	return out, nil
}

// byArrival is the ids of a thread's messages, oldest first; messages
// that arrived in the same second keep a stable order by id.
func byArrival(list []member) []string {
	sort.Slice(list, func(i, j int) bool {
		if list[i].received != list[j].received {
			return list[i].received < list[j].received
		}

		return list[i].guid < list[j].guid
	})

	out := make([]string, len(list))
	for i, m := range list {
		out[i] = m.guid
	}

	return out
}

// threadChanges implements Thread/changes (RFC 8621 3.2). A thread
// changes when a message joins or leaves it; a message whose keywords or
// mailboxes change stays in its thread, which does not change.
func threadChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	w, merr := readWindow(sess, args)
	if merr != nil {
		return nil, merr
	}

	threads, err := w.threadMoves()
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	members, err := w.loc.threadMembers(keys(threads))
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	out := w.response(sess)

	for t, net := range threads {
		now := len(members[t])
		out.add(t, classOf(max(now-net, 0), now))
	}

	out.sorted()

	return out.changesResponse, nil
}

// threadMoves counts, per thread, the messages that joined it minus
// those that left it inside the window.
func (w *window) threadMoves() (map[string]int, error) {
	var moved []string

	for guid, c := range w.classes {
		if c == classCreated || c == classDestroyed {
			moved = append(moved, guid)
		}
	}

	recs, err := w.loc.emailRecs(moved)
	if err != nil {
		return nil, err
	}

	out := map[string]int{}

	for _, guid := range moved {
		rec, ok := recs[guid]
		if !ok || rec.Thread == "" {
			// A message the locator never described came and went before
			// it could be; its thread did not change because of it.
			continue
		}

		if w.classes[guid] == classCreated {
			out[rec.Thread]++
		} else {
			out[rec.Thread]--
		}
	}

	return out, nil
}

// keys is the keys of a map.
func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
