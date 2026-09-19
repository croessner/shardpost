package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"time"

	"shardpost/internal/adminapi"
)

// traceVerbOn is the only verb that takes -for.
const traceVerbOn = "on"

const traceUsage = "usage: shardpostctl trace on [-for <duration>] <account> | off <account> | status <account> [--json]"

// traceCmd switches the protocol trace of an account (role trace).
//
//	trace on [-for 30m] <account>   trace new sessions of the account
//	trace off <account>             stop tracing
//	trace status <account>          show the switch
//
// The traced lines are in the server log (event protocol_trace).
func traceCmd(client *adminapi.ClientWithResponses, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		sayln(stderr, traceUsage)
		return exUsage
	}

	verb := args[0]

	fs := flag.NewFlagSet("trace "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)

	dur := fs.Duration("for", 0, "how long the trace lasts (default: the server's default, capped at its maximum)")
	asJSON := fs.Bool("json", false, "print the status as JSON")

	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 || (*dur != 0 && verb != traceVerbOn) || *dur < 0 {
		sayln(stderr, traceUsage)
		return exUsage
	}

	switch account := fs.Arg(0); verb {
	case traceVerbOn:
		return traceOn(client, account, *dur, *asJSON, stdout, stderr)
	case "off":
		return traceOff(client, account, stdout, stderr)
	case "status":
		return traceStatus(client, account, *asJSON, stdout, stderr)
	default:
		sayln(stderr, traceUsage)
		return exUsage
	}
}

// traceOn switches the trace on (dur 0: the server's default).
func traceOn(client *adminapi.ClientWithResponses, account string, dur time.Duration, asJSON bool, stdout, stderr io.Writer) int {
	req := adminapi.TraceRequest{}

	if dur > 0 {
		secs := int(max(dur/time.Second, 1))
		req.DurationSeconds = &secs
	}

	resp, err := client.SetTraceWithResponse(context.Background(), account, req)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exTempFail
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printTrace(stdout, resp.JSON200, asJSON)
}

// traceOff switches the trace off.
func traceOff(client *adminapi.ClientWithResponses, account string, stdout, stderr io.Writer) int {
	resp, err := client.ClearTraceWithResponse(context.Background(), account)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exTempFail
	}

	if resp.StatusCode() != http.StatusNoContent {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	sayf(stdout, "%s: trace off\n", account)

	return exOK
}

// traceStatus prints the switch.
func traceStatus(client *adminapi.ClientWithResponses, account string, asJSON bool, stdout, stderr io.Writer) int {
	resp, err := client.GetTraceWithResponse(context.Background(), account)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exTempFail
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printTrace(stdout, resp.JSON200, asJSON)
}

// printTrace prints a trace status.
func printTrace(stdout io.Writer, st *adminapi.TraceStatus, asJSON bool) int {
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(st)
		return exOK
	}

	if !st.Active || st.Until == nil {
		sayf(stdout, "%s: trace off\n", st.Account)
		return exOK
	}

	by := ""
	if st.SetBy != nil {
		by = " (set by " + *st.SetBy + ")"
	}

	sayf(stdout, "%s: trace on until %s%s\n", st.Account, st.Until.UTC().Format(time.RFC3339), by)

	return exOK
}
