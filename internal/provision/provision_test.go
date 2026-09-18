package provision

import (
	"io"
	"log/slog"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

func TestApplyLimit(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	u, err := memstore.New().User("u")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.SetQuotaLimit(model.Quota{Bytes: 1, Msgs: 500}); err != nil {
		t.Fatal(err)
	}

	Apply(u, &auth.Result{Account: "u", Attributes: map[string][]string{QuotaAttribute: {"2M"}}}, log)

	if l, _ := u.QuotaLimit(); l != (model.Quota{Bytes: 2 << 20, Msgs: 500}) {
		t.Fatalf("limit %+v, the message limit must survive", l)
	}

	Apply(u, &auth.Result{Account: "u", Attributes: map[string][]string{QuotaAttribute: {"lots"}}}, log)
	Apply(u, nil, log)

	if l, _ := u.QuotaLimit(); l.Bytes != 2<<20 {
		t.Fatalf("invalid attribute applied: %+v", l)
	}
}
