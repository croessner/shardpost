package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"shardpost/internal/model"
)

// Roles of the job kinds (admin API roles of the same name).
const (
	RoleDiagnose = "diagnose"
	RoleRepair   = "repair"
	RoleDelete   = "delete"
)

// Kind names.
const (
	KindIndexCheck    = "index_check"
	KindScrub         = "scrub"
	KindQuotaRecalc   = "quota_recalc"
	KindACLReconcile  = "acl_reconcile"
	KindFTSRebuild    = "fts_rebuild"
	KindMailboxDelete = "mailbox_delete"
	KindPurge         = "purge"
)

// Touching is implemented by plan summaries that name other accounts the
// job would change (the admin API checks the client scope on them).
type Touching interface{ Touched() []string }

// identAnyone is the RFC 4314 identifier for everybody (no pointers).
const identAnyone = "anyone"

// errNotPlanned is the Plan of an unplanned kind.
var errNotPlanned = errors.New("this kind runs without a plan")

// kindBase carries the static properties of a kind.
type kindBase struct {
	name, role         string
	planned, exclusive bool
}

func (k kindBase) Name() string    { return k.name }
func (k kindBase) Role() string    { return k.role }
func (k kindBase) Planned() bool   { return k.planned }
func (k kindBase) Exclusive() bool { return k.exclusive }

// Kinds returns every job kind; grace is the scrub grace period, settle
// how long an exclusive recount waits for deliveries in flight.
func Kinds(grace time.Duration) []Kind {
	return []Kind{
		&indexCheck{kindBase{KindIndexCheck, RoleDiagnose, false, false}, grace},
		&scrubKind{kindBase{KindScrub, RoleRepair, true, false}, grace},
		&quotaRecalc{kindBase{KindQuotaRecalc, RoleRepair, true, true}},
		&aclReconcile{kindBase{KindACLReconcile, RoleRepair, true, false}},
		&ftsRebuild{kindBase{KindFTSRebuild, RoleRepair, true, true}},
		&mailboxDelete{kindBase{KindMailboxDelete, RoleDelete, true, true}},
		&purge{kindBase{KindPurge, RoleDelete, true, true}},
	}
}

// capability returns the store capability T of the account or an error.
func capability[T any](u model.User, what string) (T, error) {
	c, ok := u.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("the store cannot %s", what)
	}

	return c, nil
}

// --- index_check -----------------------------------------------------------

// indexCheck runs the scrubber without repairs and reports its findings.
type indexCheck struct {
	kindBase
	grace time.Duration
}

func (k *indexCheck) Plan(context.Context, *Env, model.User, string, json.RawMessage) (any, string, error) {
	return nil, "", errNotPlanned
}

func (k *indexCheck) Run(_ context.Context, run *Run) (any, error) {
	sc, err := capability[model.Scrubber](run.User, "scrub")
	if err != nil {
		return nil, err
	}

	return sc.RunScrub(model.ScrubRequest{DryRun: true, Grace: k.grace})
}

// --- scrub -----------------------------------------------------------------

// scrubParams selects repair classes (empty = all).
type scrubParams struct {
	Classes []string `json:"classes,omitempty"`
}

// scrubKind repairs the selected classes; the plan is the dry run.
type scrubKind struct {
	kindBase
	grace time.Duration
}

func parseScrub(params json.RawMessage) (map[string]bool, error) {
	var p scrubParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%w: params: %v", ErrInvalidParams, err)
		}
	}

	if len(p.Classes) == 0 {
		p.Classes = model.RepairClasses
	}

	set := map[string]bool{}

	for _, c := range p.Classes {
		if !slices.Contains(model.RepairClasses, c) {
			return nil, fmt.Errorf("%w: unknown repair class %q (known: %s)", ErrInvalidParams, c, strings.Join(model.RepairClasses, ", "))
		}

		set[c] = true
	}

	return set, nil
}

func (k *scrubKind) Plan(_ context.Context, _ *Env, u model.User, _ string, params json.RawMessage) (any, string, error) {
	classes, err := parseScrub(params)
	if err != nil {
		return nil, "", err
	}

	sc, err := capability[model.Scrubber](u, "scrub")
	if err != nil {
		return nil, "", err
	}

	rep, err := sc.RunScrub(model.ScrubRequest{DryRun: true, Grace: k.grace})
	if err != nil {
		return nil, "", err
	}

	return map[string]any{"classes": sortedKeys(classes), "findings": rep}, "", nil
}

func (k *scrubKind) Run(_ context.Context, run *Run) (any, error) {
	classes, err := parseScrub(run.Job.Params)
	if err != nil {
		return nil, err
	}

	sc, err := capability[model.Scrubber](run.User, "scrub")
	if err != nil {
		return nil, err
	}

	if err := run.Fence(); err != nil {
		return nil, err
	}

	rep, err := sc.RunScrub(model.ScrubRequest{Repairs: classes, Grace: k.grace, Fence: run.Fence})
	if err != nil {
		return rep, err
	}

	// Repairs skipped after a lost lease must not count as done.
	return rep, run.Fence()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// --- quota_recalc ----------------------------------------------------------

// quotaRecalc replaces the usage counters by a recount under the
// maintenance lock (no delivery holds a reservation then).
type quotaRecalc struct{ kindBase }

type quotaSummary struct {
	Counted model.Quota `json:"counted"`
	Stored  model.Quota `json:"stored"`
	Differs bool        `json:"differs"`
}

func (k *quotaRecalc) Plan(_ context.Context, _ *Env, u model.User, _ string, _ json.RawMessage) (any, string, error) {
	rc, err := capability[model.QuotaRecounter](u, "recount quota")
	if err != nil {
		return nil, "", err
	}

	counted, stored, err := rc.RecountQuota(false)

	return quotaSummary{counted, stored, counted != stored}, "", err
}

func (k *quotaRecalc) Run(_ context.Context, run *Run) (any, error) {
	rc, err := capability[model.QuotaRecounter](run.User, "recount quota")
	if err != nil {
		return nil, err
	}

	if err := run.Fence(); err != nil {
		return nil, err
	}

	counted, stored, err := rc.RecountQuota(true)

	return quotaSummary{counted, stored, counted != stored}, err
}

// --- acl_reconcile ---------------------------------------------------------

// aclFix is one repair of a shared-mailbox pointer.
type aclFix struct {
	Action  string `json:"action"` // add_pointer, remove_pointer
	Owner   string `json:"owner"`
	Mailbox string `json:"mailbox"`
	Grantee string `json:"grantee"`
}

// Actions of acl_reconcile.
const (
	fixAddPointer    = "add_pointer"
	fixRemovePointer = "remove_pointer"
)

type aclSummary struct {
	Fixes    []aclFix `json:"fixes"`
	Accounts []string `json:"accounts"`
}

func (s aclSummary) Touched() []string { return s.Accounts }

// aclReconcile checks both sides of the account's sharing: pointers its
// grantees should hold for its mailboxes, and its own pointers to
// mailboxes of others that no longer grant it anything.
type aclReconcile struct{ kindBase }

func (k *aclReconcile) Plan(_ context.Context, env *Env, u model.User, account string, _ json.RawMessage) (any, string, error) {
	s, err := reconcileFixes(env, u, account)
	return s, "", err
}

// reconcileFixes computes the needed pointer repairs.
func reconcileFixes(env *Env, u model.User, account string) (aclSummary, error) {
	s := aclSummary{Fixes: []aclFix{}, Accounts: []string{}}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return s, err
	}

	for _, mb := range boxes {
		for grantee, rights := range mb.ACL {
			if grantee == identAnyone || rights == "" {
				continue
			}

			has, err := hasPointer(env, grantee, account, mb.Name)
			if err != nil {
				return s, err
			}

			if !has {
				s.Fixes = append(s.Fixes, aclFix{fixAddPointer, account, mb.Name, grantee})
				s.Accounts = appendUnique(s.Accounts, grantee)
			}
		}
	}

	refs, err := u.ListShared()
	if err != nil {
		return s, err
	}

	for _, ref := range refs {
		granted, err := grants(env, ref.Owner, ref.Mailbox, account)
		if err != nil {
			return s, err
		}

		if !granted {
			s.Fixes = append(s.Fixes, aclFix{fixRemovePointer, ref.Owner, ref.Mailbox, account})
		}
	}

	return s, nil
}

func hasPointer(env *Env, grantee, owner, mailbox string) (bool, error) {
	g, err := env.Accounts.User(grantee)
	if err != nil {
		return false, err
	}
	defer g.Close()

	refs, err := g.ListShared()
	if err != nil {
		return false, err
	}

	for _, r := range refs {
		if r.Owner == owner && r.Mailbox == mailbox {
			return true, nil
		}
	}

	return false, nil
}

// grants reports whether the owner's mailbox still names the account.
func grants(env *Env, owner, mailbox, account string) (bool, error) {
	o, err := env.Accounts.User(owner)
	if err != nil {
		return false, err
	}
	defer o.Close()

	acl, err := o.GetACL(mailbox)
	if errors.Is(err, model.ErrNoMailbox) {
		return false, nil
	}

	return acl[account] != "", err
}

func appendUnique(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}

	return append(list, s)
}

func (k *aclReconcile) Run(_ context.Context, run *Run) (any, error) {
	var planned aclSummary
	if run.Plan != nil {
		_ = json.Unmarshal(run.Plan.Summary, &planned)
	}

	now, err := reconcileFixes(run.Env, run.User, run.Job.Account)
	if err != nil {
		return nil, err
	}

	applied := []aclFix{}

	// Only repairs that were planned and are still needed.
	for _, fix := range now.Fixes {
		if !slices.Contains(planned.Fixes, fix) {
			continue
		}

		if err := run.Fence(); err != nil {
			return applied, err
		}

		if err := applyFix(run, fix); err != nil {
			run.Partial("%s %s/%s for %s: %v", fix.Action, fix.Owner, fix.Mailbox, fix.Grantee, err)
			continue
		}

		applied = append(applied, fix)
	}

	return map[string]any{"applied": applied}, nil
}

func applyFix(run *Run, fix aclFix) error {
	switch fix.Action {
	case fixAddPointer:
		// Replacing the rights with themselves writes the pointer.
		mod, err := capability[model.ACLModifier](run.User, "change ACLs")
		if err != nil {
			return err
		}

		_, _, err = mod.ModifyACL(fix.Mailbox, fix.Grantee, -1, func(old string) string { return old })

		return err
	case fixRemovePointer:
		return run.User.RemoveSharedRef(model.SharedRef{Owner: fix.Owner, Mailbox: fix.Mailbox})
	default:
		return fmt.Errorf("unknown action %q", fix.Action)
	}
}

// --- fts_rebuild -----------------------------------------------------------

// ftsRebuild rebuilds the full-text index under the maintenance lock.
type ftsRebuild struct{ kindBase }

type ftsSummary struct {
	Mailboxes int   `json:"mailboxes"`
	Messages  int64 `json:"messages"`
	Bytes     int64 `json:"bytes"`
}

func (k *ftsRebuild) Plan(_ context.Context, _ *Env, u model.User, _ string, _ json.RawMessage) (any, string, error) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return nil, "", err
	}

	s := ftsSummary{Mailboxes: len(boxes)}

	for i := range boxes {
		msgs, err := u.ListMessages(boxes[i].ID)
		if err != nil {
			return nil, "", err
		}

		for j := range msgs {
			s.Messages++
			s.Bytes += msgs[j].Size
		}
	}

	return s, "", nil
}

func (k *ftsRebuild) Run(_ context.Context, run *Run) (any, error) {
	rb, err := capability[model.FTSRebuilder](run.User, "rebuild the full-text index")
	if err != nil {
		return nil, err
	}

	var last time.Time

	indexed := 0
	err = rb.RebuildFTS(run.Fence, func(done, total int) {
		indexed = done
		if time.Since(last) > time.Second || done == total {
			last = time.Now()
			_ = run.Progress("index", int64(done), int64(total))
		}
	})

	return map[string]int{"indexed": indexed}, err
}

// --- mailbox_delete --------------------------------------------------------

type mailboxDeleteParams struct {
	MailboxID string `json:"mailbox_id"`
}

type deleteTarget struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Messages int64  `json:"messages"`
	Bytes    int64  `json:"bytes"`
}

type deleteSummary struct {
	Mailboxes []deleteTarget `json:"mailboxes"`
	Messages  int64          `json:"messages"`
	Bytes     int64          `json:"bytes"`
}

// mailboxDelete deletes a mailbox with its children from a plan.
type mailboxDelete struct{ kindBase }

// deleteTargets returns the mailbox and its children (deepest first) and
// the fingerprint of their state.
func deleteTargets(u model.User, params json.RawMessage) (deleteSummary, string, error) {
	var p mailboxDeleteParams
	if err := json.Unmarshal(params, &p); err != nil || p.MailboxID == "" {
		return deleteSummary{}, "", fmt.Errorf("%w: mailbox_id required", ErrInvalidParams)
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return deleteSummary{}, "", err
	}

	i := slices.IndexFunc(boxes, func(mb model.Mailbox) bool { return mb.ID == p.MailboxID })
	if i < 0 {
		return deleteSummary{}, "", model.ErrNoMailbox
	}

	root := boxes[i].Name
	if strings.EqualFold(root, "INBOX") {
		return deleteSummary{}, "", fmt.Errorf("%w: INBOX cannot be deleted (purge the account instead)", ErrInvalidParams)
	}

	var s deleteSummary

	for _, mb := range boxes {
		if mb.Name == root || strings.HasPrefix(mb.Name, root+"/") {
			t, err := targetOf(u, mb)
			if err != nil {
				return deleteSummary{}, "", err
			}

			s.Mailboxes = append(s.Mailboxes, t)
			s.Messages += t.Messages
			s.Bytes += t.Bytes
		}
	}

	sort.Slice(s.Mailboxes, func(a, b int) bool { return len(s.Mailboxes[a].Name) > len(s.Mailboxes[b].Name) })

	fp, err := fingerprint(u, s.Mailboxes)

	return s, fp, err
}

func targetOf(u model.User, mb model.Mailbox) (deleteTarget, error) {
	msgs, err := u.ListMessages(mb.ID)
	if err != nil {
		return deleteTarget{}, err
	}

	t := deleteTarget{ID: mb.ID, Name: mb.Name, Messages: int64(len(msgs))}
	for i := range msgs {
		t.Bytes += msgs[i].Size
	}

	return t, nil
}

// fingerprint hashes the identity and change state of the mailboxes.
func fingerprint(u model.User, targets []deleteTarget) (string, error) {
	lines := make([]string, 0, len(targets))

	for _, t := range targets {
		info, err := u.Info(t.Name)
		if err != nil {
			return "", err
		}

		lines = append(lines, fmt.Sprintf("%s %s %d %d %d", info.ID, info.Name, info.UIDValidity, info.UIDNext, info.HighestModSeq))
	}

	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))

	return hex.EncodeToString(sum[:]), nil
}

func (k *mailboxDelete) Plan(_ context.Context, _ *Env, u model.User, _ string, params json.RawMessage) (any, string, error) {
	return deleteTargets(u, params)
}

// deleteCheckpoint lists the mailboxes already deleted.
type deleteCheckpoint struct {
	Deleted []string `json:"deleted"`
}

func (k *mailboxDelete) Run(_ context.Context, run *Run) (any, error) {
	var cp deleteCheckpoint

	resumed := run.LoadCheckpoint(&cp)

	var targets []deleteTarget

	if resumed {
		// The drift check passed before the first delete; the plan names
		// what remains.
		var s deleteSummary

		_ = json.Unmarshal(run.Plan.Summary, &s)
		targets = s.Mailboxes
	} else {
		s, fp, err := deleteTargets(run.User, run.Job.Params)
		if err != nil {
			return nil, err
		}

		if fp != run.Plan.Fingerprint {
			return nil, ErrDrift
		}

		targets = s.Mailboxes
	}

	for _, t := range targets {
		if slices.Contains(cp.Deleted, t.ID) {
			continue
		}

		if err := run.Fence(); err != nil {
			return cp, err
		}

		if err := run.User.DeleteMailbox(t.Name); err != nil && !errors.Is(err, model.ErrNoMailbox) {
			return cp, fmt.Errorf("delete %q: %w", t.Name, err)
		}

		cp.Deleted = append(cp.Deleted, t.ID)

		if err := run.Checkpoint(cp); err != nil {
			return cp, err
		}
	}

	return cp, nil
}

// --- purge -----------------------------------------------------------------

type grantOut struct {
	Mailbox string `json:"mailbox"`
	Grantee string `json:"grantee"`
}

type grantIn struct {
	Owner   string `json:"owner"`
	Mailbox string `json:"mailbox"`
}

type purgeSummary struct {
	Mailboxes int        `json:"mailboxes"`
	Messages  int64      `json:"messages"`
	Bytes     int64      `json:"bytes"`
	Scripts   []string   `json:"scripts"`
	GrantsOut []grantOut `json:"grants_out"`
	GrantsIn  []grantIn  `json:"grants_in"`
	Accounts  []string   `json:"accounts"`
}

func (s purgeSummary) Touched() []string { return s.Accounts }

// purge removes everything the store holds for an account: its grants to
// others (their pointers), grants others gave it (their ACL entries), its
// learning jobs and finally every object of its namespaces. Identities
// stay with their owner (LDAP).
type purge struct{ kindBase }

func purgeInventory(u model.User) (purgeSummary, string, error) {
	s := purgeSummary{Scripts: []string{}, GrantsOut: []grantOut{}, GrantsIn: []grantIn{}, Accounts: []string{}}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return s, "", err
	}

	s.Mailboxes = len(boxes)
	targets := make([]deleteTarget, 0, len(boxes))

	for _, mb := range boxes {
		t, err := targetOf(u, mb)
		if err != nil {
			return s, "", err
		}

		targets = append(targets, t)
		s.Messages += t.Messages
		s.Bytes += t.Bytes

		for grantee, rights := range mb.ACL {
			if grantee != identAnyone && rights != "" {
				s.GrantsOut = append(s.GrantsOut, grantOut{mb.Name, grantee})
				s.Accounts = appendUnique(s.Accounts, grantee)
			}
		}
	}

	refs, err := u.ListShared()
	if err != nil {
		return s, "", err
	}

	for _, ref := range refs {
		s.GrantsIn = append(s.GrantsIn, grantIn{ref.Owner, ref.Mailbox})
		s.Accounts = appendUnique(s.Accounts, ref.Owner)
	}

	names, active, err := u.ListScripts()
	if err != nil {
		return s, "", err
	}

	s.Scripts = append(s.Scripts, names...)

	fp, err := fingerprint(u, targets)

	return s, fp + " " + strings.Join(names, ",") + " " + active, err
}

func (k *purge) Plan(_ context.Context, _ *Env, u model.User, _ string, _ json.RawMessage) (any, string, error) {
	return purgeInventory(u)
}

// purgeCheckpoint records the finished phases.
type purgeCheckpoint struct {
	Phase   int `json:"phase"`
	Removed int `json:"removed"`
}

func (k *purge) Run(_ context.Context, run *Run) (any, error) {
	var cp purgeCheckpoint

	if !run.LoadCheckpoint(&cp) {
		if _, fp, err := purgeInventory(run.User); err != nil {
			return nil, err
		} else if fp != run.Plan.Fingerprint {
			return nil, ErrDrift
		}
	}

	var plan purgeSummary

	_ = json.Unmarshal(run.Plan.Summary, &plan)

	steps := []func(*Run, purgeSummary, *purgeCheckpoint) error{revokeGrantsOut, revokeGrantsIn, purgeLearning, purgeObjects}

	for cp.Phase < len(steps) {
		if err := run.Fence(); err != nil {
			return cp, err
		}

		if err := steps[cp.Phase](run, plan, &cp); err != nil {
			return cp, err
		}

		cp.Phase++

		if err := run.Checkpoint(cp); err != nil {
			return cp, err
		}
	}

	return cp, nil
}

// revokeGrantsOut removes the account's ACL entries for others (their
// pointers follow).
func revokeGrantsOut(run *Run, plan purgeSummary, _ *purgeCheckpoint) error {
	mod, err := capability[model.ACLModifier](run.User, "change ACLs")
	if err != nil {
		return err
	}

	for _, g := range plan.GrantsOut {
		if _, _, err := mod.ModifyACL(g.Mailbox, g.Grantee, -1, func(string) string { return "" }); err != nil &&
			!errors.Is(err, model.ErrNoMailbox) && !errors.Is(err, model.ErrPointerSync) {
			run.Partial("revoke %s on %s: %v", g.Grantee, g.Mailbox, err)
		}
	}

	return nil
}

// revokeGrantsIn removes the ACL entries others gave the account.
func revokeGrantsIn(run *Run, plan purgeSummary, _ *purgeCheckpoint) error {
	for _, g := range plan.GrantsIn {
		if err := revokeAt(run.Env, g, run.Job.Account); err != nil {
			run.Partial("revoke at %s/%s: %v", g.Owner, g.Mailbox, err)
		}
	}

	return nil
}

func revokeAt(env *Env, g grantIn, account string) error {
	o, err := env.Accounts.User(g.Owner)
	if err != nil {
		return err
	}
	defer o.Close()

	mod, err := capability[model.ACLModifier](o, "change ACLs")
	if err != nil {
		return err
	}

	_, _, err = mod.ModifyACL(g.Mailbox, account, -1, func(string) string { return "" })
	if errors.Is(err, model.ErrNoMailbox) || errors.Is(err, model.ErrPointerSync) {
		return nil
	}

	return err
}

func purgeLearning(run *Run, _ purgeSummary, _ *purgeCheckpoint) error {
	if run.Env.Learning == nil {
		return nil
	}

	n, err := run.Env.Learning.PurgeOwner(run.Job.Account)
	if err != nil {
		run.Partial("learning jobs: %v", err)
	}

	run.Env.Log.Info("purge: learning jobs removed", "account", run.Job.Account, "count", n)

	return nil
}

func purgeObjects(run *Run, _ purgeSummary, cp *purgeCheckpoint) error {
	pg, err := capability[model.NamespacePurger](run.User, "purge")
	if err != nil {
		return err
	}

	// The registry with the maintenance lock goes last; renewing the lock
	// after that would fail and stop the job.
	run.DropLock()

	n, err := pg.PurgeNamespace(run.Fence)
	cp.Removed += n

	return err
}
