package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

type (
	markCall   func(context.Context, ...adminapi.RequestEditorFn) (*http.Response, error)
	markEnable func(context.Context, adminapi.FTSLayout3Enable, ...adminapi.RequestEditorFn) (*http.Response, error)
)

// stageMark are the raw calls of one of the marks of stage B16/B17.
type stageMark struct {
	key, capability string
	get, disable    markCall
	enable          markEnable
}

// stageMarks are the two marks over the fixture's client.
func stageMarks(fx *layoutFixture) []stageMark {
	c := fx.c.ClientInterface

	return []stageMark{
		{key: model.QuotaOnlineMarkKey, capability: model.CapQuotaOnlineRecount, get: c.GetQuotaOnline, enable: c.EnableQuotaOnline, disable: c.DisableQuotaOnline},
		{key: model.MailboxLockMarkKey, capability: model.CapMailboxLock, get: c.GetMailboxLock, enable: c.EnableMailboxLock, disable: c.DisableMailboxLock},
	}
}

// markResult is a decoded mark response: the status on 200, the error
// message otherwise.
type markResult struct {
	st   *adminapi.FTSLayout3Status
	msg  string
	code int
	err  error
}

// decodeMark decodes a raw mark response.
func decodeMark(rsp *http.Response, err error) markResult {
	if err != nil {
		return markResult{err: err}
	}

	defer func() { _ = rsp.Body.Close() }()

	if rsp.StatusCode != http.StatusOK {
		var e adminapi.Error

		_ = json.NewDecoder(rsp.Body).Decode(&e)

		return markResult{msg: e.Message, code: rsp.StatusCode}
	}

	var st adminapi.FTSLayout3Status

	return markResult{st: &st, code: rsp.StatusCode, err: json.NewDecoder(rsp.Body).Decode(&st)}
}

// enabled reports whether the result says enabled (false for none).
func (r markResult) enabled(t *testing.T) bool {
	t.Helper()

	if r.err != nil {
		t.Fatal(r.err)
	}

	return r.st != nil && r.st.Enabled
}

// TestStageMarksEnableDisable: the marks of online quota recounts and
// of the mailbox lock are set when exactly --pods fresh registrations
// report their capability, and removed by disable; neither touches the
// other marks.
func TestStageMarksEnableDisable(t *testing.T) {
	fx := serveLayout(t, Options{}, RoleDiagnose, jobs.RoleRepair)
	ctx := context.Background()

	for _, pod := range []string{"pod-a", "pod-b"} {
		if err := jobs.RegisterServer(fx.kv, pod, "0.52.0", time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	marks := stageMarks(fx)

	for _, m := range marks {
		t.Run(m.key+" enable", func(t *testing.T) { enableMark(ctx, t, m) })
	}

	marksSet(t, fx, map[string]bool{model.QuotaOnlineMarkKey: true, model.MailboxLockMarkKey: true, model.FTSOnlineMarkKey: false,
		model.FTSLayoutMarkKey: false})

	for i, m := range marks {
		t.Run(m.key+" disable", func(t *testing.T) {
			if decodeMark(m.disable(ctx)).enabled(t) {
				t.Fatal("still enabled after the disable")
			}

			if other := marks[1-i]; i == 0 && !decodeMark(other.get(ctx)).enabled(t) {
				t.Fatal("the disable touched the other mark")
			}
		})
	}

	marksSet(t, fx, map[string]bool{model.QuotaOnlineMarkKey: false, model.MailboxLockMarkKey: false})
}

// enableMark: the status before, a refused count, then the enable.
func enableMark(ctx context.Context, t *testing.T, m stageMark) {
	t.Helper()

	if r := decodeMark(m.get(ctx)); r.enabled(t) || r.st == nil || len(r.st.Servers) != 2 {
		t.Fatalf("status %+v", r.st)
	}

	if r := decodeMark(m.enable(ctx, adminapi.FTSLayout3Enable{Pods: 3})); r.code != http.StatusConflict || !strings.Contains(r.msg, m.capability) {
		t.Fatalf("enable with the wrong count: %d %q", r.code, r.msg)
	}

	if r := decodeMark(m.enable(ctx, adminapi.FTSLayout3Enable{Pods: 2})); !r.enabled(t) {
		t.Fatalf("enable: %d %+v", r.code, r.st)
	}
}
