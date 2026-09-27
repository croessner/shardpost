package jmap

import (
	"encoding/json"
	"net/http"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// quotaServer is a front-end whose account has the given limits, with
// quota warnings at 80 % of the storage and 90 % of the messages.
func quotaServer(t *testing.T, limit model.Quota) (*Server, model.User) {
	t.Helper()

	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.SetQuotaLimit(limit); err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Quota = QuotaOptions{StorageWarnPercent: 80, MessagesWarnPercent: 90}
	})

	return s, u
}

// quotaCall is callWith with the quota capability.
func quotaCall(t *testing.T, s *Server, name, args string) map[string]any {
	t.Helper()

	return callWith(t, s, CapQuota, name, args)
}

// quotaError sends one call with the quota capability and returns the
// error type it gave.
func quotaError(t *testing.T, s *Server, name, args string) string {
	t.Helper()

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:quota"],
		"methodCalls":[["`+name+`",`+args+`,"c1"]]}`)

	got, _ := responses(t, out)[0].([]any)
	if got[0] != kwError {
		t.Fatalf("%s answered %v, want an error: %#v", name, got[0], got[1])
	}

	typ, _ := got[1].(map[string]any)["type"].(string)

	return typ
}

// TestQuotaGet: one object per limited resource, with the usage, the
// limit and the warning of the configuration.
func TestQuotaGet(t *testing.T) {
	s, u := quotaServer(t, model.Quota{Bytes: 10000, Msgs: 10})
	deliver(t, u, "INBOX", "Subject: a\r\n\r\nhello\r\n")

	got := quotaObjects(t, s)
	octets, count := got[quotaOctets], got[quotaCount]

	if octets == nil || count == nil || len(got) != 2 {
		t.Fatalf("quotas %#v", got)
	}

	hasValues(t, "octets", octets, map[string]any{propResourceType: "octets", propHardLimit: float64(10000),
		propWarnLimit: float64(8000), propScope: "account", propName: "Mail storage"})
	hasValues(t, "count", count, map[string]any{propUsed: float64(1), propHardLimit: float64(10), propWarnLimit: float64(9)})

	if used, _ := octets[propUsed].(float64); used <= 0 {
		t.Errorf("octets used %#v", octets[propUsed])
	}

	if !idsAre(count[propTypes], "Email") {
		t.Errorf("types %#v", count[propTypes])
	}

	if _, ok := count[propSoftLimit]; ok {
		t.Errorf("a softLimit this server does not have: %#v", count)
	}
}

// quotaObjects is Quota/get of everything, keyed by id.
func quotaObjects(t *testing.T, s *Server) map[string]map[string]any {
	t.Helper()

	res := quotaCall(t, s, "Quota/get", `{"accountId":"test"}`)
	got := map[string]map[string]any{}

	for _, o := range res["list"].([]any) {
		obj := o.(map[string]any)
		got[obj[propID].(string)] = obj
	}

	return got
}

// TestQuotaGetProperties: ids, properties and what is not there.
func TestQuotaGetProperties(t *testing.T) {
	s, _ := quotaServer(t, model.Quota{Bytes: 10000, Msgs: 10})

	res := quotaCall(t, s, "Quota/get", `{"accountId":"test","ids":["count","storage"],"properties":["used"]}`)
	if nf, _ := res["notFound"].([]any); len(nf) != 1 || nf[0] != "storage" {
		t.Errorf("notFound %#v", res["notFound"])
	}

	if list := res["list"].([]any); len(list) != 1 || len(list[0].(map[string]any)) != 2 {
		t.Errorf("properties %#v", res["list"])
	}

	if got := quotaError(t, s, "Quota/get", `{"accountId":"test","properties":["size"]}`); got != errInvalidArguments {
		t.Errorf("an unknown property gave %s", got)
	}
}

// TestQuotaUnlimited: a resource without a limit has no object, and an
// account without limits has none at all.
func TestQuotaUnlimited(t *testing.T) {
	s, _ := quotaServer(t, model.Quota{Msgs: 10})

	res := quotaCall(t, s, "Quota/get", `{"accountId":"test"}`)
	if list := res["list"].([]any); len(list) != 1 || list[0].(map[string]any)[propID] != quotaCount {
		t.Errorf("with a message limit only %#v", list)
	}

	s, _ = quotaServer(t, model.Quota{})

	res = quotaCall(t, s, "Quota/get", `{"accountId":"test"}`)
	if list := res["list"].([]any); len(list) != 0 {
		t.Errorf("without limits %#v", list)
	}
}

// TestQuotaChanges: the delta between two states. A usage that moved is
// an update of used alone.
func TestQuotaChanges(t *testing.T) {
	s, u := quotaServer(t, model.Quota{Bytes: 10000, Msgs: 10})

	state := quotaCall(t, s, "Quota/get", `{"accountId":"test"}`)["state"].(string)

	res := quotaCall(t, s, "Quota/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	if res["newState"] != state || len(res["updated"].([]any)) != 0 || res["updatedProperties"] != nil {
		t.Errorf("nothing changed %#v", res)
	}

	deliver(t, u, "INBOX", "Subject: a\r\n\r\nhello\r\n")

	res = quotaCall(t, s, "Quota/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	if upd := res["updated"].([]any); len(upd) != 2 {
		t.Fatalf("a delivery %#v", res)
	}

	if props, _ := res["updatedProperties"].([]any); len(props) != 1 || props[0] != propUsed {
		t.Errorf("updatedProperties %#v", res["updatedProperties"])
	}
}

// TestQuotaChangesLimits: a limit that went is a destruction, one that
// changed an update of more than used, a new one a creation; more changes
// than maxChanges come in steps that end at the state of now.
func TestQuotaChangesLimits(t *testing.T) {
	s, u := quotaServer(t, model.Quota{Bytes: 10000, Msgs: 10})

	state := quotaCall(t, s, "Quota/get", `{"accountId":"test"}`)["state"].(string)

	if err := u.SetQuotaLimit(model.Quota{Bytes: 20000}); err != nil {
		t.Fatal(err)
	}

	res := quotaCall(t, s, "Quota/changes", `{"accountId":"test","sinceState":"`+state+`","maxChanges":1}`)
	if !idsAre(res["destroyed"], quotaCount) || res["hasMoreChanges"] != true {
		t.Fatalf("first step %#v", res)
	}

	res = quotaCall(t, s, "Quota/changes", `{"accountId":"test","sinceState":"`+res["newState"].(string)+`"}`)
	if !idsAre(res["updated"], quotaOctets) || res["updatedProperties"] != nil || res["hasMoreChanges"] != false {
		t.Fatalf("second step %#v", res)
	}

	now := quotaCall(t, s, "Quota/get", `{"accountId":"test"}`)["state"]
	if res["newState"] != now {
		t.Errorf("the steps end at %v, the state is %v", res["newState"], now)
	}

	if err := u.SetQuotaLimit(model.Quota{Bytes: 20000, Msgs: 5}); err != nil {
		t.Fatal(err)
	}

	res = quotaCall(t, s, "Quota/changes", `{"accountId":"test","sinceState":"`+now.(string)+`"}`)
	if !idsAre(res["created"], quotaCount) {
		t.Errorf("a new limit %#v", res)
	}
}

// idsAre reports whether a decoded id list is exactly the ids.
func idsAre(v any, ids ...string) bool {
	list, _ := v.([]any)
	if len(list) != len(ids) {
		return false
	}

	for i, id := range ids {
		if list[i] != id {
			return false
		}
	}

	return true
}

// hasValues fails the test for every property of want the object does
// not have with that value.
func hasValues(t *testing.T, what string, obj map[string]any, want map[string]any) {
	t.Helper()

	for k, v := range want {
		if obj[k] != v {
			t.Errorf("%s: %s is %#v, want %#v", what, k, obj[k], v)
		}
	}
}

// TestQuotaChangesRefuses: a state this server did not write, and a
// maxChanges of zero.
func TestQuotaChangesRefuses(t *testing.T) {
	s, _ := quotaServer(t, model.Quota{Msgs: 10})

	for _, bad := range []string{"", "7", "q1-x1.2.3", "q1-c1.2", "q1-c1.2.3-c1.2.3", "q2", "q1-c-1.2.3", "q1-", "q1-c01.2.3", "q1-c1.02.3", "q1-c1.2.+3"} {
		if got := quotaError(t, s, "Quota/changes", `{"accountId":"test","sinceState":"`+bad+`"}`); got != errCannotCalculateChanges {
			t.Errorf("state %q gave %s", bad, got)
		}
	}

	if got := quotaError(t, s, "Quota/changes", `{"accountId":"test","sinceState":"q1","maxChanges":0}`); got != errInvalidArguments {
		t.Errorf("maxChanges 0 gave %s", got)
	}
}

// TestQuotaQuery: the filter conditions, the operators and the sort.
func TestQuotaQuery(t *testing.T) {
	s, u := quotaServer(t, model.Quota{Bytes: 10000, Msgs: 10})
	deliver(t, u, "INBOX", "Subject: a\r\n\r\nhello\r\n")

	query := func(args string) []string {
		t.Helper()

		res := quotaCall(t, s, "Quota/query", args)

		var out []string
		for _, id := range res["ids"].([]any) {
			out = append(out, id.(string))
		}

		return out
	}

	cases := []struct {
		args string
		want []string
	}{
		{`{"accountId":"test"}`, []string{quotaCount, quotaOctets}},
		{`{"accountId":"test","filter":{"name":"STORAGE"}}`, []string{quotaOctets}},
		{`{"accountId":"test","filter":{"resourceType":"count"}}`, []string{quotaCount}},
		{`{"accountId":"test","filter":{"scope":"domain"}}`, nil},
		{`{"accountId":"test","filter":{"type":"Email"}}`, []string{quotaCount, quotaOctets}},
		{`{"accountId":"test","filter":{"type":"Mail"}}`, []string{quotaCount, quotaOctets}},
		{`{"accountId":"test","filter":{"type":"Calendar"}}`, nil},
		{`{"accountId":"test","filter":{"operator":"NOT","conditions":[{"resourceType":"count"}]}}`, []string{quotaOctets}},
		{`{"accountId":"test","filter":{"operator":"OR","conditions":[{"name":"messages"},{"name":"storage"}]}}`,
			[]string{quotaCount, quotaOctets}},
		{`{"accountId":"test","sort":[{"property":"used","isAscending":false}]}`, []string{quotaOctets, quotaCount}},
		{`{"accountId":"test","sort":[{"property":"name","isAscending":false}]}`, []string{quotaOctets, quotaCount}},
		{`{"accountId":"test","sort":[{"property":"used"}],"position":1}`, []string{quotaOctets}},
	}

	for _, c := range cases {
		if got := query(c.args); len(got) != len(c.want) || (len(got) > 0 && (got[0] != c.want[0] || got[len(got)-1] != c.want[len(c.want)-1])) {
			t.Errorf("%s gave %v, want %v", c.args, got, c.want)
		}
	}

	refused := []struct{ args, want string }{
		{`{"accountId":"test","filter":{"used":1}}`, errUnsupportedFilter},
		{`{"accountId":"test","filter":{"operator":"XOR","conditions":[]}}`, errUnsupportedFilter},
		{`{"accountId":"test","filter":{"operator":"AND","conditions":[{"hardLimit":1}]}}`, errUnsupportedFilter},
		{`{"accountId":"test","sort":[{"property":"hardLimit"}]}`, errUnsupportedSort},
		{`{"accountId":"test","sort":[{"property":"name","collation":"i;unicode-casemap"}]}`, errUnsupportedSort},
		{`{"accountId":"test","sinceQueryState":"q1"}`, errCannotCalculateChanges},
	}

	for i, c := range refused {
		name := "Quota/query"
		if i == len(refused)-1 {
			name = "Quota/queryChanges"
		}

		if got := quotaError(t, s, name, c.args); got != c.want {
			t.Errorf("%s gave %s, want %s", c.args, got, c.want)
		}
	}
}

// TestQuotaCapability: the capability is in the session and in the
// account, and a method is only reachable through it.
func TestQuotaCapability(t *testing.T) {
	s, _ := quotaServer(t, model.Quota{Msgs: 10})

	w := do(t, s, http.MethodGet, PathSession, "")

	var got Session
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if string(got.Capabilities[CapQuota]) != `{}` || string(got.Accounts["test"].AccountCapabilities[CapQuota]) != `{}` ||
		got.PrimaryAccounts[CapQuota] != "test" {
		t.Errorf("session %#v", got)
	}

	if got := mailError(t, s, "Quota/get", `{"accountId":"test"}`); got != errUnknownMethod {
		t.Errorf("Quota/get without the quota capability gave %s", got)
	}
}

// TestQuotaState: a state token reads back as the values it was written
// from.
func TestQuotaState(t *testing.T) {
	list := []*quotaObject{{ID: quotaCount, Used: 1, HardLimit: 2}, {ID: quotaOctets, Used: 3, HardLimit: 4, WarnLimit: 5}}

	back, ok := parseQuotaState(quotaState(list))
	if !ok || len(back) != 2 || *back[quotaCount] != *list[0] || *back[quotaOctets] != *list[1] {
		t.Errorf("%s read back as %v", quotaState(list), back)
	}

	for _, token := range []string{"q1", "q1-c0.10.0", "q1-c1.2.0-o3.4.5"} {
		m, ok := parseQuotaState(token)
		if !ok || quotaState(inOrder(m)) != token {
			t.Errorf("%s does not round-trip", token)
		}
	}

	if w := warnLimit(1<<62, 80); w != 3689348814741910323 {
		t.Errorf("warnLimit of a large limit %d", w)
	}
}
