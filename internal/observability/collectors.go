package observability

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Collector writes Prometheus text-format samples for one subsystem. It runs on
// every scrape, must be fast and bounded, and must never emit credentials,
// paths, principals or resource IDs as label values.
type Collector func(w io.Writer)

var (
	collectorMu sync.Mutex
	collectors  = map[string]Collector{}
)

// RegisterCollector installs (or replaces) the named collector and returns a
// function that removes it again, which tests and shutdown paths use.
func RegisterCollector(name string, c Collector) (unregister func()) {
	collectorMu.Lock()
	collectors[name] = c
	collectorMu.Unlock()
	return func() {
		collectorMu.Lock()
		delete(collectors, name)
		collectorMu.Unlock()
	}
}

func writeCollectors(w io.Writer) {
	collectorMu.Lock()
	names := make([]string, 0, len(collectors))
	for name := range collectors {
		names = append(names, name)
	}
	sort.Strings(names)
	list := make([]Collector, 0, len(names))
	for _, name := range names {
		list = append(list, collectors[name])
	}
	collectorMu.Unlock()
	for _, c := range list {
		c(w)
	}
}

// Label escapes a label value for the text exposition format.
func Label(v string) string {
	return fmt.Sprintf("%q", strings.ToValidUTF8(v, "?"))
}
