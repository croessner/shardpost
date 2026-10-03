package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"

	"shardpost/internal/adminapi"
)

// The cluster marks of the online quota recount (stage B16) and of the
// mailbox lock of mailbox_delete (stage B17), as "fts online":
//
//	quota online status [-json]                        role diagnose
//	quota online enable -pods n [-force] [-json]       role repair
//	quota online disable [-json]                       role repair
//	mailbox lock status [-json]                        role diagnose
//	mailbox lock enable -pods n [-force] [-json]       role repair
//	mailbox lock disable [-json]                       role repair

const marksUsage = "usage: shardpostctl quota online status|enable -pods n [-force]|disable [-json] | " +
	"mailbox lock status|enable -pods n [-force]|disable [-json]"

// The group words after the noun.
const (
	markQuotaOnline = "online"
	markMailboxLock = "lock"
)

// isMarkCmd reports whether "<noun> <args[0]>" is one of the mark
// commands (quota online, mailbox lock).
func isMarkCmd(noun string, args []string) bool {
	if len(args) == 0 {
		return false
	}

	return (noun == cmdQuota && args[0] == markQuotaOnline) || (noun == labelMailbox && args[0] == markMailboxLock)
}

// marksCmd runs "quota online <verb>" and "mailbox lock <verb>".
func marksCmd(client *adminapi.ClientWithResponses, noun string, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		sayln(stderr, marksUsage)
		return exUsage
	}

	group, verb := args[0], args[1]

	fs := flag.NewFlagSet(noun+" "+group+" "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var a ftsArgs

	fs.BoolVar(&a.asJSON, "json", false, "print JSON")
	fs.BoolVar(&a.force, "force", false, "enable: set the mark without the registrations check (audited)")
	fs.IntVar(&a.pods, "pods", 0, "enable: the number of servers running now (required)")

	rest, err := parseInterleaved(fs, args[2:])
	if err != nil || len(rest) != 0 {
		sayln(stderr, marksUsage)
		return exUsage
	}

	calls := stageMark{ctx: context.Background(), c: client, mailboxLock: noun == labelMailbox}
	show := func(st *adminapi.FTSLayout3Status) int { return printLayout3(stdout, st, a.asJSON) }

	switch verb {
	case verbStatus:
		return request(stderr, calls.status, show)
	case verbEnable:
		return markEnable(stderr, noun+" "+group, a, calls.enable, show)
	case verbDisable:
		return request(stderr, calls.disable, show)
	default:
		sayln(stderr, marksUsage)
		return exUsage
	}
}

// stageMark is one of the two marks with its API calls.
type stageMark struct {
	ctx         context.Context
	c           *adminapi.ClientWithResponses
	mailboxLock bool
}

// status answers the mark's status.
func (m stageMark) status() (*adminapi.FTSLayout3Status, *adminapi.Error, int, error) {
	if m.mailboxLock {
		return decodeMark(m.c.GetMailboxLock(m.ctx))
	}

	return decodeMark(m.c.GetQuotaOnline(m.ctx))
}

// enable sets the mark.
func (m stageMark) enable(body adminapi.FTSLayout3Enable) statusCall {
	return func() (*adminapi.FTSLayout3Status, *adminapi.Error, int, error) {
		if m.mailboxLock {
			return decodeMark(m.c.EnableMailboxLock(m.ctx, body))
		}

		return decodeMark(m.c.EnableQuotaOnline(m.ctx, body))
	}
}

// disable removes the mark.
func (m stageMark) disable() (*adminapi.FTSLayout3Status, *adminapi.Error, int, error) {
	if m.mailboxLock {
		return decodeMark(m.c.DisableMailboxLock(m.ctx))
	}

	return decodeMark(m.c.DisableQuotaOnline(m.ctx))
}

// decodeMark reads the answer of a mark call: the status on 200, the
// error document otherwise (the six calls share the two documents, so
// one decoder serves them all).
func decodeMark(rsp *http.Response, err error) (*adminapi.FTSLayout3Status, *adminapi.Error, int, error) {
	if err != nil {
		return nil, nil, 0, err
	}

	defer func() { _ = rsp.Body.Close() }()

	body, err := io.ReadAll(rsp.Body)
	if err != nil {
		return nil, nil, 0, err
	}

	if rsp.StatusCode == http.StatusOK {
		var st adminapi.FTSLayout3Status
		if err := json.Unmarshal(body, &st); err != nil {
			return nil, nil, 0, err
		}

		return &st, nil, rsp.StatusCode, nil
	}

	var e adminapi.Error
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, nil, rsp.StatusCode, nil
	}

	return nil, &e, rsp.StatusCode, nil
}
