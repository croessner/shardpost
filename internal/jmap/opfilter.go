package jmap

import "encoding/json"

// The filters of the small /query methods — Quota, SieveScript — are
// trees of the same shape: a node is a condition, or an operator over
// further nodes (RFC 8620 5.5). The objects are few and filtered in
// memory; what differs between the types is only the condition.

// The two properties of a FilterOperator.
const (
	filterOperator   = "operator"
	filterConditions = "conditions"
)

// opFilter is one node of such a filter.
type opFilter interface {
	// operator is "AND", "OR" or "NOT", and "" for a condition.
	operator() string
	// conditionCount is the number of nodes below an operator.
	conditionCount() int
}

// decodeOpFilter decodes one filter node into f and the nodes below it,
// which child reaches. A property the node may not have (keys names the
// ones it may), or an operator that does not exist, is unsupportedFilter:
// a condition this server ignored would widen the result behind the
// client's back. what names the type in the error ("a quota").
func decodeOpFilter[F opFilter](raw json.RawMessage, f F, keys map[string]bool, what string, child func(F, int) F) *methodError {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return &methodError{Type: errInvalidArguments, Arguments: []string{argFilter}, Description: err.Error()}
	}

	for k := range given {
		if !keys[k] {
			return &methodError{Type: errUnsupportedFilter, Description: what + " cannot be filtered by " + k}
		}
	}

	if err := json.Unmarshal(raw, f); err != nil {
		return &methodError{Type: errInvalidArguments, Arguments: []string{argFilter}, Description: err.Error()}
	}

	if f.operator() == "" {
		if f.conditionCount() > 0 {
			return &methodError{Type: errUnsupportedFilter, Description: "conditions need an operator"}
		}

		return nil
	}

	switch f.operator() {
	case opAND, opOR, opNOT:
	default:
		return &methodError{Type: errUnsupportedFilter, Description: "unknown operator " + f.operator()}
	}

	var subs []json.RawMessage
	if err := json.Unmarshal(given[filterConditions], &subs); err != nil {
		return &methodError{Type: errInvalidArguments, Arguments: []string{argFilter}, Description: "an operator needs conditions"}
	}

	for i, sub := range subs {
		if merr := decodeOpFilter(sub, child(f, i), keys, what, child); merr != nil {
			return merr
		}
	}

	return nil
}

// matchOpFilter evaluates a filter node: a condition by cond, an
// operator over the nodes below it. AND is decided by the first node
// that fails, OR and NOT by the first that holds.
func matchOpFilter[F opFilter](f F, cond func(F) bool, child func(F, int) F) bool {
	if f.operator() == "" {
		return cond(f)
	}

	decides := f.operator() != opAND

	for i := range f.conditionCount() {
		if matchOpFilter(child(f, i), cond, child) == decides {
			return f.operator() == opOR
		}
	}

	return f.operator() != opOR
}

// checkSort refuses a sort by a property keys does not name, and a
// collation this server does not have.
func checkSort(by []comparator, keys map[string]bool) *methodError {
	for _, c := range by {
		if !keys[c.Property] {
			return &methodError{Type: errUnsupportedSort, Description: "cannot sort by " + c.Property}
		}

		if !validCollation(c.Collation) {
			return &methodError{Type: errUnsupportedSort, Description: "cannot sort with " + c.Collation}
		}
	}

	return nil
}
