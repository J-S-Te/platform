package accesssubject

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
)

type repositoryStub struct{ subject domain.SessionSubject }

func (r repositoryStub) FindClient(context.Context, string, time.Time) (domain.Client, error) {
	return domain.Client{ID: "client-id", TenantID: r.subject.TenantID}, nil
}
func (r repositoryStub) ResolveSessionSubject(context.Context, string, time.Time) (domain.SessionSubject, error) {
	return r.subject, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func TestResolveAccessTokenSubjectCarriesOnlyPublicSessionLoginIP(t *testing.T) {
	for _, test := range []struct{ name, ip, want string }{
		{name: "public IPv4", ip: "203.0.113.9", want: "203.0.113.9"},
		{name: "private IPv4 omitted", ip: "172.18.0.2", want: ""},
		{name: "missing omitted", ip: "", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver, err := New(repositoryStub{subject: domain.SessionSubject{TenantID: "tenant-a", SessionID: "session-a", UserID: "user-a", LoginIP: net.ParseIP(test.ip)}}, fixedClock{now: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			subject, err := resolver.ResolveAccessTokenSubject(context.Background(), "client-a", "session-a", "user-a")
			if err != nil || subject.LoginIP != test.want {
				t.Fatalf("subject=%#v err=%v", subject, err)
			}
		})
	}
}
