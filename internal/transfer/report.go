package transfer

// Actions reported for each inventory item.
const (
	ActionCreated   = "created"
	ActionUpdated   = "updated"
	ActionUnchanged = "unchanged"
	ActionSkipped   = "skipped"
	ActionConflict  = "conflict"
)

const maxReportItems = 5000

// Item is one mapped object. Unchanged items are counted but not listed.
type Item struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// Unmapped is configuration that cannot be carried over automatically and needs
// operator attention.
type Unmapped struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Report is the inventory and mapping report of an import or migration.
// A dry run and the matching apply produce identical Items and Summary.
type Report struct {
	DryRun            bool                      `json:"dry_run"`
	Summary           map[string]map[string]int `json:"summary"`
	Items             []Item                    `json:"items"`
	ItemsTruncated    bool                      `json:"items_truncated,omitempty"`
	Unmapped          []Unmapped                `json:"unmapped"`
	IntegrityFailures []Failure                 `json:"integrity_failures"`
}

func newReport(dry bool) *Report {
	return &Report{DryRun: dry, Summary: map[string]map[string]int{}, Items: []Item{}, Unmapped: []Unmapped{}, IntegrityFailures: []Failure{}}
}

func (r *Report) add(kind, name, action, reason string) {
	m := r.Summary[kind]
	if m == nil {
		m = map[string]int{}
		r.Summary[kind] = m
	}
	m[action]++
	if action == ActionUnchanged {
		return
	}
	if len(r.Items) >= maxReportItems {
		r.ItemsTruncated = true
		return
	}
	r.Items = append(r.Items, Item{kind, name, action, reason})
}

func (r *Report) unmapped(kind, name, reason string) {
	if len(r.Unmapped) < maxReportItems {
		r.Unmapped = append(r.Unmapped, Unmapped{kind, name, reason})
	}
}

// Count returns the number of items of a kind with an action.
func (r *Report) Count(kind, action string) int { return r.Summary[kind][action] }

// Clean reports whether nothing conflicted, was skipped or failed integrity.
func (r *Report) Clean() bool {
	if len(r.IntegrityFailures) > 0 {
		return false
	}
	for _, m := range r.Summary {
		if m[ActionConflict] > 0 || m[ActionSkipped] > 0 {
			return false
		}
	}
	return true
}
