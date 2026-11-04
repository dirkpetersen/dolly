package ldapconn

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/go-ldap/ldap/v3"
)

// pager serves pages of 2 entries with a cookie until the last one.
type pager struct {
	total, calls int
	cookies      []string
}

func (p *pager) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	c := ldap.FindControl(req.Controls, ldap.ControlTypePaging).(*ldap.ControlPaging)
	p.cookies = append(p.cookies, string(c.Cookie))
	res := &ldap.SearchResult{}
	for i := 0; i < 2 && p.calls*2+i < p.total; i++ {
		res.Entries = append(res.Entries, ldap.NewEntry("cn=x", nil))
	}
	p.calls++
	next := ldap.NewControlPaging(2)
	if p.calls*2 < p.total {
		next.SetCookie([]byte{byte('0' + p.calls)})
	}
	res.Controls = []ldap.Control{next}
	return res, nil
}

// SearchPaged follows the cookies and counts each page as it arrives.
func TestSearchPagedCounts(t *testing.T) {
	p := &pager{total: 5}
	var n atomic.Int64
	got, err := SearchPaged(context.Background(), p, ldap.NewSearchRequest("dc=x", ldap.ScopeWholeSubtree, 0, 0, 0, false, "(objectClass=*)", nil, nil), 2, &n)
	if err != nil || len(got) != 5 || n.Load() != 5 || p.calls != 3 || p.cookies[0] != "" || p.cookies[1] != "1" || p.cookies[2] != "2" {
		t.Errorf("entries %d, count %d, calls %d, cookies %q, err %v", len(got), n.Load(), p.calls, p.cookies, err)
	}
}
