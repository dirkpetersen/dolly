package planner

import (
	"fmt"
	"io"
	"strings"
)

var sectionOrder = []Section{SectionContainers, SectionUsers, SectionGroups, SectionMemberships, SectionRecords}

var symbols = map[Kind]string{
	CreateContainer: "+", AddEntry: "+", AddMember: "+", AddPlaceholder: "+", AddRecord: "+",
	ModifyEntry: "~", UpdateRecord: "~", RenameMember: ">", ModRDN: ">",
	DeleteEntry: "-", DeleteMember: "-", DeletePlaceholder: "-", DeleteRecord: "-",
}

// Print writes a readable plan: operations grouped by section with their
// reasons, then warnings, the guard, and the summary counts. Each operation
// shows its step number, the order a real run applies them in.
func (p *Plan) Print(w io.Writer) {
	phases := []string{}
	if p.Users {
		phases = append(phases, "users")
	}
	if p.Groups {
		phases = append(phases, "groups")
	}
	c := p.Counts
	fmt.Fprintf(w, "Plan: dolly %s (%s)\n", p.Mode, strings.Join(phases, " and "))
	if p.ADUsersRead {
		fmt.Fprintf(w, "AD: %d users, %d groups in scope; %d out-of-scope objects followed; %d unresolved, %d filtered, and %d non-user members skipped\n",
			c.ADUsers, c.ADGroups, c.Followed, c.UnresolvedMembers, c.FilteredMembers, c.SkippedMembers)
	} else {
		fmt.Fprintf(w, "AD: %d groups in scope; users base not read, %d members fetched by DN; %d unresolved, %d filtered, and %d non-user members skipped\n",
			c.ADGroups, c.Followed, c.UnresolvedMembers, c.FilteredMembers, c.SkippedMembers)
	}
	users := fmt.Sprintf("%d user entries", c.TargetUsers)
	if !p.TargetUsersRead {
		users = "users_base not read"
	}
	fmt.Fprintf(w, "Target: %s, %d group entries; Dolly owns %d users and %d memberships\n",
		users, c.TargetGroups, p.Guard.OwnedUsers, p.Guard.OwnedMemberships)

	for _, sec := range sectionOrder {
		var steps []int
		for i, op := range p.Ops {
			if op.Section == sec {
				steps = append(steps, i)
			}
		}
		if len(steps) == 0 {
			continue
		}
		width := len(fmt.Sprint(len(p.Ops)))
		fmt.Fprintf(w, "\n%s (%d)\n", sec, len(steps))
		for _, i := range steps {
			op := p.Ops[i]
			fmt.Fprintf(w, "  %*d %s %s\n", width, i+1, symbols[op.Kind], describe(op))
			if op.Reason != "" {
				fmt.Fprintf(w, "  %*s     %s\n", width, "", op.Reason)
			}
		}
	}
	if len(p.Ops) == 0 {
		fmt.Fprintf(w, "\nNo changes.\n")
	}

	if len(p.Warnings) > 0 {
		fmt.Fprintf(w, "\nWarnings (%d)\n", len(p.Warnings))
		for _, x := range p.Warnings {
			fmt.Fprintf(w, "  %-12s %s: %s\n", x.Kind, x.Subject, x.Message)
		}
	}

	g := p.Guard
	fmt.Fprintf(w, "\nGuard: ")
	switch {
	case p.Mode == "adopt":
		fmt.Fprintf(w, "not applied (adopt removes nothing)\n")
	case g.Tripped:
		fmt.Fprintf(w, "TRIPPED, a real run would abort without writing (use --force to override)\n")
		for _, r := range g.Reasons {
			fmt.Fprintf(w, "  %s\n", r)
		}
	default:
		fmt.Fprintf(w, "ok (%d of %d owned memberships and %d of %d owned users removed; limit: more than %d and more than %g%%)\n",
			g.MembershipRemovals, g.OwnedMemberships, g.UserRemovals, g.OwnedUsers, g.MaxDeleteMin, g.MaxDeletePercent)
	}
	if p.Mode != "adopt" && g.LocalRemovals > 0 {
		fmt.Fprintf(w, "  plus %d local memberships removed by prune (not counted by the guard)\n", g.LocalRemovals)
	}

	fmt.Fprintf(w, "\nSummary: %d operations\n", len(p.Ops))
	fmt.Fprintf(w, "  users:       +%d ~%d >%d -%d (disabled %d, re-enabled %d, newly missing %d)\n",
		c.UsersAdded, c.UsersModified, c.UsersRenamed, c.UsersDeleted, c.UsersDisabled, c.UsersReenabled, c.UsersMissing)
	fmt.Fprintf(w, "  groups:      +%d ~%d >%d\n", c.GroupsAdded, c.GroupsModified, c.GroupsRenamed)
	fmt.Fprintf(w, "  memberships: +%d -%d (%d of them local, removed by a prune; %d values renamed)\n", c.MembersAdded, c.MembersRemoved, p.Guard.LocalRemovals, c.MembersRenamed)
	fmt.Fprintf(w, "  records:     +%d ~%d -%d\n", c.RecordsAdded, c.RecordsUpdated, c.RecordsDeleted)
	fmt.Fprintf(w, "  ignored:     %d entries missing required attributes\n", c.IgnoredEntries)
}

func describe(op Op) string {
	switch op.Kind {
	case CreateContainer:
		return "create " + op.DN
	case AddEntry:
		return "add " + op.DN + attrSummary(op.Attrs)
	case AddRecord:
		return "add record " + op.DN + attrSummary(op.Attrs)
	case ModifyEntry, UpdateRecord:
		var parts []string
		for _, c := range op.Changes {
			parts = append(parts, fmt.Sprintf("%s %s: %s", c.Type, c.Attr, valueList(c.Values)))
		}
		what := "modify "
		if op.Kind == UpdateRecord {
			what = "update record "
		}
		return what + op.DN + " [" + strings.Join(parts, "; ") + "]"
	case ModRDN:
		return "rename " + op.DN + " -> " + op.NewRDN
	case DeleteEntry:
		return "delete " + op.DN
	case DeleteRecord:
		return "delete record " + op.DN
	case AddMember, AddPlaceholder:
		return fmt.Sprintf("%s: add %s %s", op.DN, op.Attr, op.Value)
	case DeleteMember, DeletePlaceholder:
		return fmt.Sprintf("%s: delete %s %s", op.DN, op.Attr, op.Value)
	case RenameMember:
		return fmt.Sprintf("%s: %s %s -> %s", op.DN, op.Attr, op.Old, op.Value)
	}
	return string(op.Kind) + " " + op.DN
}

func attrSummary(attrs []Attribute) string {
	var parts []string
	for _, a := range attrs {
		if len(a.Values) > 3 {
			parts = append(parts, fmt.Sprintf("%s: %d values", a.Name, len(a.Values)))
			continue
		}
		parts = append(parts, a.Name+": "+valueList(a.Values))
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, "; ") + "]"
}

func valueList(v []string) string {
	if len(v) == 0 {
		return "(none)"
	}
	return strings.Join(v, ", ")
}
