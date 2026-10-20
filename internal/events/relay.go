package events

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// MaxAttempts is the number of delivery attempts before a delivery is dead.
const MaxAttempts = maxAttempts

// Backoff is the delay before attempt n+1 after attempt n failed.
func Backoff(attempt int) time.Duration { return backoff(attempt) }

// Sender posts one request through the same hardened transport as webhook
// delivery: https only (http with AllowHTTP), no proxies, redirects not
// followed, special-use addresses refused on every connection. Retries and the
// delivery log belong to the caller; the incoming hook relay keeps them on its
// delivery rows, using Backoff and MaxAttempts.
type Sender struct {
	client  *http.Client
	options Options
}

// NewSender builds a Sender. A nil client uses the default transport settings.
func NewSender(client *http.Client, options Options) (*Sender, error) {
	safe, err := safeClient(client, options)
	if err != nil {
		return nil, err
	}
	return &Sender{client: safe, options: options}, nil
}

// Close releases idle connections.
func (s *Sender) Close() { s.client.CloseIdleConnections() }

// Signing re-signs a relayed body with the scheme used for webhook events.
// Keys are the current secret and, inside a rotation grace window, the previous.
type Signing struct {
	EventID string
	Keys    [][]byte
}

// Post sends body to endpoint with the given headers and returns "" on a 2xx
// answer, else a closed failure class: invalid_destination, transport_failure
// or http_failure. Configuration-time validation is ValidateEndpointFor; here the
// scheme is checked again and every connection is checked against special-use
// addresses.
func (s *Sender) Post(ctx context.Context, endpoint string, body []byte, header http.Header, signing *Signing) string {
	endpoint = canonicalEndpoint(endpoint)
	if !endpointAllowed(endpoint, s.options) {
		return "invalid_destination"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "invalid_destination"
	}
	for name, values := range header {
		for _, v := range values {
			request.Header.Add(name, v)
		}
	}
	if signing != nil {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		request.Header.Set("X-Ant-Event-ID", signing.EventID)
		request.Header.Set("X-Ant-Timestamp", timestamp)
		request.Header.Set("X-Ant-Signature", SignatureHeader(timestamp, signing.EventID, body, signing.Keys...))
	}
	response, err := s.client.Do(request)
	if err != nil {
		return "transport_failure"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return ""
	}
	return "http_failure"
}
