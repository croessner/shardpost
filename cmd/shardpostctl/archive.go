package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/archive"
	"shardpost/internal/migrate"
)

// Account export and restore (roadmap 8e):
//
//	export -o file.tar <account>                               role export; the archive is verified after writing
//	archive verify <file.tar>                                  offline check (checksums, manifest)
//	restore [-account target] [-acl] [-activate-sieve] <file.tar>   role import
//
// restore verifies the whole archive first and imports only an intact
// one. Into another account (an isolated restore test) ACLs are not
// restored and no Sieve script is activated unless -acl or
// -activate-sieve say so. Exit codes: 0 ok, 1 partly failed (restore:
// run it again), 65 damaged or incomplete archive, 64 usage, 69 failed.

// Command nouns of export and restore.
const (
	cmdExport  = "export"
	cmdArchive = "archive"
	cmdRestore = "restore"
)

// archiveCmd dispatches export, archive verify and restore.
func archiveCmd(client *adminapi.ClientWithResponses, noun string, args []string, idle time.Duration, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(noun, flag.ContinueOnError)
	fs.SetOutput(stderr)

	out := fs.String("o", "", "export: archive file (required)")
	account := fs.String("account", "", "restore: target account (default: the exported one)")
	acl := fs.Bool("acl", false, "restore: restore ACLs into another account too")
	activate := fs.Bool("activate-sieve", false, "restore: activate the Sieve script in another account too")
	asJSON := fs.Bool("json", false, "print the report as JSON")

	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return exUsage
	}

	switch {
	case noun == cmdExport && len(rest) == 1 && *out != "":
		return exportAccount(client, rest[0], *out, idle, stdout, stderr)
	case noun == cmdArchive && len(rest) == 2 && rest[0] == "verify":
		return verifyArchive(rest[1], *asJSON, stdout, stderr)
	case noun == cmdRestore && len(rest) == 1:
		o := migrate.RestoreOptions{Account: *account, ACL: *acl, ActivateSieve: *activate, Log: slog.New(slog.NewTextHandler(stderr, nil))}
		return restoreAccount(client, rest[0], o, *asJSON, stdout, stderr)
	default:
		sayln(stderr, "usage: shardpostctl export -o file.tar <account> | archive verify <file.tar> | restore [-account A] [-acl] [-activate-sieve] <file.tar>")
		return exUsage
	}
}

// exportAccount downloads the archive into a file and verifies it. The
// download has no overall timeout; it stops when no data arrives for
// idle. A failed download leaves no file behind.
func exportAccount(client *adminapi.ClientWithResponses, account, path string, idle time.Duration, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := client.ExportAccount(ctx, account)
	if err != nil {
		return transportError(stderr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		parsed, _ := adminapi.ParseExportAccountResponse(resp)
		return failure(stderr, parsed.JSONDefault, resp.StatusCode)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exCantCreate
	}

	_, err = io.Copy(f, &idleReader{r: resp.Body, timer: time.AfterFunc(idle, cancel), idle: idle})
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		_ = os.Remove(path)

		sayln(stderr, "shardpostctl: export:", err)

		return exUnavailable
	}

	return verifyArchive(path, false, stdout, stderr)
}

// idleReader restarts the idle timer with every read that returns data.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}

	return n, err
}

// archiveSummary is what verify reports.
type archiveSummary struct {
	Account   string           `json:"account"`
	Created   string           `json:"created"`
	Server    string           `json:"server_version"`
	Manifest  archive.Manifest `json:"manifest"`
	Mailboxes []string         `json:"mailboxes"`
}

// verifyArchive reads the whole archive and checks every checksum.
func verifyArchive(path string, asJSON bool, stdout, stderr io.Writer) int {
	sum, err := readArchive(path)
	if err != nil {
		sayln(stderr, "shardpostctl:", path+":", err)

		if errors.Is(err, archive.ErrChecksum) || errors.Is(err, archive.ErrIncomplete) || errors.Is(err, archive.ErrFormat) {
			return exDataErr
		}

		return exUnavailable
	}

	if asJSON {
		sum.Manifest.Files = nil
		return printJSON(stdout, sum)
	}

	m := sum.Manifest
	sayf(stdout, "%s: account %s, exported %s by %s\n", path, sum.Account, sum.Created, sum.Server)
	sayf(stdout, "verified: %d mailboxes, %d messages, %d bytes, %d files, active sieve %q\n", m.Mailboxes, m.Messages, m.Bytes,
		len(m.Files), m.ActiveSieve)

	if len(m.Skipped)+len(m.SkippedMailboxes) > 0 {
		sayf(stdout, "skipped during export (deleted meanwhile): messages %v, mailboxes %v\n", m.Skipped, m.SkippedMailboxes)
	}

	return exOK
}

func readArchive(path string) (*archiveSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	r, err := archive.NewReader(f)
	if err != nil {
		return nil, err
	}

	sum := &archiveSummary{Account: r.Header.Account, Created: r.Header.Created.Format("2006-01-02 15:04:05 MST"), Server: r.Header.ServerVersion}

	for {
		e, err := r.Next()
		if errors.Is(err, io.EOF) {
			return sum, nil
		}

		if err != nil {
			return nil, err
		}

		switch e.Kind {
		case archive.KindMailbox:
			sum.Mailboxes = append(sum.Mailboxes, e.Mailbox.Name)
		case archive.KindManifest:
			sum.Manifest = *e.Manifest
		}
	}
}

// restoreAccount verifies the archive, then imports it.
func restoreAccount(client *adminapi.ClientWithResponses, path string, o migrate.RestoreOptions, asJSON bool, stdout, stderr io.Writer) int {
	if _, err := readArchive(path); err != nil {
		sayln(stderr, "shardpostctl:", path+":", err, "(nothing restored)")
		return exDataErr
	}

	f, err := os.Open(path)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exUsage
	}
	defer func() { _ = f.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep, err := migrate.Restore(ctx, f, &migrate.APITarget{C: client}, o)
	if rep != nil {
		printRestore(stdout, rep, asJSON)
	}

	if err != nil {
		sayln(stderr, "shardpostctl: restore:", err)
		return exPartial
	}

	return exOK
}

func printRestore(stdout io.Writer, rep *migrate.RestoreReport, asJSON bool) {
	if asJSON {
		printJSON(stdout, rep)
		return
	}

	sayf(stdout, "restore of %s into %s\n", rep.Source, rep.Account)

	for _, m := range rep.Mailboxes {
		sayf(stdout, "  %-40s uidvalidity %-10d %6d messages (%d imported, %d already there) verified %v\n", m.Name, m.UIDValidity,
			m.Messages, m.Imported, m.Existing, m.Verified)
	}

	b, _ := json.Marshal(rep.Scripts)
	sayf(stdout, "sieve scripts %s, active %q; acl restored %v\n", b, rep.Active, rep.ACLRestored)

	if !rep.SieveActivated && len(rep.Scripts) > 0 {
		sayln(stdout, "note: no Sieve script activated (restore into another account; -activate-sieve to change)")
	}

	if len(rep.Skipped) > 0 {
		sayln(stdout, fmt.Sprintf("skipped at export: %v", rep.Skipped))
	}
}
