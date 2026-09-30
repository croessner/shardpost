package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/migrate"
)

// stateOK is how a report line says that nothing is wrong.
const stateOK = "ok"

// exPartial is the exit code of a run in which some mailboxes (or the
// Sieve scripts) failed; a later run continues where it stopped.
const exPartial = 1

// migrateArgs are the flags of migrate beyond migrate.Options.
type migrateArgs struct {
	o                      migrate.Options
	sieve                  migrate.Endpoint
	pwFile, readers        string
	noPrune, noACL, asJSON bool
}

// migrateFlags defines the flags of migrate (defaults from the
// environment, so a sidecar can be configured without arguments).
func migrateFlags(fs *flag.FlagSet) *migrateArgs {
	a := &migrateArgs{}
	o := &a.o

	fs.StringVar(&o.IMAP.Addr, "source-addr", os.Getenv("SHARDPOST_MIGRATE_SOURCE_ADDR"), "source IMAP host:port")
	fs.StringVar(&o.IMAP.TLS, "source-tls", envOr("SHARDPOST_MIGRATE_SOURCE_TLS", migrate.TLSImplicit), "source TLS: implicit, starttls or none")
	fs.StringVar(&o.IMAP.ServerName, "source-server-name", os.Getenv("SHARDPOST_MIGRATE_SOURCE_SERVER_NAME"), "expected certificate name (default: host of -source-addr)")
	fs.StringVar(&o.IMAP.CAFile, "source-ca", os.Getenv("SHARDPOST_MIGRATE_SOURCE_CA"), "CA file for the source and ManageSieve (default: system roots)")
	fs.BoolVar(&o.IMAP.Proxy, "source-proxy", envBool("SHARDPOST_MIGRATE_SOURCE_PROXY"), "send a PROXY v1 header to the source IMAP port")
	fs.DurationVar(&o.IMAP.Timeout, "source-timeout", 2*time.Minute, "timeout per source command")
	fs.StringVar(&o.SourceUser, "source-user", "", "login name at the source (default: the account)")
	fs.StringVar(&o.MasterUser, "master-user", os.Getenv("SHARDPOST_MIGRATE_MASTER_USER"), "master user at the source (empty: log in as the user)")
	fs.StringVar(&o.UserFormat, "user-format", envOr("SHARDPOST_MIGRATE_USER_FORMAT", "{user}*{master}"), "master login format")
	fs.StringVar(&a.pwFile, "password-file", os.Getenv("SHARDPOST_MIGRATE_PASSWORD_FILE"), "file with the (master) password")
	fs.StringVar(&a.sieve.Addr, "sieve-addr", os.Getenv("SHARDPOST_MIGRATE_SIEVE_ADDR"), "source ManageSieve host:port (empty: Sieve is not copied)")
	fs.StringVar(&a.sieve.TLS, "sieve-tls", envOr("SHARDPOST_MIGRATE_SIEVE_TLS", migrate.TLSStartTLS), "ManageSieve TLS: implicit, starttls or none")
	fs.StringVar(&a.sieve.ServerName, "sieve-server-name", os.Getenv("SHARDPOST_MIGRATE_SIEVE_SERVER_NAME"), "expected ManageSieve certificate name")
	fs.BoolVar(&a.sieve.Proxy, "sieve-proxy", envBool("SHARDPOST_MIGRATE_SIEVE_PROXY"), "send a PROXY v1 header to ManageSieve")
	fs.StringVar(&o.Namespace, "source-namespace", "", "import this namespace of the source (its prefix, such as \"Public/\") instead of the personal one")
	fs.StringVar(&o.TargetPrefix, "target-prefix", "", "with -source-namespace: put this in front of the mailbox names (\"\" or ending in \"/\")")
	fs.StringVar(&a.readers, "readers", "", "with the account ~public: comma-separated accounts whose own \\Seen and subscriptions are carried over")
	fs.StringVar(&o.Session, "session", "", "resume this import session")
	fs.BoolVar(&a.noPrune, "no-prune", false, "keep target mailboxes the source no longer has")
	fs.BoolVar(&a.noACL, "no-acl", false, "do not copy mailbox ACLs")
	fs.BoolVar(&o.Reimport, "reimport", false, "replace target mailboxes whose UIDVALIDITY no longer matches the source (their messages are imported again)")
	fs.StringVar(&o.POP3UIDLFormat, "pop3-uidl-format", os.Getenv("SHARDPOST_MIGRATE_POP3_UIDL_FORMAT"), "Dovecot pop3_uidl_format of the source (empty: POP3 UIDL is the GUID)")
	fs.IntVar(&o.BatchMessages, "batch-messages", 200, "messages per import request")
	fs.Int64Var(&o.BatchBytes, "batch-bytes", 32<<20, "bytes per import request")
	fs.BoolVar(&a.asJSON, "json", false, "print the report as JSON")

	return a
}

// migrateCmd copies an account from an IMAP source into ShardPost with
// its identities (UIDVALIDITY, UIDs, flags, MODSEQ). Every run is a
// delta; run it until it reports no changes, then once more under a
// director hold before switching the routing.
func migrateCmd(client *adminapi.ClientWithResponses, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)

	a := migrateFlags(fs)

	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || a.o.IMAP.Addr == "" || a.pwFile == "" {
		sayln(stderr, "usage: shardpostctl migrate -source-addr host:port -password-file file [-master-user name] [-sieve-addr host:port] [...] <account>")
		sayln(stderr, "       shardpostctl migrate [...] -source-user name -source-namespace prefix [-target-prefix name/] [-readers account,...] '~public'")

		return exUsage
	}

	pw, err := os.ReadFile(a.pwFile)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exUsage
	}

	o := a.o
	o.Account = fs.Arg(0)
	o.Password = strings.TrimRight(string(pw), "\r\n")
	o.Prune, o.ACL = !a.noPrune, !a.noACL
	o.Readers = readerList(a.readers)
	o.Log = slog.New(slog.NewTextHandler(stderr, nil))

	if a.sieve.Addr != "" {
		a.sieve.CAFile, a.sieve.Timeout = o.IMAP.CAFile, time.Minute
		o.Sieve = &a.sieve
	}

	if err := o.Check(); err != nil {
		sayln(stderr, "shardpostctl: migrate:", err)
		return exUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep, err := migrate.Run(ctx, o, &migrate.APITarget{C: client})
	if rep != nil {
		printReport(stdout, rep, a.asJSON)
	}

	switch {
	case err == nil:
		return exOK
	case errors.Is(err, migrate.ErrPartial):
		return exPartial
	default:
		sayln(stderr, "shardpostctl: migrate:", err)
		return exUnavailable
	}
}

// readerList splits the comma-separated accounts of -readers.
func readerList(s string) []string {
	var out []string

	for _, reader := range strings.Split(s, ",") {
		if reader = strings.TrimSpace(reader); reader != "" {
			out = append(out, reader)
		}
	}

	return out
}

// envBool is true when the variable is "true".
func envBool(name string) bool { return os.Getenv(name) == "true" }

// printReport prints the run report.
func printReport(w io.Writer, rep *migrate.Report, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)

		return
	}

	sayf(w, "account %s, session %s, %s\n", rep.Account, rep.Session, rep.Duration)

	for _, m := range rep.Mailboxes {
		state := "verified"
		if !m.Verified {
			state = "not verified"
		}

		if m.Error != "" {
			state = "FAILED: " + m.Error
		}

		sayf(w, "  %-40s uidvalidity %d uidnext %d modseq %d msgs %d new %d existing %d flags %d expunged %d  %s\n",
			m.Name, m.UIDValidity, m.UIDNext, m.HighestModSeq, m.Messages, m.Imported, m.Existing, m.FlagChanges, m.Expunged, state)
	}

	for _, rd := range rep.Readers {
		state := stateOK
		if rd.Error != "" {
			state = "FAILED: " + rd.Error
		}

		sayf(w, "  reader %-33s mailboxes %d subscribed %d seen changes %d  %s\n", rd.Account, rd.Mailboxes, rd.Subscribed, rd.Changed, state)
	}

	for _, p := range rep.Pruned {
		sayf(w, "  pruned %s\n", p)
	}

	if rep.Sieve != "" {
		sayf(w, "  sieve: %s\n", rep.Sieve)
	}

	for _, warn := range rep.Warnings {
		sayf(w, "  warning: %s\n", warn)
	}
}
