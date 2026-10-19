package directory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
)

type fixture struct {
	bind   func(string, string) error
	search func(*ldap.SearchRequest) (*ldap.SearchResult, error)
	mu     sync.Mutex
	closed bool
}

func (f *fixture) Bind(dn, pw string) error {
	if f.bind != nil {
		return f.bind(dn, pw)
	}
	return nil
}
func (f *fixture) Search(_ context.Context, r *ldap.SearchRequest) (*ldap.SearchResult, error) {
	return f.search(r)
}
func (f *fixture) Close() error { f.mu.Lock(); defer f.mu.Unlock(); f.closed = true; return nil }
func config() Config {
	return Config{ProviderID: "corp", URL: "ldaps://directory.example", BindDN: "cn=reader,dc=example", UserBaseDN: "ou=users,dc=example", GroupBaseDN: "ou=groups,dc=example", LoginAttribute: "uid", UserIDAttribute: "entryUUID", GroupIDAttribute: "entryUUID", UserClass: "person", GroupClass: "groupOfNames", MemberAttribute: "member"}
}
func entry(dn, id string) *ldap.Entry {
	return ldap.NewEntry(dn, map[string][]string{"entryUUID": {id}})
}

const userDN = "uid=alice,ou=users,dc=example"

func group(n string) *ldap.Entry                       { return entry("cn="+n+",ou=groups,dc=example", n) }
func result(entries ...*ldap.Entry) *ldap.SearchResult { return &ldap.SearchResult{Entries: entries} }
func provider(t *testing.T, c Config, f *fixture) *Provider {
	t.Helper()
	p, e := New(c, func(context.Context) ([]byte, time.Time, error) {
		return []byte("service-secret"), time.Now().Add(time.Minute), nil
	}, func(_ context.Context, _ string, tc *tls.Config) (Connection, error) {
		if tc.InsecureSkipVerify || tc.ServerName != "directory.example" {
			t.Fatal("unsafe TLS")
		}
		return f, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestAuthenticationMappingAndEscaping(t *testing.T) {
	c := config()
	c.GroupRoles = map[string][]string{base64.RawURLEncoding.EncodeToString([]byte("dev")): {"reader"}}
	calls := 0
	f := &fixture{search: func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
		calls++
		if r.SizeLimit <= 0 || r.TimeLimit <= 0 || r.DerefAliases != ldap.NeverDerefAliases {
			t.Fatal("unbounded search")
		}
		switch calls {
		case 1:
			if !strings.Contains(r.Filter, `uid=alice\2a\29\28uid=\2a\29`) {
				t.Fatal(r.Filter)
			}
			return result(entry(userDN, "immutable-user")), nil
		case 2:
			return result(group("dev")), nil
		default:
			return result(), nil
		}
	}}
	p := provider(t, c, f)
	c.GroupRoles[base64.RawURLEncoding.EncodeToString([]byte("dev"))][0] = "admin"
	pw := []byte("password")
	got, e := p.Authenticate(context.Background(), "alice*)(uid=*)", pw)
	if e != nil {
		t.Fatal(e)
	}
	if got.Subject != base64.RawURLEncoding.EncodeToString([]byte("immutable-user")) || len(got.RoleIDs) != 1 || got.RoleIDs[0] != "reader" {
		t.Fatalf("%+v", got)
	}
	for _, b := range pw {
		if b != 0 {
			t.Fatal("password retained")
		}
	}
}
func TestFailClosed(t *testing.T) {
	for _, scenario := range []string{"cycle", "depth", "partial", "referral", "ambiguous", "missing-id", "duplicate-id", "invalid-password", "limit", "outside-base"} {
		t.Run(scenario, func(t *testing.T) {
			c := config()
			c.MaxDepth = 2
			c.MaxGroups = 2
			calls := 0
			f := &fixture{bind: func(dn, pw string) error {
				if scenario == "invalid-password" && dn == userDN {
					return errors.New("sensitive server diagnostic")
				}
				return nil
			}, search: func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
				calls++
				if calls == 1 {
					if scenario == "ambiguous" {
						return result(entry(userDN, "u"), entry(userDN, "v")), nil
					}
					if scenario == "missing-id" {
						return result(ldap.NewEntry(userDN, nil)), nil
					}
					return result(entry(userDN, "u")), nil
				}
				switch scenario {
				case "partial":
					return result(group("dev")), errors.New("size limit exceeded")
				case "referral":
					return &ldap.SearchResult{Referrals: []string{"ldap://evil"}}, nil
				case "cycle":
					return result(group("dev")), nil
				case "depth":
					return result(group(strings.Repeat("a", calls))), nil
				case "duplicate-id":
					return result(entry("cn=a,ou=groups,dc=example", "same"), entry("cn=b,ou=groups,dc=example", "same")), nil
				case "limit":
					return result(group("a"), group("b"), group("c")), nil
				case "outside-base":
					return result(entry("cn=a,dc=evil", "a")), nil
				}
				return result(), nil
			}}
			got, e := provider(t, c, f).Authenticate(context.Background(), "alice", []byte("secret"))
			if e == nil || got.Subject != "" || len(got.RoleIDs) > 0 {
				t.Fatalf("accepted %s: %+v %v", scenario, got, e)
			}
			if strings.Contains(e.Error(), "sensitive") {
				t.Fatal("diagnostic leaked")
			}
		})
	}
}
func TestDepthBoundaryAndNoAutomaticRoles(t *testing.T) {
	calls := 0
	f := &fixture{search: func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
		calls++
		if calls == 1 {
			return result(entry(userDN, "u")), nil
		}
		if calls <= 11 {
			return result(group(strings.Repeat("a", calls))), nil
		}
		return result(), nil
	}}
	got, e := provider(t, config(), f).Authenticate(context.Background(), "alice", []byte("pw"))
	if e != nil || len(got.GroupIDs) != 10 || len(got.RoleIDs) != 0 {
		t.Fatalf("%+v %v", got, e)
	}
}
func TestSecretExpiryAndClearing(t *testing.T) {
	secret := []byte("expired-secret")
	binds := 0
	f := &fixture{bind: func(string, string) error { binds++; return nil }}
	p := provider(t, config(), f)
	p.secret = func(context.Context) ([]byte, time.Time, error) { return secret, time.Now().Add(-time.Second), nil }
	_, e := p.Authenticate(context.Background(), "a", []byte("pw"))
	if e == nil || binds != 0 {
		t.Fatal("expired secret used")
	}
	for _, b := range secret {
		if b != 0 {
			t.Fatal("secret retained")
		}
	}
}
func TestConfigRejectsUnsafe(t *testing.T) {
	for _, change := range []func(*Config){func(c *Config) { c.URL = "http://example" }, func(c *Config) { c.URL = "ldap://u:p@example" }, func(c *Config) { c.LoginAttribute = "uid)(x=*" }, func(c *Config) { c.MaxDepth = 11 }, func(c *Config) { c.Timeout = time.Minute }, func(c *Config) { c.CAPEM = []byte("bad") }, func(c *Config) { c.UserIDAttribute = "cn" }} {
		c := config()
		change(&c)
		if _, e := New(c, func(context.Context) ([]byte, time.Time, error) { return nil, time.Time{}, nil }, nil); e == nil {
			t.Fatal("accepted unsafe config")
		}
	}
}

func TestTLSVerification(t *testing.T) {
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := certServer.TLS.Certificates[0]
	leaf := certServer.Certificate()
	certServer.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))
	for _, mode := range []string{"ldaps", "ldap"} {
		for _, test := range []struct {
			name       string
			roots      *x509.CertPool
			serverName string
			wantOK     bool
		}{{"trusted", roots, "127.0.0.1", true}, {"wrong-name", roots, "wrong.example", false}, {"untrusted", x509.NewCertPool(), "127.0.0.1", false}} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				listener, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				defer listener.Close()
				done := make(chan struct{})
				go func() {
					defer close(done)
					raw, e := listener.Accept()
					if e != nil {
						return
					}
					defer raw.Close()
					_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
					if mode == "ldap" {
						var header [2]byte
						if _, e = io.ReadFull(raw, header[:]); e != nil {
							return
						}
						if header[1] >= 128 {
							return
						}
						if _, e = io.CopyN(io.Discard, raw, int64(header[1])); e != nil {
							return
						}
						_, _ = raw.Write([]byte{0x30, 0x0c, 0x02, 0x01, 0x01, 0x78, 0x07, 0x0a, 0x01, 0, 0x04, 0, 0x04, 0})
					}
					secure := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
					_ = secure.Handshake()
					_, _ = io.Copy(io.Discard, secure)
				}()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				conn, e := dialLDAP(ctx, mode+"://"+listener.Addr().String(), &tls.Config{RootCAs: test.roots, ServerName: test.serverName, MinVersion: tls.VersionTLS12})
				if conn != nil {
					_ = conn.Close()
				}
				if (e == nil) != test.wantOK {
					t.Fatalf("error %v wantOK %v", e, test.wantOK)
				}
				<-done
			})
		}
	}
}
func TestStartTLSRefusalNeverBinds(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 1024)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte{0x30, 0x0c, 0x02, 0x01, 0x01, 0x78, 0x07, 0x0a, 0x01, 0x35, 0x04, 0, 0x04, 0})
		n, _ := c.Read(buf)
		if n != 0 {
			t.Error("sent data after rejected StartTLS")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, e = dialLDAP(ctx, "ldap://"+listener.Addr().String(), &tls.Config{ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if e == nil {
		t.Fatal("accepted refusal")
	}
	<-done
}

func TestADSchemaAndBinaryIdentity(t *testing.T) {
	c := config()
	c.LoginAttribute = "sAMAccountName"
	c.UserClass = "user"
	c.GroupClass = "group"
	c.UserIDAttribute = "objectGUID"
	c.GroupIDAttribute = "objectGUID"
	guid := string([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 255})
	calls := 0
	f := &fixture{search: func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
		calls++
		if calls == 1 {
			if !strings.Contains(r.Filter, "(sAMAccountName=alice)") {
				t.Fatal(r.Filter)
			}
			return result(ldap.NewEntry(userDN, map[string][]string{"objectGUID": {guid}})), nil
		}
		return result(), nil
	}}
	got, e := provider(t, c, f).Authenticate(context.Background(), "alice", []byte("pw"))
	if e != nil || got.Subject != base64.RawURLEncoding.EncodeToString([]byte(guid)) {
		t.Fatalf("%+v %v", got, e)
	}
}
func TestHealthAndSearchBudget(t *testing.T) {
	f := &fixture{search: func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
		if r.Scope != ldap.ScopeBaseObject || r.SizeLimit != 1 {
			t.Fatal("unbounded health probe")
		}
		return result(entry(r.BaseDN, "base")), nil
	}}
	if e := provider(t, config(), f).Health(context.Background()); e != nil {
		t.Fatal(e)
	}
	c := config()
	c.MaxSearches = 1
	calls := 0
	f = &fixture{search: func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		calls++
		if calls == 1 {
			return result(entry(userDN, "u")), nil
		}
		return result(group("g")), nil
	}}
	if _, e := provider(t, c, f).Authenticate(context.Background(), "alice", []byte("pw")); !errors.Is(e, ErrGroups) {
		t.Fatal(e)
	}
}
func TestTransportCancellationDuringStartTLS(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	received := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		b := make([]byte, 1024)
		_, _ = c.Read(b)
		close(received)
		_, _ = c.Read(b)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { <-received; cancel() }()
	start := time.Now()
	_, e = dialLDAP(ctx, "ldap://"+listener.Addr().String(), &tls.Config{ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if e == nil || time.Since(start) > time.Second {
		t.Fatalf("cancellation failed: %v", e)
	}
	<-done
}

// nestedDirectory models alice -> inner -> outer; the answer to a membership
// search depends on which DN is the member.
func nestedDirectory(t *testing.T) *fixture {
	t.Helper()
	calls := 0
	return &fixture{search: func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
		calls++
		switch {
		case calls == 1:
			return result(entry(userDN, "immutable-user")), nil
		case strings.Contains(r.Filter, "(member="+ldap.EscapeFilter(userDN)+")"):
			return result(group("inner")), nil
		case strings.Contains(r.Filter, "(member="+ldap.EscapeFilter("cn=inner,ou=groups,dc=example")+")"):
			return result(group("outer")), nil
		}
		return result(), nil
	}}
}

func TestGroupNestingPolicy(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, tc := range map[string]struct {
		nesting string
		groups  []string
		roles   []string
	}{
		"default is nested": {"", []string{enc("inner"), enc("outer")}, []string{"outer-role"}},
		"nested":            {NestingNested, []string{enc("inner"), enc("outer")}, []string{"outer-role"}},
		"direct":            {NestingDirect, []string{enc("inner")}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			c := config()
			c.GroupNesting = tc.nesting
			c.GroupRoles = map[string][]string{enc("outer"): {"outer-role"}}
			got, e := provider(t, c, nestedDirectory(t)).Authenticate(context.Background(), "alice", []byte("password"))
			if e != nil {
				t.Fatal(e)
			}
			if strings.Join(got.GroupIDs, ",") != strings.Join(tc.groups, ",") || strings.Join(got.RoleIDs, ",") != strings.Join(tc.roles, ",") || !got.RolesMapped {
				t.Fatalf("%+v", got)
			}
		})
	}
	c := config()
	c.GroupNesting = "sideways"
	if _, e := New(c, func(context.Context) ([]byte, time.Time, error) { return nil, time.Time{}, nil }, nil); !errors.Is(e, ErrConfig) {
		t.Fatalf("unknown policy accepted: %v", e)
	}
	// Without any mapping the identity reports no role mapping at all.
	got, e := provider(t, config(), nestedDirectory(t)).Authenticate(context.Background(), "alice", []byte("password"))
	if e != nil || got.RolesMapped {
		t.Fatalf("%+v %v", got, e)
	}
}
