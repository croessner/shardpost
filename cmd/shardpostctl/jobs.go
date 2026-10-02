package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"shardpost/internal/adminapi"
)

// Maintenance jobs (roadmap 8c):
//
//	plan [-classes a,b] [-mailbox M] [-uids 1,2 | -deleted] [-layout 2|3] [-shards n] [-due-only] <kind> <account>   dry run, prints the plan
//	plan -all [-classes a,b] [-skip-idle] [-layout 2|3] [-shards n] [-due-only] <kind>   a batch of the kind over every account
//	plan show <plan-id>
//	apply [-wait] [-idempotency-key K] <plan-id>        run the plan as a job
//	check [-wait] <account>                             read-only index check
//	job list [-account A] [-state S] [-limit n] [-after id]
//	job show|wait|cancel <job-id>
//
// With -wait (and for job wait) the exit code is the job's outcome: 0
// succeeded, 1 partially failed, 69 failed or cancelled.

const jobsUsage = `usage: shardpostctl plan [-classes a,b] [-mailbox M] [-uids 1,2 | -deleted] [-layout 2|3] [-shards n] [-due-only] <kind> <account> |
  plan -all [-classes a,b] [-skip-idle] [-layout 2|3] [-shards n] [-due-only] <kind> | plan show <plan-id> |
  apply [-wait] [-idempotency-key K] <plan-id> | check [-wait] <account> |
  job list [-account A] [-state S] [-limit n] [-after id] | job show|wait|cancel <job-id>   (each with --json)
  kinds: scrub, quota_recalc, acl_reconcile, fts_rebuild, fts_dict_convert, fts_layout, mailbox_name_convert,
  attachment_backfill, mailbox_delete, expunge, purge; with -all: index_check, scrub, fts_rebuild, attachment_backfill, fts_layout`

// labelKind labels the kind row.
const labelKind = "kind"

// jobArgs are the parsed flags of the job commands.
type jobArgs struct {
	args             []string
	asJSON, wait     bool
	deleted          bool
	all, skipIdle    bool
	dueOnly          bool
	layout, shards   int
	uids             string
	classes, mailbox string
	idemKey, account string
	state, after     string
	limit            int
	poll             time.Duration
}

func parseJobArgs(name string, args []string, stderr io.Writer) (jobArgs, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var a jobArgs

	fs.BoolVar(&a.asJSON, "json", false, "print JSON")
	fs.BoolVar(&a.wait, "wait", false, "wait until the job ends")
	fs.StringVar(&a.classes, "classes", "", "scrub: repair classes, comma-separated (default all)")
	fs.StringVar(&a.mailbox, labelMailbox, "", "mailbox_delete, expunge: mailbox name or id")
	fs.StringVar(&a.uids, "uids", "", "expunge: UIDs, comma-separated")
	fs.BoolVar(&a.deleted, "deleted", false, "expunge: every message flagged \\Deleted")
	fs.BoolVar(&a.all, "all", false, "plan: a batch of the kind over every account")
	fs.BoolVar(&a.skipIdle, "skip-idle", false, "plan -all scrub: skip the accounts unchanged since their last clean scrub")
	fs.IntVar(&a.layout, "layout", 0, "fts_layout: the target layout (2 or 3; default the account's)")
	fs.IntVar(&a.shards, "shards", 0, "fts_layout: forced object counts of a layout 3 target (default from the key counts)")
	fs.BoolVar(&a.dueOnly, "due-only", false, "fts_layout: no change while the key counts ask for the shape the index has")
	fs.StringVar(&a.idemKey, "idempotency-key", "", "reuse the job of an earlier identical request")
	fs.StringVar(&a.account, "account", "", "job list: only this account")
	fs.StringVar(&a.state, labelState, "", "job list: only this state")
	fs.StringVar(&a.after, "after", "", "job list: continue after this job id")
	fs.IntVar(&a.limit, "limit", 50, "job list: jobs per page")
	fs.DurationVar(&a.poll, "poll", 2*time.Second, "how often -wait polls")

	var err error
	if a.args, err = parseInterleaved(fs, args); err != nil {
		return a, false
	}

	return a, true
}

// jobsCmd dispatches plan, apply, check and job.
func jobsCmd(client *adminapi.ClientWithResponses, cmd string, args []string, stdout, stderr io.Writer) int {
	a, ok := parseJobArgs(cmd, args, stderr)
	if !ok {
		return exUsage
	}

	ctx := context.Background()
	verb := cmd

	if (cmd == cmdPlan || cmd == cmdJob) && len(a.args) > 0 {
		switch a.args[0] {
		case verbShow, verbList, "wait", verbCancel:
			verb, a.args = cmd+" "+a.args[0], a.args[1:]
		}
	}

	run, n := jobCommand(verb)
	if verb == cmdPlan && a.all {
		run, n = planAll, 1
	}

	if run == nil || len(a.args) != n || (a.all && verb != cmdPlan) {
		sayln(stderr, jobsUsage)
		return exUsage
	}

	return run(ctx, client, a, stdout, stderr)
}

type jobFunc func(context.Context, *adminapi.ClientWithResponses, jobArgs, io.Writer, io.Writer) int

func jobCommand(verb string) (jobFunc, int) {
	switch verb {
	case cmdPlan:
		return planCreate, 2
	case "plan show":
		return planShow, 1
	case cmdApply:
		return apply, 1
	case cmdCheck:
		return check, 1
	case "job list":
		return jobList, 0
	case "job show":
		return jobShow(false), 1
	case "job wait":
		return jobShow(true), 1
	case "job cancel":
		return jobCancel, 1
	default:
		return nil, 0
	}
}

func planCreate(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, stdout, stderr io.Writer) int {
	kind, account := a.args[0], a.args[1]
	params := map[string]any{}

	if a.classes != "" {
		params["classes"] = strings.Split(a.classes, ",")
	}

	if a.mailbox != "" {
		id, code, ok := resolveMailbox(ctx, c, account, a.mailbox, stderr)
		if !ok {
			return code
		}

		params["mailbox_id"] = id
	}

	if a.uids != "" {
		uids, err := uidList(a.uids)
		if err != nil {
			sayln(stderr, "shardpostctl:", err)
			return exUsage
		}

		params["uids"] = uids
	}

	if a.deleted {
		params["deleted"] = true
	}

	layoutParams(a, params)

	req := adminapi.PlanRequest{Kind: adminapi.PlanRequestKind(kind)}
	if len(params) > 0 {
		req.Params = &params
	}

	resp, err := c.CreatePlanWithResponse(ctx, account, req)
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON201 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printPlan(stdout, a, resp.JSON201)
}

// planAll plans a batch of the kind over every account of the account
// directory (role of the kind, scope of all accounts).
func planAll(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, stdout, stderr io.Writer) int {
	if a.mailbox != "" || a.uids != "" || a.deleted {
		sayln(stderr, "shardpostctl: -mailbox, -uids and -deleted name one account; a batch runs on all")
		return exUsage
	}

	req := adminapi.BatchPlanRequest{Kind: a.args[0]}
	params := map[string]any{}

	if a.classes != "" {
		params["classes"] = strings.Split(a.classes, ",")
	}

	if layoutParams(a, params); len(params) > 0 {
		req.Params = &params
	}

	if a.skipIdle {
		req.SkipIdle = &a.skipIdle
	}

	resp, err := c.CreateBatchPlanWithResponse(ctx, req)
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON201 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printPlan(stdout, a, resp.JSON201)
}

func planShow(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, stdout, stderr io.Writer) int {
	return request(stderr, func() (*adminapi.Plan, *adminapi.Error, int, error) {
		resp, err := c.GetPlanWithResponse(ctx, a.args[0])
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(p *adminapi.Plan) int { return printPlan(stdout, a, p) })
}

// layoutParams adds the parameters of fts_layout.
func layoutParams(a jobArgs, params map[string]any) {
	if a.layout != 0 {
		params["layout"] = a.layout
	}

	if a.shards != 0 {
		params["shards"] = a.shards
	}

	if a.dueOnly {
		params["due_only"] = true
	}
}

// request runs one API call: a transport error or an error document ends
// with its exit code, a result is printed.
func request[T any](stderr io.Writer, call func() (*T, *adminapi.Error, int, error), show func(*T) int) int {
	body, e, status, err := call()
	if err != nil {
		return transportError(stderr, err)
	}

	if body == nil {
		return failure(stderr, e, status)
	}

	return show(body)
}

func printPlan(stdout io.Writer, a jobArgs, p *adminapi.Plan) int {
	if a.asJSON {
		return printJSON(stdout, p)
	}

	summary, _ := json.MarshalIndent(p.Summary, "", "  ")

	table(stdout, [][2]string{{cmdPlan, p.Id}, {labelKind, p.Kind}, {cmdAccount, accountOrAll(p.Account)},
		{"expires", p.Expires.Format(time.RFC3339)}, {"applied", deref(p.AppliedJob)}})
	sayln(stdout, string(summary))
	sayf(stdout, "apply with: shardpostctl apply %s\n", p.Id)

	return exOK
}

func apply(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, stdout, stderr io.Writer) int {
	return submit(ctx, c, a, adminapi.JobRequest{PlanId: &a.args[0]}, stdout, stderr)
}

func check(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, stdout, stderr io.Writer) int {
	kind := adminapi.IndexCheck

	return submit(ctx, c, a, adminapi.JobRequest{Kind: &kind, Account: &a.args[0]}, stdout, stderr)
}

func submit(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, req adminapi.JobRequest, stdout, stderr io.Writer) int {
	resp, err := c.SubmitJobWithResponse(ctx, &adminapi.SubmitJobParams{IdempotencyKey: optional(a.idemKey)}, req)
	if err != nil {
		return transportError(stderr, err)
	}

	j := resp.JSON202
	if j == nil {
		j = resp.JSON200
	}

	if j == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	if a.wait {
		return waitJob(ctx, c, a, j.Id, stdout, stderr)
	}

	return printJob(stdout, a, j)
}

func jobShow(wait bool) jobFunc {
	return func(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, stdout, stderr io.Writer) int {
		if wait {
			return waitJob(ctx, c, a, a.args[0], stdout, stderr)
		}

		j, code := getJob(ctx, c, a.args[0], stderr)
		if j == nil {
			return code
		}

		return printJob(stdout, a, j)
	}
}

func getJob(ctx context.Context, c *adminapi.ClientWithResponses, id string, stderr io.Writer) (*adminapi.Job, int) {
	resp, err := c.GetJobWithResponse(ctx, id)
	if err != nil {
		return nil, transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return nil, failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return resp.JSON200, exOK
}

// waitJob polls until the job ends and exits with its outcome.
func waitJob(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, id string, stdout, stderr io.Writer) int {
	for {
		j, code := getJob(ctx, c, id, stderr)
		if j == nil {
			return code
		}

		switch j.State {
		case adminapi.Succeeded, adminapi.PartiallyFailed, adminapi.Failed, adminapi.Cancelled:
			printJob(stdout, a, j)
			return jobExit(j.State)
		}

		time.Sleep(a.poll)
	}
}

// accountOrAll names the account of a plan or job; a batch has none.
func accountOrAll(account string) string {
	if account == "" {
		return "(all accounts)"
	}

	return account
}

// jobExit maps the final state to the exit code.
func jobExit(state adminapi.JobState) int {
	switch state {
	case adminapi.Succeeded:
		return exOK
	case adminapi.PartiallyFailed:
		return exPartial
	default:
		return exUnavailable
	}
}

func jobCancel(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, stdout, stderr io.Writer) int {
	resp, err := c.CancelJobWithResponse(ctx, a.args[0])
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.StatusCode() != http.StatusOK {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return jobShow(false)(ctx, c, a, stdout, stderr)
}

func jobList(ctx context.Context, c *adminapi.ClientWithResponses, a jobArgs, stdout, stderr io.Writer) int {
	p := adminapi.ListJobsParams{Account: optional(a.account), After: optional(a.after), Limit: &a.limit}

	if a.state != "" {
		st := adminapi.JobState(a.state)
		p.State = &st
	}

	resp, err := c.ListJobsWithResponse(ctx, &p)
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	if a.asJSON {
		return printJSON(stdout, resp.JSON200)
	}

	rows := [][2]string{}
	for _, j := range resp.JSON200.Items {
		rows = append(rows, [2]string{j.Id, fmt.Sprintf("%-16s %-18s %s", j.Kind, j.State, accountOrAll(j.Account))})
	}

	table(stdout, rows)

	if resp.JSON200.Next != nil {
		sayf(stdout, "more: -after %s\n", *resp.JSON200.Next)
	}

	return exOK
}

func printJob(stdout io.Writer, a jobArgs, j *adminapi.Job) int {
	if a.asJSON {
		return printJSON(stdout, j)
	}

	progress := fmt.Sprintf("%d/%d", j.Progress.Done, j.Progress.Total)
	if j.Progress.Phase != nil {
		progress = *j.Progress.Phase + " " + progress
	}

	table(stdout, [][2]string{{cmdJob, j.Id}, {labelKind, j.Kind}, {cmdAccount, accountOrAll(j.Account)}, {labelState, string(j.State)},
		{"progress", progress}, {cmdServer, deref(j.Server)}, {labelAttempts, fmt.Sprint(j.Attempts)}})

	for _, e := range j.Errors {
		sayln(stdout, "error: "+e)
	}

	if j.Result != nil {
		b, _ := json.MarshalIndent(j.Result, "", "  ")
		sayln(stdout, string(b))
	}

	return exOK
}
