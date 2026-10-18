package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/google/uuid"
)

func remoteFixture(t *testing.T) (*sourceHTTPFixture, *secrets.Keyring) {
	t.Helper()
	f := newSourceHTTPFixture(t)
	root := bytes.Repeat([]byte{9}, 32)
	keys, err := secrets.New("v1", map[string][]byte{"v1": root})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterRemoteSources(mux, f.db, keys, []string{"approved-ca"}, root)
	mux.Handle("/", f.handler)
	f.handler = WithRequestID(mux)
	return f, keys
}
func remoteBody(t *testing.T, c remoteconfig.Config) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestRemoteConfigStrictJSON(t *testing.T) {
	valid := remoteBody(t, remoteconfig.DefaultConfig("https://upstream.example/repository"))
	for _, body := range []string{`{}`, strings.Replace(valid, `"origin":`, `"Origin":`, 1), strings.Replace(valid, `"origin":`, `"origin":"https://duplicate.example","origin":`, 1), strings.Replace(valid, `"max_entries":1024`, `"max_entries":1024,"MAX_ENTRIES":5`, 1), strings.Replace(valid, `"cache":{`, `"cache":{"metadata_ttl_seconds":300,`, 1), strings.Replace(valid, `"origin":"https://upstream.example/repository"`, `"origin":"http://upstream.example"`, 1), strings.Replace(valid, `"cache":{`, `"cache":{"secret":"not-allowed",`, 1)} {
		r := httptest.NewRequest("PUT", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if _, ok := readRemoteConfig(httptest.NewRecorder(), r); ok {
			t.Fatal("invalid JSON accepted", body)
		}
	}
	for _, input := range []RemoteCredentialInput{{}, {Authorization: "Bearer secret\r\nInjected: yes"}, {APIKey: strings.Repeat("x", 8193)}, {APIKey: "\x00"}, {APIKey: " "}, {APIKey: "é"}} {
		if validRemoteCredential(input) {
			t.Fatal("unsafe credential accepted")
		}
	}
}
func TestRemoteConfigCredentialHTTPContractAndAtomicity(t *testing.T) {
	f, keys := remoteFixture(t)
	source := f.source("remote-config", "remote", "raw")
	path := "/api/v1/sources/" + source.ID + "/remote-config"
	credentialPath := "/api/v1/sources/" + source.ID + "/remote-credential"
	expectSourceStatus(t, f.request("GET", path, "", "", ""), 404)
	c := remoteconfig.DefaultConfig("https://UPSTREAM.example:443/repository")
	c.CARef = "approved-ca"
	body := remoteBody(t, c)
	first := f.request("PUT", path, body, "configure-remote", `"1"`)
	expectSourceStatus(t, first, 200)
	if first.Header().Get("ETag") != `"2"` {
		t.Fatal("missing config revision")
	}
	expectSourceStatus(t, f.request("PUT", path, body, "configure-remote", `"1"`), 200)
	const secret = "Bearer fixture-private-upstream-token"
	credentialBody := fmt.Sprintf(`{"authorization":%q}`, secret)
	response := f.request("PUT", credentialPath, credentialBody, "credential-create", `"2"`)
	expectSourceStatus(t, response, 200)
	replay := f.request("PUT", credentialPath, credentialBody, "credential-create", `"2"`)
	expectSourceStatus(t, replay, 200)
	if replay.Body.String() != response.Body.String() || strings.Contains(response.Body.String(), secret) {
		t.Fatal("credential replay disclosure/change")
	}
	expectSourceStatus(t, f.request("PUT", credentialPath, `{"authorization":"different"}`, "credential-create", `"2"`), 409)
	var record remoteconfig.Record
	if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil || record.Revision != 3 || record.Config.CredentialID == "" {
		t.Fatal(record, err)
	}
	store := secrets.Store{DB: f.db, Keys: keys}
	stored, plain, err := store.Open(context.Background(), f.instance, record.Config.CredentialID, remoteconfig.CredentialPurpose(source.ID))
	if err != nil || !bytes.Contains(plain, []byte(secret)) || stored.Revision != 1 {
		t.Fatal("encrypted credential missing", err)
	}
	clear(plain)
	var leaked bool
	if err = f.db.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM mutation_results WHERE response::text LIKE '%fixture-private-upstream-token%') OR EXISTS(SELECT 1 FROM audit_events WHERE details::text LIKE '%fixture-private-upstream-token%') OR EXISTS(SELECT 1 FROM encrypted_secrets WHERE envelope::text LIKE '%fixture-private-upstream-token%')`).Scan(&leaked); err != nil || leaked {
		t.Fatal("plaintext persisted outside encryption", err)
	}
	canonical, _ := json.Marshal(struct {
		Credential RemoteCredentialInput
		Revision   int64
	}{RemoteCredentialInput{Authorization: secret}, 2})
	unkeyed := sha256.Sum256(canonical)
	var actual []byte
	if err = f.db.QueryRow(context.Background(), `SELECT fingerprint FROM mutation_results WHERE operation='source.remote.credential'`).Scan(&actual); err != nil || bytes.Equal(actual, unkeyed[:]) {
		t.Fatal("credential fingerprint not keyed", err)
	}
	second := f.request("PUT", credentialPath, `{"api_key":"second-private-value"}`, "credential-update", `"3"`)
	expectSourceStatus(t, second, 200)
	var updated remoteconfig.Record
	json.Unmarshal(second.Body.Bytes(), &updated)
	if updated.Config.CredentialID != record.Config.CredentialID || updated.Revision != 4 {
		t.Fatal("update replaced identity or wrong revision")
	}
	stored, plain, err = store.Open(context.Background(), f.instance, record.Config.CredentialID, remoteconfig.CredentialPurpose(source.ID))
	if err != nil || stored.Revision != 2 || !bytes.Contains(plain, []byte("second-private-value")) {
		t.Fatal("credential not rotated", err)
	}
	clear(plain)
	if _, err = f.db.Exec(context.Background(), `ALTER TABLE outbox_events ADD CONSTRAINT reject_remote_config CHECK(topic<>'source.remote.configure') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("PUT", credentialPath, `{"api_key":"rollback-private-value"}`, "credential-rollback", `"4"`), 503)
	stored, plain, err = store.Open(context.Background(), f.instance, record.Config.CredentialID, remoteconfig.CredentialPurpose(source.ID))
	if err != nil || stored.Revision != 2 || bytes.Contains(plain, []byte("rollback-private-value")) {
		t.Fatal("secret update escaped rollback", err)
	}
	clear(plain)
	get := f.request("GET", path, "", "", "")
	expectSourceStatus(t, get, 200)
	if get.Header().Get("ETag") != `"4"` {
		t.Fatal("revision escaped rollback")
	}
	// Generated client performs a real HTTP read of the public configuration DTO.
	server := httptest.NewServer(f.handler)
	defer server.Close()
	client, err := api.NewClientWithResponses(server.URL, api.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+f.credential)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := client.GetRemoteSourceConfigWithResponse(context.Background(), uuid.MustParse(source.ID))
	if err != nil || reply.JSON200 == nil || reply.JSON200.Revision != 4 {
		t.Fatal("generated client", err)
	}
}
func TestRemoteConfigAuthorizationAndCrossSourceReferences(t *testing.T) {
	f, _ := remoteFixture(t)
	source := f.source("remote-auth", "remote", "raw")
	other := f.source("remote-other", "remote", "raw")
	local := f.source("local-hidden", "local", "raw")
	path := "/api/v1/sources/" + source.ID + "/remote-config"
	credentialPath := "/api/v1/sources/" + source.ID + "/remote-credential"
	body := remoteBody(t, remoteconfig.DefaultConfig("https://upstream.example"))
	expectSourceStatus(t, f.request("PUT", credentialPath, `{"api_key":"no-config"}`, "missing-config", `"1"`), 404)
	expectSourceStatus(t, f.request("PUT", path, body, "set-auth", `"1"`), 200)
	response := f.request("PUT", credentialPath, `{"api_key":"bound-to-source"}`, "credential-auth", `"2"`)
	expectSourceStatus(t, response, 200)
	var record remoteconfig.Record
	json.Unmarshal(response.Body.Bytes(), &record)
	copyConfig := record.Config
	expectSourceStatus(t, f.request("PUT", "/api/v1/sources/"+other.ID+"/remote-config", remoteBody(t, copyConfig), "cross-source", `"1"`), 400)
	expectSourceStatus(t, f.request("PUT", "/api/v1/sources/"+local.ID+"/remote-config", body, "wrong-kind", `"1"`), 404)
	token, err := (identity.Tokens{DB: f.db}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.requestAs("GET", path, "", "", "", token.Secret), 404)
	expectSourceStatus(t, f.requestAs("PUT", credentialPath, `{"api_key":"bound-to-source"}`, "credential-auth", `"2"`, token.Secret), 404)
	expectSourceStatus(t, f.request("PUT", path, body, "missing-cas", ""), 428)
	expectSourceStatus(t, f.request("PUT", path, body, "stale-cas", `"1"`), 412)
}
