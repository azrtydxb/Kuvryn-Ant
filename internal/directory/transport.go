package directory

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"time"

	"github.com/go-ldap/ldap/v3"
)

type ldapConnection struct{ *ldap.Conn }

func (c *ldapConnection) Search(ctx context.Context, req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Async iteration bounds accumulated results even if the server ignores SizeLimit.
	response := c.Conn.SearchAsync(ctx, req, 0)
	out := &ldap.SearchResult{}
	for response.Next() {
		if response.Referral() != "" {
			return nil, ErrUnavailable
		}
		e := response.Entry()
		if e == nil {
			continue
		}
		if len(out.Entries) >= req.SizeLimit {
			return nil, ErrUnavailable
		}
		size := len(e.DN)
		for _, a := range e.Attributes {
			size += len(a.Name)
			for _, v := range a.ByteValues {
				size += len(v)
			}
		}
		if size > 64*1024 {
			return nil, ErrUnavailable
		}
		out.Entries = append(out.Entries, e)
	}
	if response.Err() != nil || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	return out, nil
}
func dialLDAP(ctx context.Context, endpoint string, config *tls.Config) (Connection, error) {
	u, e := url.Parse(endpoint)
	if e != nil || config == nil || config.InsecureSkipVerify || config.ServerName == "" || (u.Scheme != "ldap" && u.Scheme != "ldaps") {
		return nil, ErrConfig
	}
	port := u.Port()
	if port == "" {
		port = "389"
		if u.Scheme == "ldaps" {
			port = "636"
		}
	}
	raw, e := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if e != nil {
		return nil, ErrUnavailable
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	_ = raw.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	var conn *ldap.Conn
	if u.Scheme == "ldaps" {
		secure := tls.Client(raw, config.Clone())
		if e = secure.HandshakeContext(ctx); e != nil {
			_ = raw.Close()
			return nil, ErrUnavailable
		}
		conn = ldap.NewConn(secure, true)
		conn.Start()
	} else {
		conn = ldap.NewConn(raw, false)
		conn.Start()
		conn.SetTimeout(time.Until(deadline))
		if e = conn.StartTLS(config.Clone()); e != nil {
			_ = conn.Close()
			return nil, ErrUnavailable
		}
	}
	state, ok := conn.TLSConnectionState()
	if !ok || !state.HandshakeComplete || len(state.VerifiedChains) == 0 || ctx.Err() != nil {
		_ = conn.Close()
		return nil, ErrUnavailable
	}
	conn.SetTimeout(time.Until(deadline))
	return &ldapConnection{conn}, nil
}
