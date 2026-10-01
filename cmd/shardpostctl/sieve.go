package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"shardpost/internal/adminapi"
)

// Sieve and learning queue commands (roadmap 8d), see support.go.

func sieveList(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	resp, err := c.ListSieveScriptsWithResponse(ctx, a.args[0])

	return fetch(stderr, resp, err, sieveListBody, func(l *adminapi.SieveList) int { return printSieveList(stdout, a, l) })
}

// fetch shows a response: a transport error or an error document ends
// with its exit code. body is only called without error.
func fetch[R, T any](stderr io.Writer, resp R, err error, body func(R) (*T, *adminapi.Error, int), show func(*T) int) int {
	if err != nil {
		return transportError(stderr, err)
	}

	b, e, status := body(resp)
	if b == nil {
		return failure(stderr, e, status)
	}

	return show(b)
}

func sieveListBody(r *adminapi.ListSieveScriptsResponse) (*adminapi.SieveList, *adminapi.Error, int) {
	return r.JSON200, r.JSONDefault, r.StatusCode()
}

func learningJobBody(r *adminapi.GetLearningJobResponse) (*adminapi.LearningJob, *adminapi.Error, int) {
	return r.JSON200, r.JSONDefault, r.StatusCode()
}

func printSieveList(stdout io.Writer, a supportArgs, l *adminapi.SieveList) int {
	if a.asJSON {
		return printJSON(stdout, l)
	}

	for _, s := range l.Scripts {
		mark := " "
		if s.Active {
			mark = "*"
		}

		sayf(stdout, "%s %-30s %8d  %s\n", mark, s.Name, s.Size, s.Etag)
	}

	return exOK
}

func sieveShow(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	return request(stderr, func() (*adminapi.SieveScript, *adminapi.Error, int, error) {
		resp, err := c.GetSieveScriptWithResponse(ctx, a.args[0], a.args[1])
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(s *adminapi.SieveScript) int { return printScript(stdout, a, s) })
}

func printScript(stdout io.Writer, a supportArgs, s *adminapi.SieveScript) int {
	if a.asJSON {
		return printJSON(stdout, s)
	}

	sayf(stdout, "# %s  active %v  etag %s\n%s", s.Name, s.Active, s.Etag, s.Script)

	if !strings.HasSuffix(s.Script, "\n") {
		sayln(stdout, "")
	}

	return exOK
}

func sievePut(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	text, err := readInput(a)
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exUsage
	}

	p := adminapi.PutSieveScriptParams{IfMatch: optional(a.ifMatch)}
	if a.create {
		star := adminapi.PutSieveScriptParamsIfNoneMatch("*")
		p.IfNoneMatch = &star
	}

	req := adminapi.SievePut{Script: string(text), Activate: &a.activate}

	return request(stderr, func() (*adminapi.SieveScript, *adminapi.Error, int, error) {
		resp, err := c.PutSieveScriptWithResponse(ctx, a.args[0], a.args[1], &p, req)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(s *adminapi.SieveScript) int {
		if a.asJSON {
			return printJSON(stdout, s)
		}

		sayf(stdout, "stored %s  active %v  etag %s\n", s.Name, s.Active, s.Etag)

		return exOK
	})
}

func sieveDelete(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, _, stderr io.Writer) int {
	resp, err := c.DeleteSieveScriptWithResponse(ctx, a.args[0], a.args[1], &adminapi.DeleteSieveScriptParams{IfMatch: optional(a.ifMatch)})
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.StatusCode() != http.StatusNoContent {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return exOK
}

// sieveActivate activates args[1], or deactivates without it.
func sieveActivate(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	name := ""
	if len(a.args) > 1 {
		name = a.args[1]
	}

	return request(stderr, func() (*adminapi.SieveList, *adminapi.Error, int, error) {
		resp, err := c.SetActiveSieveScriptWithResponse(ctx, a.args[0], adminapi.SieveActivate{Name: name})
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(l *adminapi.SieveList) int { return printSieveList(stdout, a, l) })
}

// sieveCheck: -name checks a stored script (default the active one),
// otherwise stdin/-file is the script; -message names a stored message
// (mailbox:uidvalidity:uid) to run it on.
// maxCheckMessage bounds a test message read from standard input; the
// server has a limit of its own and answers 413 beyond it.
const maxCheckMessage = 1 << 20

func sieveCheck(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	req := adminapi.SieveCheck{Name: optional(a.name), EnvelopeFrom: optional(a.from), EnvelopeTo: optional(a.to)}

	if a.name == "" && (a.scriptFile != "" || a.message == "") {
		text, err := readInput(a)
		if err != nil {
			sayln(stderr, "shardpostctl:", err)
			return exUsage
		}

		s := string(text)
		req.Script = &s
	}

	if code, ok := sieveCheckMessage(ctx, c, a, &req, stderr); !ok {
		return code
	}

	return request(stderr, func() (*adminapi.SieveCheckResult, *adminapi.Error, int, error) {
		resp, err := c.CheckSieveWithResponse(ctx, a.args[0], req)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(res *adminapi.SieveCheckResult) int { return printCheck(stdout, a, res) })
}

// sieveCheckMessage adds the message the script runs on: the stored one
// that -message names, or a test message from standard input when the
// script itself came from a file or from the account. Without either,
// the script is only compiled.
func sieveCheckMessage(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, req *adminapi.SieveCheck, stderr io.Writer) (int, bool) {
	if a.message != "" {
		ref, code, ok := sieveMessageRef(ctx, c, a, stderr)
		if !ok {
			return code, false
		}

		req.Message = ref

		return exOK, true
	}

	if req.Script != nil && a.scriptFile == "" {
		return exOK, true // standard input carried the script
	}

	raw, err := io.ReadAll(io.LimitReader(a.stdin, maxCheckMessage+1))

	switch {
	case err != nil:
		sayln(stderr, "shardpostctl:", err)

		return exUsage, false
	case len(raw) > maxCheckMessage:
		sayln(stderr, "shardpostctl: the test message is too large")

		return exUsage, false
	case len(raw) > 0:
		text := string(raw)
		req.Raw = &text
	}

	return exOK, true
}

// sieveMessageRef parses -message mailbox:uidvalidity:uid (the mailbox
// by name or id; the last two fields are numbers).
func sieveMessageRef(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stderr io.Writer) (*adminapi.SieveMessageRef, int, bool) {
	parts := strings.Split(a.message, ":")
	if len(parts) < 3 {
		sayln(stderr, "shardpostctl: -message is mailbox:uidvalidity:uid")
		return nil, exUsage, false
	}

	n := len(parts)
	validity, err1 := strconv.ParseInt(parts[n-2], 10, 64)
	uid, err2 := strconv.ParseInt(parts[n-1], 10, 64)

	if err1 != nil || err2 != nil {
		sayln(stderr, "shardpostctl: -message is mailbox:uidvalidity:uid")
		return nil, exUsage, false
	}

	id, code, ok := resolveMailbox(ctx, c, a.args[0], strings.Join(parts[:n-2], ":"), stderr)
	if !ok {
		return nil, code, false
	}

	return &adminapi.SieveMessageRef{MailboxId: id, Uidvalidity: validity, Uid: uid}, exOK, true
}

func printCheck(stdout io.Writer, a supportArgs, res *adminapi.SieveCheckResult) int {
	if a.asJSON {
		return printJSON(stdout, res)
	}

	if !res.Valid {
		sayln(stdout, "invalid: "+deref(res.Error))
		return exDataErr
	}

	sayln(stdout, "valid")

	if res.Error != nil {
		sayln(stdout, "run failed: "+*res.Error)
	}

	if x := res.Actions; x != nil {
		for _, f := range x.Fileinto {
			sayf(stdout, "fileinto %q %s\n", f.Mailbox, strings.Join(f.Flags, " "))
		}

		rows := [][2]string{{"keep", fmt.Sprint(x.Keep)}, {"redirect", strings.Join(x.Redirect, ", ")},
			{"notify", strings.Join(x.Notify, ", ")}, {"programs", strings.Join(x.Programs, ", ")}}
		if x.Reject != nil {
			rows = append(rows, [2]string{"reject", *x.Reject})
		}

		if x.Vacation != nil {
			rows = append(rows, [2]string{"vacation", fmt.Sprintf("%q %s", x.Vacation.Subject, vacationPeriod(x.Vacation.Days, x.Vacation.Seconds))})
		}

		if x.Scripts != nil {
			rows = append(rows, [2]string{"scripts", strings.Join(*x.Scripts, ", ")})
		}

		table(stdout, rows)

		if x.FailedScripts != nil {
			for _, s := range *x.FailedScripts {
				sayln(stdout, "failed script (skipped): "+s)
			}
		}

		for _, s := range x.Suppressed {
			sayln(stdout, "suppressed: "+s)
		}
	}

	return exOK
}

func learningList(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	p := adminapi.ListLearningJobsParams{Account: optional(a.account), After: optional(a.after), Limit: optInt(a.limit)}
	if a.state != "" {
		st := adminapi.LearningState(a.state)
		p.State = &st
	}

	return request(stderr, func() (*adminapi.LearningPage, *adminapi.Error, int, error) {
		resp, err := c.ListLearningJobsWithResponse(ctx, &p)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(page *adminapi.LearningPage) int {
		if a.asJSON {
			return printJSON(stdout, page)
		}

		for _, j := range page.Items {
			sayf(stdout, "%s  %-9s %-16s attempts %d  %s\n", j.Id, j.State, j.Program, j.Attempts, j.Account)
		}

		if page.Next != nil {
			sayf(stdout, "more: -after %s\n", *page.Next)
		}

		return exOK
	})
}

func printLearningJob(stdout io.Writer, a supportArgs, j *adminapi.LearningJob) int {
	if a.asJSON {
		return printJSON(stdout, j)
	}

	table(stdout, [][2]string{{labelID, j.Id}, {cmdAccount, j.Account}, {labelMailbox, j.MailboxId}, {"program", j.Program},
		{labelState, string(j.State)}, {labelAttempts, fmt.Sprint(j.Attempts)}, {"retries", fmt.Sprint(j.Retries)},
		{"created", j.Created.Format("2006-01-02 15:04:05")}, {"next", j.NextAttempt.Format("2006-01-02 15:04:05")},
		{labelSize, fmt.Sprint(j.Size)}})

	return exOK
}

func learningShow(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	resp, err := c.GetLearningJobWithResponse(ctx, a.args[0])

	return fetch(stderr, resp, err, learningJobBody, func(j *adminapi.LearningJob) int { return printLearningJob(stdout, a, j) })
}

func learningRetry(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	return request(stderr, func() (*adminapi.LearningJob, *adminapi.Error, int, error) {
		resp, err := c.RetryLearningJobWithResponse(ctx, a.args[0], adminapi.LearningRetry{ConfirmRepeat: &a.confirm})
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(j *adminapi.LearningJob) int { return printLearningJob(stdout, a, j) })
}

// vacationPeriod describes the effective period of a vacation reply. A
// server that does not report seconds yet sends days alone.
func vacationPeriod(days int, seconds int64) string {
	switch {
	case seconds == 0 && days > 0:
		return fmt.Sprintf("every %d days", days)
	case seconds == 0:
		return "for every message"
	case seconds%86400 == 0:
		return fmt.Sprintf("every %d days", seconds/86400)
	default:
		return fmt.Sprintf("every %d seconds", seconds)
	}
}
