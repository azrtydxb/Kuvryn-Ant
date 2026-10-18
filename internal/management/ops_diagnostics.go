package management

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
)

// getDiagnostics streams the redacted support bundle (tar.gz). The bundle is
// assembled in memory first so a failure never yields a truncated archive, and
// the download is recorded in the audit log.
func (h opsHandlers) getDiagnostics(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	if h.d.Diagnostics == nil {
		Error(w, r, http.StatusServiceUnavailable, api.ErrorCodeUnavailable, "diagnostics unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var buf bytes.Buffer
	if err := h.d.Diagnostics(ctx, &buf); err != nil {
		Error(w, r, http.StatusInternalServerError, api.ErrorCodeInternalError, "diagnostics bundle failed")
		return
	}
	actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer acancel()
	_, _ = h.db.Exec(actx, `INSERT INTO audit_events(instance_id,principal_id,action) VALUES($1,$2,'diagnostics.download')`, p.InstanceID, p.PrincipalID)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="ant-diagnostics-`+time.Now().UTC().Format("20060102T150405Z")+`.tar.gz"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
