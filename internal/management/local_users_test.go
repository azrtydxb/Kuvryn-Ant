package management

import (
	"strings"
	"testing"
)

func TestLocalUserAdministration(t *testing.T) {
	f := newIDPFixture(t)
	const pw = "a-long-enough-passphrase"
	body := `{"username":"alice","password":"` + pw + `"}`

	// Admin only; unauthenticated callers are refused.
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/local-accounts", "", "", "", f.limited), 403)
	expectSourceStatus(t, f.requestAs("POST", "/api/v1/local-accounts", body, "", "", f.limited), 403)
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/local-accounts", "", "", "", ""), 401)

	// Validation: weak password, bad username, unknown fields, missing key.
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"alice","password":"short"}`, "", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"Al Ice","password":"`+pw+`"}`, "", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"alice","password":"`+pw+`","admin":true}`, "", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", body, "-", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"break-glass","password":"`+pw+`"}`, "", ""), 409)

	// Create is idempotent, never echoes or persists the password.
	a := f.request("POST", "/api/v1/local-accounts", body, "lu-create", "")
	b := f.request("POST", "/api/v1/local-accounts", body, "lu-create", "")
	expectSourceStatus(t, a, 201)
	expectSourceStatus(t, b, 201)
	if strings.Contains(a.Body.String()+b.Body.String(), pw) {
		t.Fatal("password echoed")
	}
	u := decodeJSON[LocalUser](t, a)
	if u.Username != "alice" || u.PrincipalID == "" || u.BreakGlass {
		t.Fatalf("%+v", u)
	}
	if decodeJSON[LocalUser](t, b).PrincipalID != u.PrincipalID {
		t.Fatal("replay created a second principal")
	}
	var n int
	if err := f.db.QueryRow(t.Context(), `SELECT count(*) FROM mutation_results WHERE response::text LIKE '%`+pw+`%'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("password persisted in replay store", err, n)
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", body, "", ""), 409)

	list := decodeJSON[LocalUserList](t, f.get("/api/v1/local-accounts"))
	if len(list.Items) != 1 || list.Items[0].Username != "alice" {
		t.Fatalf("%+v", list)
	}

	// Password change and removal.
	expectSourceStatus(t, f.request("PUT", "/api/v1/local-accounts/alice/password", `{"password":"short"}`, "", ""), 400)
	expectSourceStatus(t, f.request("PUT", "/api/v1/local-accounts/nobody/password", `{"password":"`+pw+`-2"}`, "", ""), 404)
	expectSourceStatus(t, f.request("PUT", "/api/v1/local-accounts/alice/password", `{"password":"`+pw+`-2"}`, "", ""), 204)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/local-accounts/alice", "", "", ""), 204)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/local-accounts/alice", "", "", ""), 404)
	if got := decodeJSON[LocalUserList](t, f.get("/api/v1/local-accounts")); len(got.Items) != 0 {
		t.Fatalf("%+v", got)
	}
}
