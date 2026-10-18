package replication

import (
	"net/http"
	"strconv"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
)

// applyIfMatch accepts the revision as an `If-Match: "N"` header like the rest
// of the API. The body `revision` stays supported; when both are present they
// must agree. A malformed header is rejected.
func applyIfMatch(w http.ResponseWriter, r *http.Request, revision *int64) bool {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return true
	}
	v := ""
	if len(values) == 1 {
		v = values[0]
	}
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
		return badIfMatch(w, r)
	}
	n, err := strconv.ParseInt(v[1:len(v)-1], 10, 64)
	if err != nil || n < 1 || v != strconv.Quote(strconv.FormatInt(n, 10)) {
		return badIfMatch(w, r)
	}
	if *revision != 0 && *revision != n {
		management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "If-Match and body revision differ")
		return false
	}
	*revision = n
	return true
}

func badIfMatch(w http.ResponseWriter, r *http.Request) bool {
	management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, `If-Match must be a quoted revision such as "3"`)
	return false
}
