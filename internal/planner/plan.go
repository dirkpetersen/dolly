// Package planner turns an AD snapshot, a target snapshot, and Dolly's
// ownership records into an ordered plan of LDAP operations. It is a pure
// function: no I/O, no clock (the caller passes Now), deterministic output.
// `dolly sync --dry-run` prints exactly the plan a real run applies.
package planner

// Kind is the type of a planned operation.
type Kind string

// Operation kinds. Member operations add or delete a single value; Dolly
// never replaces a whole member or memberUid attribute.
const (
	CreateContainer   Kind = "create-container"   // Attrs: organizationalUnit under state_base
	AddEntry          Kind = "add-entry"          // Attrs: the full new entry
	ModifyEntry       Kind = "modify-entry"       // Changes: replace mapped attributes
	ModRDN            Kind = "modrdn"             // NewRDN, deleteoldrdn, same parent
	DeleteEntry       Kind = "delete-entry"       //
	AddMember         Kind = "add-member"         // Attr, Value
	DeleteMember      Kind = "delete-member"      // Attr, Value
	RenameMember      Kind = "rename-member"      // Attr, Old -> Value, one atomic modify (delete + add)
	AddPlaceholder    Kind = "add-placeholder"    // Attr=member, Value=empty_group_member
	DeletePlaceholder Kind = "delete-placeholder" // Attr=member, Value=empty_group_member
	AddRecord         Kind = "add-record"         // Attrs: organizationalRole entry
	UpdateRecord      Kind = "update-record"      // Changes: add/delete roleOccupant, replace seeAlso or description
	DeleteRecord      Kind = "delete-record"      //
)

// Section groups operations for printing.
type Section string

// Sections.
const (
	SectionContainers  Section = "Containers"
	SectionUsers       Section = "Users"
	SectionGroups      Section = "Groups"
	SectionMemberships Section = "Memberships"
	SectionRecords     Section = "Ownership records"
)

// ChangeType is an LDAP modify operation type.
type ChangeType string

// Change types.
const (
	Add     ChangeType = "add"
	Delete  ChangeType = "delete"
	Replace ChangeType = "replace" // an empty Values list deletes the attribute
)

// Attribute is a named list of values.
type Attribute struct {
	Name   string
	Values []string
}

// Change is one part of an LDAP modify request.
type Change struct {
	Type   ChangeType
	Attr   string
	Values []string
}

// Op is one planned LDAP operation. Operations must be applied in order;
// the order is what makes a run crash-safe.
type Op struct {
	Kind    Kind
	Section Section
	DN      string
	NewRDN  string      // ModRDN
	Attr    string      // member ops
	Value   string      // member ops: the value added, deleted, or renamed to
	Old     string      // RenameMember: the value replaced
	Attrs   []Attribute // AddEntry, AddRecord, CreateContainer
	Changes []Change    // ModifyEntry, UpdateRecord
	Reason  string
}

// WarningKind classifies run-summary warnings.
type WarningKind string

// Warning kinds.
const (
	WarnIgnored     WarningKind = "ignored"      // missing a required attribute
	WarnConflict    WarningKind = "conflict"     // target entry without a record, rename collision
	WarnDuplicate   WarningKind = "duplicate"    // duplicate uid/cn in AD, first one wins
	WarnDuplicateID WarningKind = "duplicate-id" // duplicate uidNumber/gidNumber
	WarnIDChanged   WarningKind = "id-changed"   // uidNumber/gidNumber changed
	WarnPending     WarningKind = "pending"      // groupOfNames group without members
	WarnInvalid     WarningKind = "invalid"      // mapping failed for an entry
)

// MissingMember is an AD group member that wasn't added to a target group
// because it has no entry on the target. It is debug-level information,
// not a warning: the summary shows only the count, and --debug lists them.
// The member is checked again on every run and added once its entry exists.
type MissingMember struct {
	Group string // target group DN
	UID   string // the member's uid
	DN    string // the member's target DN
	Why   string // e.g. "no entry on the target under ou=people,dc=local"
}

// Warning is one line in the run summary. Warnings don't stop the run.
type Warning struct {
	Kind    WarningKind
	Subject string // DN or name
	Message string
}

// Counts summarizes a plan.
type Counts struct {
	UsersAdded, UsersModified, UsersRenamed, UsersDeleted  int
	UsersDisabled, UsersReenabled, UsersMissing            int
	GroupsAdded, GroupsModified, GroupsRenamed             int
	MembersAdded, MembersRemoved, MembersRenamed           int
	RecordsAdded, RecordsUpdated, RecordsDeleted           int
	UnresolvedMembers, SkippedMembers, IgnoredEntries      int
	FilteredMembers                                        int // members in AD that match neither search filter
	ADUsers, ADGroups, TargetUsers, TargetGroups, Followed int
}

// Guard is the mass-deletion guard evaluation.
type Guard struct {
	MembershipRemovals int  // owned (group, user) pairs removed this run
	LocalRemovals      int  // local (group, user) pairs removed by a prune; reported, not guarded
	OwnedMemberships   int  // roleOccupant values before the run
	UserRemovals       int  // user entries deleted this run
	OwnedUsers         int  // user records before the run
	ADUsers, ADGroups  int  // in-scope objects AD returned
	UsersRead          bool // the AD users base was read (ADUsers is meaningful)
	MaxDeleteMin       int
	MaxDeletePercent   float64
	Tripped            bool
	Reasons            []string
}

// Plan is the planner's output.
type Plan struct {
	Mode     string // "sync" or "adopt"
	Users    bool   // users were planned
	Groups   bool   // groups were planned
	Ops      []Op
	Warnings []Warning
	// MissingMembers are the (group, member) pairs skipped because the
	// member has no entry on the target, sorted by group, then uid.
	MissingMembers []MissingMember
	Counts         Counts
	Guard          Guard
	// ADUsersRead and TargetUsersRead say whether the AD users base and the
	// target's users_base were read in full. A groups-only run reads
	// neither: it fetches AD members by DN and looks up the uids it needs.
	ADUsersRead, TargetUsersRead bool
}

// Empty reports whether the plan makes no changes.
func (p *Plan) Empty() bool { return len(p.Ops) == 0 }
