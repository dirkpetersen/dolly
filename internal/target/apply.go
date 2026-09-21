package target

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/planner"
)

// Outcome is what happened to one planned operation.
type Outcome int

// Outcomes.
const (
	Applied     Outcome = iota // the server accepted the operation
	AlreadyDone                // the server said it was already in place (a re-run after a crash)
	Skipped                    // an operation it depends on failed or was skipped
	Failed                     // the server rejected it
	NotRun                     // the run was stopped (signal or run_timeout) before it
)

func (o Outcome) String() string {
	return [...]string{"applied", "already in place", "skipped", "failed", "not run"}[o]
}

// OpResult is the outcome of one operation.
type OpResult struct {
	Step    int // 1-based position in the plan, as the plan prints it
	Op      planner.Op
	Outcome Outcome
	Err     error  // Failed: the server's error
	Blocker int    // Skipped: the step whose failure caused the skip
	Key     string // Skipped: the dependency key that was broken
}

// Result is the outcome of applying a plan.
type Result struct {
	Ops                                           []OpResult
	Applied, AlreadyDone, Skipped, Failed, NotRun int
	// Stopped is the context error if the run was stopped before the end.
	Stopped error
}

// OK reports whether every operation was applied (or already in place).
func (r *Result) OK() bool {
	return r.Failed == 0 && r.Skipped == 0 && r.NotRun == 0 && r.Stopped == nil
}

// Apply executes the plan's operations in plan order against c. The planner
// orders them crash-safely; Apply never reorders them.
//
// A rejected operation doesn't stop the run: it is logged with its step and
// DN, and the run continues. Only operations that depend on it are skipped
// (planner.Op Needs and Provides, see internal/planner/deps.go), for
// example the entry add after its ownership record failed, or a
// roleOccupant delete after its member delete failed. Once ctx ends
// (SIGTERM, SIGINT, run_timeout), Apply stops before the next operation.
//
// On a re-run after a crash, entryAlreadyExists on an add whose planned
// content is present, attributeOrValueExists on a single-value add, and
// noSuchAttribute on a single-value delete count as already done. A value
// rename (delete old, add new) that the server has already carried out
// itself, as the refint overlay does for DN values after a modrdn, counts as
// already done too; see renameValue.
//
// log receives one line per failure and skip; with debug, one per applied
// operation too.
func Apply(ctx context.Context, c Conn, plan *planner.Plan, log io.Writer, debug bool) *Result {
	res := &Result{}
	broken := map[string]int{} // dependency key -> step that broke it
	for i, op := range plan.Ops {
		r := OpResult{Step: i + 1, Op: op}
		switch key, blocker := firstBroken(op.Needs, broken); {
		case ctx.Err() != nil:
			r.Outcome = NotRun
			if res.Stopped == nil {
				res.Stopped = ctx.Err()
				fmt.Fprintf(log, "dolly: run stopped (%v) before step %d; %d operations not run\n", ctx.Err(), r.Step, len(plan.Ops)-i)
			}
		case key != "":
			r.Outcome, r.Key, r.Blocker = Skipped, key, blocker
			breakKeys(op.Provides, blocker, broken)
			fmt.Fprintf(log, "dolly: step %d skipped (step %d failed): %s\n", r.Step, blocker, op)
		default:
			done, note, err := applyOp(c, op)
			if note != "" {
				note = " (" + note + ")"
			}
			switch {
			case err != nil:
				r.Outcome, r.Err = Failed, err
				breakKeys(op.Provides, r.Step, broken)
				fmt.Fprintf(log, "dolly: step %d failed: %s: %v\n", r.Step, op, err)
			case done:
				r.Outcome = AlreadyDone
				if debug {
					fmt.Fprintf(log, "debug: step %d already in place%s: %s\n", r.Step, note, op)
				}
			default:
				r.Outcome = Applied
				if debug {
					fmt.Fprintf(log, "debug: step %d applied%s: %s\n", r.Step, note, op)
				} else if note != "" {
					fmt.Fprintf(log, "dolly: step %d applied%s: %s\n", r.Step, note, op)
				}
			}
		}
		res.count(r.Outcome)
		res.Ops = append(res.Ops, r)
	}
	return res
}

func (r *Result) count(o Outcome) {
	switch o {
	case Applied:
		r.Applied++
	case AlreadyDone:
		r.AlreadyDone++
	case Skipped:
		r.Skipped++
	case Failed:
		r.Failed++
	case NotRun:
		r.NotRun++
	}
}

func firstBroken(needs []string, broken map[string]int) (string, int) {
	for _, k := range needs {
		if s, ok := broken[k]; ok {
			return k, s
		}
	}
	return "", 0
}

func breakKeys(keys []string, step int, broken map[string]int) {
	for _, k := range keys {
		if _, ok := broken[k]; !ok {
			broken[k] = step
		}
	}
}

// applyOp performs one operation. done reports that the server said it was
// already in place; note, if set, says how the operation was carried out.
func applyOp(c Conn, op planner.Op) (done bool, note string, err error) {
	switch op.Kind {
	case planner.RenameMember:
		return renameValue(c, op.DN, op.Attr, op.Old, op.Value)
	case planner.ModifyEntry, planner.UpdateRecord:
		if attr, old, value, ok := valueRename(op.Changes); ok {
			return renameValue(c, op.DN, attr, old, value)
		}
	}
	done, err = applySimple(c, op)
	return done, "", err
}

// valueRename reports whether changes are exactly one value rename: a
// single-value delete followed by a single-value add of the same attribute
// (roleOccupant fix-ups after a user rename).
func valueRename(changes []planner.Change) (attr, old, value string, ok bool) {
	if len(changes) != 2 {
		return "", "", "", false
	}
	d, a := changes[0], changes[1]
	if d.Type != planner.Delete || a.Type != planner.Add || !strings.EqualFold(d.Attr, a.Attr) ||
		len(d.Values) != 1 || len(a.Values) != 1 {
		return "", "", "", false
	}
	return d.Attr, d.Values[0], a.Values[0], true
}

// renameValue replaces one value of attr with another in a single modify,
// so the value is never missing in between. If the server answers
// noSuchAttribute or attributeOrValueExists, it re-reads the attribute:
// the server may have done the rename itself (the refint overlay rewrites
// member, roleOccupant, and seeAlso values after a modrdn), or half of it.
//
//   - old gone, new present: already done;
//   - old gone, new gone: the owned value was lost; add the new value;
//   - old present, new present: delete the old value;
//   - otherwise the original error stands.
func renameValue(c Conn, dn, attr, old, value string) (done bool, note string, err error) {
	req := ldap.NewModifyRequest(dn, nil)
	req.Delete(attr, []string{old})
	req.Add(attr, []string{value})
	err = c.Modify(req)
	if err == nil || !(ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchAttribute) ||
		ldap.IsErrorWithCode(err, ldap.LDAPResultAttributeOrValueExists)) {
		return false, "", err
	}
	if sameValue(attr, old, value) {
		// A case-only rename can't be told apart by re-reading.
		return false, "", err
	}
	e, rerr := readEntry(c, dn, attr)
	switch {
	case rerr != nil:
		return false, "", fmt.Errorf("%w (re-reading %s: %v)", err, attr, rerr)
	case e == nil:
		return false, "", err
	}
	have := e.GetEqualFoldAttributeValues(attr)
	hasOld, hasNew := containsValue(attr, have, old), containsValue(attr, have, value)
	var fix *ldap.ModifyRequest
	switch {
	case !hasOld && hasNew:
		return true, "already renamed by the server (refint?)", nil
	case !hasOld && !hasNew:
		// The value was removed concurrently. Don't recreate it: it may be a
		// local member, and an owned one is re-added by the next run anyway.
		return true, "the old value was already gone; nothing to rename", nil
	case hasOld && hasNew:
		fix = ldap.NewModifyRequest(dn, nil)
		fix.Delete(attr, []string{old})
		note = "the new value was already present; deleted the old value"
	default:
		return false, "", err
	}
	if ferr := c.Modify(fix); ferr != nil {
		return false, "", fmt.Errorf("%w (then: %v)", err, ferr)
	}
	return false, note, nil
}

// sameValue reports whether a and b match under attr's equality rule as
// containsValue applies it.
func sameValue(attr, a, b string) bool {
	if dnAttrs[strings.ToLower(attr)] {
		return model.DNEqual(a, b)
	}
	return a == b
}

// applySimple performs every operation other than a value rename.
func applySimple(c Conn, op planner.Op) (done bool, err error) {
	switch op.Kind {
	case planner.CreateContainer, planner.AddEntry, planner.AddRecord:
		req := ldap.NewAddRequest(op.DN, nil)
		for _, a := range op.Attrs {
			if len(a.Values) > 0 {
				req.Attribute(a.Name, a.Values)
			}
		}
		err := c.Add(req)
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return false, err
		}
		ok, rerr := hasContent(c, op.DN, op.Attrs)
		switch {
		case rerr != nil:
			return false, fmt.Errorf("%w (reading the existing entry: %v)", err, rerr)
		case !ok:
			return false, fmt.Errorf("%w (the existing entry's content differs from the plan)", err)
		}
		return true, nil

	case planner.ModifyEntry, planner.UpdateRecord:
		req := ldap.NewModifyRequest(op.DN, nil)
		for _, ch := range op.Changes {
			switch ch.Type {
			case planner.Replace:
				// spec: never replace a whole member or memberUid attribute.
				if strings.EqualFold(ch.Attr, config.AttrMember) || strings.EqualFold(ch.Attr, config.AttrMemberUID) {
					return false, fmt.Errorf("refusing to replace the whole %s attribute", ch.Attr)
				}
				req.Replace(ch.Attr, ch.Values)
			case planner.Add:
				req.Add(ch.Attr, ch.Values)
			case planner.Delete:
				req.Delete(ch.Attr, ch.Values)
			default:
				return false, fmt.Errorf("unknown change type %q", ch.Type)
			}
		}
		err := c.Modify(req)
		if len(op.Changes) == 1 && len(op.Changes[0].Values) == 1 {
			return idempotent(err, op.Changes[0].Type)
		}
		return false, err

	case planner.ModRDN:
		return false, c.ModifyDN(ldap.NewModifyDNRequest(op.DN, op.NewRDN, true, ""))

	case planner.DeleteEntry, planner.DeleteRecord:
		return false, c.Del(ldap.NewDelRequest(op.DN, nil))

	case planner.AddMember, planner.AddPlaceholder:
		req := ldap.NewModifyRequest(op.DN, nil)
		req.Add(op.Attr, []string{op.Value})
		return idempotent(c.Modify(req), planner.Add)

	case planner.DeleteMember, planner.DeletePlaceholder:
		req := ldap.NewModifyRequest(op.DN, nil)
		req.Delete(op.Attr, []string{op.Value})
		return idempotent(c.Modify(req), planner.Delete)
	}
	return false, fmt.Errorf("unknown operation %q", op.Kind)
}

// idempotent maps the "already done" answers of a single-value modify to
// success: attributeOrValueExists for an add, noSuchAttribute for a delete.
func idempotent(err error, t planner.ChangeType) (bool, error) {
	switch {
	case err == nil:
		return false, nil
	case t == planner.Add && ldap.IsErrorWithCode(err, ldap.LDAPResultAttributeOrValueExists):
		return true, nil
	case t == planner.Delete && ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchAttribute):
		return true, nil
	}
	return false, err
}

// hasContent reports whether the entry at dn holds every planned value.
// Extra values on the server don't matter. objectClass values compare
// case-insensitively and DN-valued attributes as DNs.
func hasContent(c Conn, dn string, attrs []planner.Attribute) (bool, error) {
	var names []string
	for _, a := range attrs {
		names = append(names, a.Name)
	}
	e, err := readEntry(c, dn, names...)
	if err != nil || e == nil {
		return false, err
	}
	for _, a := range attrs {
		have := e.GetEqualFoldAttributeValues(a.Name)
		for _, v := range a.Values {
			if !containsValue(a.Name, have, v) {
				return false, nil
			}
		}
	}
	return true, nil
}

var dnAttrs = map[string]bool{"member": true, "roleoccupant": true, "seealso": true}

func containsValue(attr string, have []string, v string) bool {
	for _, h := range have {
		switch {
		case dnAttrs[strings.ToLower(attr)] && model.DNEqual(h, v):
			return true
		case strings.EqualFold(attr, "objectClass") && strings.EqualFold(h, v):
			return true
		case h == v:
			return true
		}
	}
	return false
}

// Print writes the result: the counts, every failed and skipped operation,
// and which groups were created or changed, with the members added (+) and
// removed (-).
func (r *Result) Print(w io.Writer) {
	fmt.Fprintf(w, "\nResult: %d applied, %d already in place, %d skipped because an earlier step failed, %d failed",
		r.Applied, r.AlreadyDone, r.Skipped, r.Failed)
	if r.NotRun > 0 {
		fmt.Fprintf(w, ", %d not run (stopped: %v)", r.NotRun, r.Stopped)
	}
	fmt.Fprintln(w)
	var failed, skipped []OpResult
	for _, o := range r.Ops {
		switch o.Outcome {
		case Failed:
			failed = append(failed, o)
		case Skipped:
			skipped = append(skipped, o)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(w, "\nFailed (%d)\n", len(failed))
		for _, o := range failed {
			fmt.Fprintf(w, "  step %d %s\n    %v\n", o.Step, o.Op, o.Err)
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(w, "\nSkipped (%d)\n", len(skipped))
		for _, o := range skipped {
			fmt.Fprintf(w, "  step %d %s\n    needs step %d, which failed (%s)\n", o.Step, o.Op, o.Blocker, o.Key)
		}
	}
	created, changed := r.Groups()
	if len(created) > 0 {
		fmt.Fprintf(w, "\nCreated groups (%d)\n", len(created))
		for _, g := range created {
			fmt.Fprintf(w, "  %s (%d members)\n", g.Name, g.Members)
		}
	}
	if len(changed) > 0 {
		fmt.Fprintf(w, "\nChanged groups (%d)\n", len(changed))
		for _, g := range changed {
			fmt.Fprintf(w, "  %s +%d -%d", g.Name, len(g.Added), len(g.Removed))
			if len(g.Renamed) > 0 {
				fmt.Fprintf(w, " (%d renamed)", len(g.Renamed))
			}
			var names []string
			for _, u := range g.Added {
				names = append(names, "+"+u)
			}
			for _, u := range g.Removed {
				names = append(names, "-"+u)
			}
			if len(names) > 0 {
				fmt.Fprintf(w, ": %s", strings.Join(names, " "))
			}
			fmt.Fprintln(w)
		}
	}
}

// GroupChange summarizes what a run did to one group.
type GroupChange struct {
	Name                    string   // the group's RDN value (cn)
	Members                 int      // created groups: initial members
	Added, Removed, Renamed []string // changed groups: member uids, sorted
}

// Groups lists the groups the applied operations created, with their
// initial member count, and the groups whose members changed, with the uids
// added, removed, and renamed. Only operations the server accepted (or
// that were already in place) count; the placeholder doesn't.
func (r *Result) Groups() (created, changed []GroupChange) {
	type sets struct {
		name                    string
		added, removed, renamed map[string]string
	}
	byGroup := map[string]*sets{}
	var order []string
	for _, o := range r.Ops {
		if o.Outcome != Applied && o.Outcome != AlreadyDone {
			continue
		}
		op := o.Op
		switch op.Kind {
		case planner.AddEntry:
			if op.Section != planner.SectionGroups {
				continue
			}
			n := 0
			for _, a := range op.Attrs {
				if strings.EqualFold(a.Name, config.AttrMember) {
					n = len(a.Values)
					break
				}
				if strings.EqualFold(a.Name, config.AttrMemberUID) {
					n = len(a.Values)
				}
			}
			created = append(created, GroupChange{Name: model.RDNValue(op.DN), Members: n})
		case planner.AddMember, planner.DeleteMember, planner.RenameMember:
			k := model.MustDNKey(op.DN)
			s := byGroup[k]
			if s == nil {
				s = &sets{added: map[string]string{}, removed: map[string]string{}, renamed: map[string]string{}}
				byGroup[k] = s
				order = append(order, k)
			}
			s.name = model.RDNValue(op.DN)
			uid := op.Value
			if strings.EqualFold(op.Attr, config.AttrMember) {
				uid = model.RDNValue(op.Value)
			}
			switch op.Kind {
			case planner.AddMember:
				s.added[strings.ToLower(uid)] = uid
			case planner.DeleteMember:
				s.removed[strings.ToLower(uid)] = uid
			default:
				s.renamed[strings.ToLower(uid)] = uid
			}
		}
	}
	for _, k := range order {
		s := byGroup[k]
		changed = append(changed, GroupChange{Name: s.name, Added: values(s.added), Removed: values(s.removed), Renamed: values(s.renamed)})
	}
	sort.SliceStable(created, func(i, j int) bool { return strings.ToLower(created[i].Name) < strings.ToLower(created[j].Name) })
	sort.SliceStable(changed, func(i, j int) bool { return strings.ToLower(changed[i].Name) < strings.ToLower(changed[j].Name) })
	return created, changed
}

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, k := range model.SortedKeys(m) {
		out = append(out, m[k])
	}
	return out
}
