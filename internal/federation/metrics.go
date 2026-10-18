package federation

import (
	"fmt"
	"io"
)

// WriteMetrics emits federation gauges from the in-memory trust state (no
// database access on scrape). Labels carry only fixed values.
func (s *Service) WriteMetrics(w io.Writer) {
	t := s.trust.Load()
	active := 0
	if t.state.State == "active" {
		active = 1
	}
	fmt.Fprintln(w, "# HELP ant_federation_active Whether this instance is an active federation member.\n# TYPE ant_federation_active gauge")
	fmt.Fprintf(w, "ant_federation_active %d\n", active)
	if t.doc == nil {
		return
	}
	fmt.Fprintln(w, "# HELP ant_federation_document_generation Applied federation document generation.\n# TYPE ant_federation_document_generation gauge")
	fmt.Fprintf(w, "ant_federation_document_generation{kind=\"membership\"} %d\n", t.doc.Generation)
	fmt.Fprintln(w, "# HELP ant_federation_authority_epoch Applied authority epoch.\n# TYPE ant_federation_authority_epoch gauge")
	fmt.Fprintf(w, "ant_federation_authority_epoch %d\n", t.doc.AuthorityEpoch)
	counts := map[string]int{"active": 0, "suspended": 0, "removed": 0}
	for _, m := range t.doc.Members {
		counts[m.Status]++
	}
	fmt.Fprintln(w, "# HELP ant_federation_members Federation members by status in the applied membership.\n# TYPE ant_federation_members gauge")
	for _, st := range []string{"active", "suspended", "removed"} {
		fmt.Fprintf(w, "ant_federation_members{status=%q} %d\n", st, counts[st])
	}
}
