// Command shardpostctl is the client of the ShardPost admin API. It talks
// to the Unix socket inside the pod or, with a client certificate, to the
// mutual-TLS listener.
//
//	shardpostctl [connection flags] version
//	shardpostctl [connection flags] deliver -d <recipient> [-f <sender>] [-m <mailbox>] [--no-sieve] [--no-enforce-quota] < message
//	shardpostctl [connection flags] migrate [flags] <account>
//	shardpostctl [connection flags] trace on [-for 30m] | off | status <account>
//	shardpostctl [connection flags] server status | account show | mailbox list|show | acl list|explain | quota show | shared list
//
// deliver behaves like dovecot-lda and exits with sysexits codes: 0 ok,
// 64 usage, 67 unknown recipient or mailbox, 73 over quota, 75 temporary
// failure (try again), 77 not permitted, 69 rejected or too large, 70
// unexpected answer. migrate copies an account from an IMAP source with
// its UIDs (see migrate -h) and exits 0 when everything was verified, 1
// when parts failed, 64 on usage errors and 69 when the run failed. trace
// switches the protocol trace of an account (lines in the server log);
// the diagnosis commands only read (see diagnose.go).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"shardpost/internal/adminapi"
)

// sysexits codes (sysexits.h).
const (
	exOK          = 0
	exUsage       = 64
	exNoUser      = 67
	exUnavailable = 69
	exSoftware    = 70
	exCantCreate  = 73
	exTempFail    = 75
	exNoPerm      = 77
)

const defaultSocket = "/run/shardpost/admin.sock"

// Command nouns that also label output rows.
const (
	cmdVersion = "version"
	cmdTrace   = "trace"
	cmdAccount = "account"
	cmdQuota   = "quota"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

// connection are the global flags that select the listener.
type connection struct {
	socket, url, cert, key, ca string
	timeout                    time.Duration
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shardpostctl", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var c connection
	fs.StringVar(&c.socket, "socket", envOr("SHARDPOST_ADMIN_SOCKET", defaultSocket), "admin Unix socket")
	fs.StringVar(&c.url, "url", os.Getenv("SHARDPOST_ADMIN_URL"), "admin TLS URL (https://host:port); overrides -socket")
	fs.StringVar(&c.cert, "cert", os.Getenv("SHARDPOST_ADMIN_CERT"), "client certificate (with -url)")
	fs.StringVar(&c.key, "key", os.Getenv("SHARDPOST_ADMIN_KEY"), "client key (with -url)")
	fs.StringVar(&c.ca, "ca", os.Getenv("SHARDPOST_ADMIN_CA"), "server CA (with -url)")
	fs.DurationVar(&c.timeout, "timeout", 2*time.Minute, "request timeout")

	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		sayln(stderr, "usage: shardpostctl [flags] version | deliver -d <recipient> [...] < message | migrate [...] <account> | trace on|off|status <account> | server|account|mailbox|acl|quota|shared ...")
		return exUsage
	}

	client, err := c.client()
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exUsage
	}

	switch fs.Arg(0) {
	case cmdVersion:
		return version(client, stdout, stderr)
	case "deliver":
		return deliver(client, fs.Args()[1:], stdin, stdout, stderr)
	case "migrate":
		return migrateCmd(client, fs.Args()[1:], stdout, stderr)
	case cmdTrace:
		return traceCmd(client, fs.Args()[1:], stdout, stderr)
	case "server", cmdAccount, "mailbox", "acl", cmdQuota, "shared":
		return diagCmd(client, fs.Arg(0), fs.Args()[1:], stdout, stderr)
	default:
		sayln(stderr, "shardpostctl: unknown command", fs.Arg(0))
		return exUsage
	}
}

// client builds the API client for the selected listener.
func (c connection) client() (*adminapi.ClientWithResponses, error) {
	if c.url == "" {
		return adminapi.NewSocketClient(c.socket, c.timeout)
	}

	if c.cert == "" || c.key == "" || c.ca == "" {
		return nil, errors.New("-url needs -cert, -key and -ca")
	}

	return adminapi.NewTLSClient(c.url, c.cert, c.key, c.ca, c.timeout)
}

func version(client *adminapi.ClientWithResponses, stdout, stderr io.Writer) int {
	resp, err := client.GetVersionWithResponse(context.Background())
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exTempFail
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	sayf(stdout, "%s (%s)\n", resp.JSON200.Version, resp.JSON200.GoVersion)

	return exOK
}

func deliver(client *adminapi.ClientWithResponses, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("deliver", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var p adminapi.DeliverParams

	from := fs.String("f", "", "envelope sender")
	mailbox := fs.String("m", "", "store into this existing mailbox without Sieve")
	fs.StringVar(&p.Recipient, "d", "", "recipient (required)")
	noSieve := fs.Bool("no-sieve", false, "do not run the user's Sieve script")
	noEnforce := fs.Bool("no-enforce-quota", false, "store even over quota (needs role deliver:noenforce)")
	asJSON := fs.Bool("json", false, "print the result as JSON")

	if err := fs.Parse(args); err != nil || p.Recipient == "" || fs.NArg() != 0 {
		sayln(stderr, "usage: shardpostctl deliver -d <recipient> [-f <sender>] [-m <mailbox>] [--no-sieve] [--no-enforce-quota] [--json] < message")
		return exUsage
	}

	p.From = optional(*from)
	p.Mailbox = optional(*mailbox)

	if *noSieve {
		p.Sieve = new(bool)
	}

	if *noEnforce {
		p.EnforceQuota = new(bool)
	}

	resp, err := client.DeliverWithBodyWithResponse(context.Background(), &p, "message/rfc822", stdin)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exTempFail
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(resp.JSON200)
		return exOK
	}

	for _, st := range resp.JSON200.Stored {
		sayf(stdout, "%s: stored in %s uid %d\n", resp.JSON200.Account, st.Mailbox, st.Uid)
	}

	if len(resp.JSON200.Stored) == 0 {
		sayf(stdout, "%s: discarded by sieve\n", resp.JSON200.Account)
	}

	return exOK
}

// failure prints the error document and maps its code to an exit code.
func failure(stderr io.Writer, e *adminapi.Error, status int) int {
	if e == nil {
		sayln(stderr, "shardpostctl: unexpected answer, HTTP status", status)
		return exSoftware
	}

	sayf(stderr, "shardpostctl: %s: %s\n", e.Code, e.Message)

	switch e.Code {
	case adminapi.NoSuchUser, adminapi.NoSuchMailbox:
		return exNoUser
	case adminapi.OverQuota:
		return exCantCreate
	case adminapi.Temporary:
		return exTempFail
	case adminapi.Unauthenticated, adminapi.Forbidden:
		return exNoPerm
	case adminapi.BadRequest:
		return exUsage
	case adminapi.Rejected, adminapi.TooLarge:
		return exUnavailable
	default:
		return exSoftware
	}
}

// sayln and sayf write to the terminal; a failed write has nowhere
// to be reported.
func sayln(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }

func sayf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}

	return fallback
}
