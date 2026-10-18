// Package auditsink forwards audit events to an operator-configured SIEM sink
// (RFC 5424 syslog over udp/tcp/tls, or HTTPS NDJSON). Delivery is
// at-least-once and ordered; progress is a durable checkpoint on the
// audit_sinks row. Credentials are stored only through internal/secrets and are
// never logged or returned.
package auditsink

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	KindSyslog = "syslog"
	KindHTTP   = "http"
	// Purpose is the encrypted_secrets purpose of a sink credential.
	Purpose = "audit-sink"
)

var ErrEndpoint = errors.New("invalid audit sink endpoint")

// ValidateEndpoint canonicalizes an endpoint for kind. HTTP sinks must be
// https URLs without credentials, query or fragment. Syslog sinks are
// udp://, tcp:// or tls:// host:port. Loopback, link-local, multicast and
// unspecified targets are rejected again at dial time after DNS resolution.
func ValidateEndpoint(kind, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return "", ErrEndpoint
	}
	switch kind {
	case KindHTTP:
		if u.Scheme != "https" {
			return "", ErrEndpoint
		}
	case KindSyslog:
		if (u.Scheme != "udp" && u.Scheme != "tcp" && u.Scheme != "tls") || u.Port() == "" || (u.Path != "" && u.Path != "/") {
			return "", ErrEndpoint
		}
		u.Path = ""
	default:
		return "", ErrEndpoint
	}
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil && forbiddenIP(ip) {
		return "", ErrEndpoint
	}
	return u.String(), nil
}

func forbiddenIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// dialer resolves and connects only to permitted addresses (private ranges are
// allowed: SIEM collectors commonly live inside the network).
func dialer(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrEndpoint
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return nil, ErrEndpoint
	}
	for _, a := range addrs {
		if forbiddenIP(a) {
			return nil, ErrEndpoint
		}
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(addrs[0].String(), port))
}

// Event is one audit record.
type Event struct {
	ID          int64           `json:"id"`
	Time        time.Time       `json:"time"`
	InstanceID  string          `json:"instance_id"`
	PrincipalID string          `json:"principal_id,omitempty"`
	Action      string          `json:"action"`
	ResourceID  string          `json:"resource_id,omitempty"`
	Details     json.RawMessage `json:"details"`
}

// Forwarder ships events for one instance.
type Forwarder struct {
	DB         *pgxpool.Pool
	InstanceID string
	Keys       *secrets.Keyring
	Interval   time.Duration // default 5s
	Batch      int           // default 200
	// Hostname identifies this process in syslog messages.
	Hostname string
	// Send overrides transport (tests).
	Send func(ctx context.Context, kind, endpoint, credential string, events []Event) error

	failures int
}

type config struct {
	kind, endpoint string
	secretRef      *string
	checkpoint     int64
}

// Run forwards until ctx ends. A failing sink backs off exponentially (to five
// minutes) without losing its place.
func (f *Forwarder) Run(ctx context.Context) {
	interval := f.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		n, err := f.RunOnce(ctx)
		delay := interval
		switch {
		case err != nil:
			f.failures++
			delay = min(interval<<min(f.failures, 8), 5*time.Minute)
		case n > 0:
			f.failures = 0
			delay = 0 // drain backlog promptly
		default:
			f.failures = 0
		}
		timer.Reset(max(delay, 10*time.Millisecond))
	}
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrEndpoint):
		return "endpoint_rejected"
	case errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrStoreInput):
		return "credential_unavailable"
	case strings.HasPrefix(err.Error(), "status "):
		return err.Error()
	default:
		return "delivery_failed"
	}
}

// RunOnce delivers at most one batch and reports how many events were sent.
func (f *Forwarder) RunOnce(ctx context.Context) (int, error) {
	batch := f.Batch
	if batch <= 0 {
		batch = 200
	}
	var c config
	var enabled bool
	err := f.DB.QueryRow(ctx, `SELECT kind,endpoint,secret_ref::text,enabled,checkpoint FROM audit_sinks WHERE instance_id=$1`, f.InstanceID).Scan(&c.kind, &c.endpoint, &c.secretRef, &enabled, &c.checkpoint)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !enabled {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := f.DB.Query(ctx, `SELECT id,created_at,COALESCE(principal_id::text,''),action,COALESCE(resource_id::text,''),details FROM audit_events WHERE instance_id=$1 AND id>$2 ORDER BY id LIMIT $3`, f.InstanceID, c.checkpoint, batch)
	if err != nil {
		return 0, err
	}
	var events []Event
	for rows.Next() {
		e := Event{InstanceID: f.InstanceID}
		if err = rows.Scan(&e.ID, &e.Time, &e.PrincipalID, &e.Action, &e.ResourceID, &e.Details); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	rows.Close()
	if rows.Err() != nil || len(events) == 0 {
		return 0, rows.Err()
	}
	credential := ""
	if c.secretRef != nil && f.Keys != nil {
		_, plain, openErr := (secrets.Store{DB: f.DB, Keys: f.Keys}).Open(ctx, f.InstanceID, *c.secretRef, Purpose)
		if openErr != nil {
			f.fail(ctx, openErr)
			return 0, openErr
		}
		credential = string(plain)
		clear(plain)
	}
	send := f.Send
	if send == nil {
		send = f.transport
	}
	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = send(sendCtx, c.kind, c.endpoint, credential, events); err != nil {
		f.fail(ctx, err)
		return 0, err
	}
	last := events[len(events)-1].ID
	// The checkpoint only moves forward and never past a concurrently reset value.
	_, err = f.DB.Exec(ctx, `UPDATE audit_sinks SET checkpoint=$2,last_forwarded_at=now(),last_error=NULL WHERE instance_id=$1 AND checkpoint=$3`, f.InstanceID, last, c.checkpoint)
	return len(events), err
}

func (f *Forwarder) fail(ctx context.Context, err error) {
	_, _ = f.DB.Exec(ctx, `UPDATE audit_sinks SET last_error=$2 WHERE instance_id=$1`, f.InstanceID, errorClass(err))
}

func (f *Forwarder) transport(ctx context.Context, kind, endpoint, credential string, events []Event) error {
	if _, err := ValidateEndpoint(kind, endpoint); err != nil {
		return err
	}
	u, _ := url.Parse(endpoint)
	if kind == KindHTTP {
		return sendHTTP(ctx, endpoint, credential, events)
	}
	return f.sendSyslog(ctx, u, events)
}

func sendHTTP(ctx context.Context, endpoint, credential string, events []Event) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("User-Agent", "kuvryn-ant-audit-sink")
	req.Header.Set("X-Audit-Last-Event-ID", strconv.FormatInt(events[len(events)-1].ID, 10))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	client := &http.Client{
		Timeout:       15 * time.Second,
		Transport:     &http.Transport{DialContext: dialer, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// FormatSyslog renders one RFC 5424 message (facility 13 log audit, severity
// informational). The structured data carries identifiers only.
func FormatSyslog(host string, e Event) string {
	if host == "" {
		host = "-"
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `]`, `\]`)
	sd := fmt.Sprintf(`[audit@32473 id="%d" principal="%s" resource="%s"]`, e.ID, esc.Replace(e.PrincipalID), esc.Replace(e.ResourceID))
	msg := strings.NewReplacer("\n", " ", "\r", " ").Replace(string(e.Details))
	action := strings.Map(func(r rune) rune {
		if r <= ' ' || r > '~' {
			return '_'
		}
		return r
	}, e.Action)
	return fmt.Sprintf("<110>1 %s %s kuvryn-ant - %s %s %s", e.Time.UTC().Format(time.RFC3339Nano), host, action, sd, msg)
}

func (f *Forwarder) sendSyslog(ctx context.Context, u *url.URL, events []Event) error {
	network := "tcp"
	if u.Scheme == "udp" {
		network = "udp"
	}
	conn, err := dialer(ctx, network, u.Host)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if u.Scheme == "tls" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err = tc.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = tc
	}
	for _, e := range events {
		msg := FormatSyslog(f.Hostname, e)
		if network == "udp" {
			_, err = conn.Write([]byte(msg))
		} else {
			// RFC 6587 octet-counting framing.
			_, err = conn.Write([]byte(strconv.Itoa(len(msg)) + " " + msg))
		}
		if err != nil {
			return err
		}
	}
	return nil
}
