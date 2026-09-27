package jmap

import (
	"context"
	"net/http"
	"testing"

	"shardpost/internal/auth"
)

// shardAuth is an authenticator whose result carries the shard attribute
// the backend assigns the account.
type shardAuth struct {
	auth.Static
	attrs map[string][]string
}

func (a shardAuth) Authenticate(ctx context.Context, r auth.Request) (*auth.Result, error) {
	res, err := a.Static.Authenticate(ctx, r)
	if err != nil {
		return nil, err
	}

	res.Attributes = a.attrs

	return res, nil
}

// TestShardGate: with a shard configured, only accounts the backend
// assigns to it are served; another shard or none is refused with 403,
// not 401, so a client does not ask for the password again. Without a
// shard configured every authenticated account is served.
func TestShardGate(t *testing.T) {
	cases := []struct {
		name  string
		gate  ShardGate
		attrs map[string][]string
		want  int
	}{
		{"no gate", ShardGate{}, nil, http.StatusOK},
		{"own shard", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {"shardpost"}}, http.StatusOK},
		{"own shard, other case", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {"ShardPost"}}, http.StatusOK},
		{"other shard", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {"mailstack"}}, http.StatusForbidden},
		{"no attribute", ShardGate{Attribute: "mailShard", Value: "shardpost"}, nil, http.StatusForbidden},
		{"empty value", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {""}}, http.StatusForbidden},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t, func(o *Options) {
				o.Auth = shardAuth{Static: auth.Static{"test": "secret"}, attrs: c.attrs}
				o.Shard = c.gate
			})

			if w := do(t, s, http.MethodGet, PathSession, ""); w.Code != c.want {
				t.Fatalf("session: %d, want %d", w.Code, c.want)
			}
		})
	}
}
