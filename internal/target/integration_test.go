//go:build integration

package target

// Integration tests against a real OpenLDAP server: apply, the run lock,
// and cn=status end to end. They run with `go test -tags integration` and
// are skipped unless these are set:
//
//	DOLLY_IT_URL       ldap://localhost:389
//	DOLLY_IT_BIND_DN   cn=admin,dc=example,dc=org
//	DOLLY_IT_PASSWORD  admin
//	DOLLY_IT_BASE      dc=example,dc=org
//
// The server needs the core, cosine, and nis (RFC 2307) or rfc2307bis
// schemas. By default groups are groupOfNames + posixGroup, which needs
// rfc2307bis; set DOLLY_IT_SCHEMA=rfc2307 for memberUid-only groups on a
// server with nis.schema. With DOLLY_IT_REQUIRED set (CI), missing
// variables fail the tests instead of skipping them. The carol -> carol2
// rename in TestIntegrationApply must also pass on servers with the refint
// overlay (the CI image enables it), which rewrites member and roleOccupant
// values itself after the modrdn. Every test works in its own ou=dolly-it-<n>
// subtree under DOLLY_IT_BASE and deletes it afterwards.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
	"github.com/dirkpetersen/dolly/internal/model"
	"github.com/dirkpetersen/dolly/internal/planner"
	"github.com/dirkpetersen/dolly/internal/source"
)

type itEnv struct {
	cfg  *config.Config
	conn *ldap.Conn
	root string // ou=dolly-it-<n>,<base>
}

func setup(t *testing.T) *itEnv {
	t.Helper()
	url, bind, pw, base := os.Getenv("DOLLY_IT_URL"), os.Getenv("DOLLY_IT_BIND_DN"), os.Getenv("DOLLY_IT_PASSWORD"), os.Getenv("DOLLY_IT_BASE")
	if url == "" || bind == "" || pw == "" || base == "" {
		if os.Getenv("DOLLY_IT_REQUIRED") != "" {
			t.Fatal("DOLLY_IT_REQUIRED is set, but DOLLY_IT_URL, DOLLY_IT_BIND_DN, DOLLY_IT_PASSWORD, or DOLLY_IT_BASE is not")
		}
		t.Skip("DOLLY_IT_URL, DOLLY_IT_BIND_DN, DOLLY_IT_PASSWORD, and DOLLY_IT_BASE are not set")
	}
	root := fmt.Sprintf("ou=dolly-it-%d,%s", time.Now().UnixNano(), base)
	cfg := templateConfig(t)
	cfg.Target.URL, cfg.Target.StartTLS, cfg.Target.CAFile = url, false, ""
	cfg.Target.BindDN, cfg.Target.BindPassword, cfg.Target.BindPasswordFile = bind, pw, ""
	cfg.Target.UsersBase = "ou=people," + root
	cfg.Target.GroupsBase = "ou=group," + root
	cfg.Target.StateBase = "ou=dolly," + root
	cfg.Target.EmptyGroupMember = "cn=empty," + root
	if os.Getenv("DOLLY_IT_SCHEMA") == "rfc2307" {
		cfg.Mapping.Groups.ObjectClasses = []string{"posixGroup"}
		cfg.Mapping.Groups.Membership = []config.Membership{{Attribute: config.AttrMemberUID}}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	var conn *ldap.Conn
	var err error
	for i := 0; i < 30; i++ { // the server may still be starting
		if conn, err = DialWriter(cfg); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	env := &itEnv{cfg: cfg, conn: conn, root: root}
	for _, dn := range []string{root, cfg.Target.UsersBase, cfg.Target.GroupsBase} {
		_, val, _ := model.RDN(dn)
		req := ldap.NewAddRequest(dn, nil)
		req.Attribute("objectClass", []string{"organizationalUnit"})
		req.Attribute("ou", []string{val})
		if err := conn.Add(req); err != nil {
			t.Fatalf("add %s: %v", dn, err)
		}
	}
	t.Cleanup(func() {
		env.deleteTree(t)
		conn.Close()
	})
	return env
}

// deleteTree removes the test subtree, leaves first.
func (e *itEnv) deleteTree(t *testing.T) {
	res, err := ldapconn.SearchPaged(context.Background(), e.conn, ldap.NewSearchRequest(e.root, ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"1.1"}, nil), 500)
	if err != nil {
		t.Logf("cleanup: %v", err)
		return
	}
	var dns []string
	for _, x := range res {
		dns = append(dns, x.DN)
	}
	sort.Slice(dns, func(i, j int) bool { return strings.Count(dns[i], ",") > strings.Count(dns[j], ",") })
	for _, dn := range dns {
		if err := e.conn.Del(ldap.NewDelRequest(dn, nil)); err != nil {
			t.Logf("cleanup %s: %v", dn, err)
		}
	}
}

func (e *itEnv) values(t *testing.T, dn, attr string) []string {
	t.Helper()
	x, err := readEntry(e.conn, dn, attr)
	if err != nil {
		t.Fatal(err)
	}
	if x == nil {
		return nil
	}
	return x.GetEqualFoldAttributeValues(attr)
}

// plan reads the target like a real run and plans against the AD objects.
func (e *itEnv) plan(t *testing.T, ad []*model.ADObject) *planner.Plan {
	t.Helper()
	ctx := context.Background()
	fake := &source.Fake{}
	for _, o := range ad {
		if o.Kind == model.KindGroup {
			fake.InScopeGroups = append(fake.InScopeGroups, o)
		} else {
			fake.InScopeUsers = append(fake.InScopeUsers, o)
		}
	}
	snap, err := source.Read(ctx, fake, true)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Dial(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tgt, recs, err := r.Read(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := planner.Build(snap, tgt, recs, e.cfg, planner.Options{Users: true, Groups: true, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func adUser(n int, uid string) *model.ADObject {
	return &model.ADObject{GUID: fmt.Sprintf("00000000-0000-0000-0000-%012x", n), DN: "CN=" + uid + ",OU=People,DC=example,DC=edu",
		Kind: model.KindUser, InScope: true, UserAccountControl: 512,
		Attrs: map[string][]string{"uid": {uid}, "uidNumber": {fmt.Sprint(1000 + n)}, "gidNumber": {"100"}, "gecos": {"Zoë " + uid}}}
}

func adGroup(n int, name string, members ...*model.ADObject) *model.ADObject {
	g := &model.ADObject{GUID: fmt.Sprintf("00000000-0000-0000-0000-%012x", n), DN: "CN=" + name + ",OU=Groups,DC=example,DC=edu",
		Kind: model.KindGroup, InScope: true, Attrs: map[string][]string{"name": {name}, "gidNumber": {fmt.Sprint(5000 + n)}}}
	for _, m := range members {
		g.Members = append(g.Members, m.DN)
	}
	return g
}

func TestIntegrationApply(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if created, err := EnsureStateBase(ctx, e.conn, e.cfg.Target.StateBase); err != nil || !created {
		t.Fatalf("state_base: %v %v", created, err)
	}
	jdoe, bob, carol := adUser(1, "jdoe"), adUser(2, "bob"), adUser(3, "carol")
	ad := []*model.ADObject{jdoe, bob, carol, adGroup(10, "lab", jdoe, bob), adGroup(11, "staff", carol)}

	p1 := e.plan(t, ad)
	var log bytes.Buffer
	res := Apply(ctx, e.conn, p1, &log, true)
	if !res.OK() {
		var out bytes.Buffer
		res.Print(&out)
		t.Fatalf("first apply:\n%s\n%s", out.String(), log.String())
	}
	created, _ := res.Groups()
	if len(created) != 2 {
		t.Errorf("created groups: %+v", created)
	}

	// The same plan again, as after a crash at the very end: every add is
	// already in place (entryAlreadyExists with matching content).
	res = Apply(ctx, e.conn, p1, &log, false)
	if res.Failed != 0 || res.AlreadyDone != len(p1.Ops) {
		var out bytes.Buffer
		res.Print(&out)
		t.Errorf("re-apply:\n%s", out.String())
	}

	// Converged: a fresh read plans nothing.
	if p := e.plan(t, ad); !p.Empty() {
		var out bytes.Buffer
		p.Print(&out)
		t.Fatalf("not converged:\n%s", out.String())
	}

	// A member added locally on the server survives; bob leaves lab in
	// AD, carol joins it, and carol is renamed to carol2.
	lab := "cn=lab," + e.cfg.Target.GroupsBase
	local := ldap.NewModifyRequest(lab, nil)
	if e.cfg.Mapping.Groups.HasMember() {
		local.Add("member", []string{"uid=local1," + e.cfg.Target.UsersBase})
	}
	local.Add("memberUid", []string{"local1"})
	if err := e.conn.Modify(local); err != nil {
		t.Fatal(err)
	}
	carol.Attrs["uid"] = []string{"carol2"}
	ad = []*model.ADObject{jdoe, bob, carol, adGroup(10, "lab", jdoe, carol), adGroup(11, "staff", carol)}
	p2 := e.plan(t, ad)
	res = Apply(ctx, e.conn, p2, &log, false)
	if !res.OK() {
		var out bytes.Buffer
		res.Print(&out)
		t.Fatalf("second apply:\n%s\n%s", out.String(), log.String())
	}
	uids := strings.Join(sorted(e.values(t, lab, "memberUid")), ",")
	if uids != "carol2,jdoe,local1" {
		t.Errorf("lab memberUid: %s", uids)
	}
	if p := e.plan(t, ad); !p.Empty() {
		var out bytes.Buffer
		p.Print(&out)
		t.Errorf("not converged after the second apply:\n%s", out.String())
	}

	// The server's answers drive the error handling: a record under a
	// missing container is rejected (noSuchObject) and its entry skipped;
	// an existing value (attributeOrValueExists) and a missing one
	// (noSuchAttribute) count as already in place.
	bad := &planner.Plan{Ops: []planner.Op{
		{Kind: planner.AddRecord, DN: "cn=x,ou=missing," + e.cfg.Target.StateBase,
			Attrs:    []planner.Attribute{{Name: "objectClass", Values: []string{"organizationalRole"}}, {Name: "cn", Values: []string{"x"}}},
			Provides: []string{"seq:x"}},
		{Kind: planner.AddEntry, DN: "uid=x," + e.cfg.Target.UsersBase, Needs: []string{"seq:x"},
			Attrs: []planner.Attribute{{Name: "objectClass", Values: []string{"account"}}, {Name: "uid", Values: []string{"x"}}}},
		{Kind: planner.AddMember, DN: lab, Attr: "memberUid", Value: "jdoe"}, // already there
		{Kind: planner.DeleteMember, DN: lab, Attr: "memberUid", Value: "nobody"},
	}}
	res = Apply(ctx, e.conn, bad, &log, false)
	if res.Failed != 1 || res.Skipped != 1 || res.AlreadyDone != 2 {
		var out bytes.Buffer
		res.Print(&out)
		t.Errorf("error handling:\n%s", out.String())
	}
}

func sorted(v []string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}

func TestIntegrationLock(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	sb := e.cfg.Target.StateBase
	if _, err := EnsureStateBase(ctx, e.conn, sb); err != nil {
		t.Fatal(err)
	}
	opts := func(pid int, ttl time.Duration) LockOptions {
		return LockOptions{StateBase: sb, TTL: ttl, Command: "sync", Host: "it", PID: pid, Log: &bytes.Buffer{}}
	}
	l1, held, err := AcquireLock(ctx, e.conn, opts(1, time.Hour))
	if err != nil || l1 == nil || held != nil {
		t.Fatalf("acquire: %v %v", held, err)
	}
	// createTimestamp comes from the server and parses.
	info, err := ReadLock(e.conn, LockDN(sb))
	if err != nil || info == nil || time.Since(info.Created) > 5*time.Minute || time.Since(info.Created) < -5*time.Minute {
		t.Fatalf("read lock: %+v %v", info, err)
	}
	if _, held, _ := AcquireLock(ctx, e.conn, opts(2, time.Hour)); held == nil {
		t.Fatal("a second run took a fresh lock")
	}
	// With lock_ttl 0 every lock is stale: pid 3 breaks pid 1's lock.
	l3, held, err := AcquireLock(ctx, e.conn, opts(3, 0))
	if err != nil || l3 == nil || l3.Broke == nil {
		t.Fatalf("break stale: %v %v %v", l3, held, err)
	}
	if err := l1.Release(ctx); err == nil {
		t.Error("pid 1 released pid 3's lock")
	}
	if err := l3.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if info, _ := ReadLock(e.conn, LockDN(sb)); info != nil {
		t.Error("lock left behind")
	}
	if err := RemoveLock(e.conn, LockDN(sb)); err == nil {
		t.Error("removing a missing lock succeeded")
	}
}

func TestIntegrationStatus(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	sb := e.cfg.Target.StateBase
	if _, err := EnsureStateBase(ctx, e.conn, sb); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 21, 19, 0, 0, 0, time.UTC)
	if _, _, err := WriteStatus(ctx, e.conn, sb, RunStatus{End: t0, Failure: "AD unreachable: Zoë"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteStatus(ctx, e.conn, sb, RunStatus{End: t0.Add(time.Hour), Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(sorted(e.values(t, StatusDN(sb), "description")), "\n")
	want := "last-result=ok\nlast-run=2026-09-21T20:00:00Z\nlast-success=2026-09-21T20:00:00Z"
	if got != want {
		t.Errorf("status:\n%s\nwant:\n%s", got, want)
	}
}
