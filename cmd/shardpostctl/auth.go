package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"

	"shardpost/internal/adminapi"
)

// cmdAuth is the command noun of the auth cache commands.
const cmdAuth = "auth"

const authUsage = "usage: shardpostctl auth flush [--json] [<account>]"

// authCmd runs the auth cache commands (role repair).
//
//	auth flush [<account>]   drop the cached logins and lookups of an
//	                         account, or of every account
func authCmd(client *adminapi.ClientWithResponses, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "flush" {
		sayln(stderr, authUsage)
		return exUsage
	}

	fs := flag.NewFlagSet("auth flush", flag.ContinueOnError)
	fs.SetOutput(stderr)

	asJSON := fs.Bool("json", false, "print the answer as JSON")

	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 1 {
		sayln(stderr, authUsage)
		return exUsage
	}

	req := adminapi.AuthFlushRequest{Account: optional(fs.Arg(0))}

	resp, err := client.FlushAuthCacheWithResponse(context.Background(), req)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exTempFail
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	out := resp.JSON200

	switch {
	case *asJSON:
		_ = json.NewEncoder(stdout).Encode(out)
	case !out.Cached:
		sayf(stdout, "%s: this server has no auth cache, nothing flushed\n", out.Account)
	case out.Account == "*":
		sayln(stdout, "auth cache flushed (all accounts); the other servers follow within a minute")
	default:
		sayf(stdout, "%s: auth cache flushed; the other servers follow within a minute\n", out.Account)
	}

	return exOK
}
