package diskusage

import (
	"fmt"
	"io"
)

// Percent is the share of the filesystem in use, rounded up, computed the way
// df does (used / (used + available)).
func (u Usage) Percent() int {
	if u.Total == 0 {
		return 0
	}
	return int((100*(u.Total-u.Free) + u.Total - 1) / u.Total)
}

// Collector returns a metrics collector for the filesystem holding dir. It
// emits nothing when the filesystem cannot be inspected, so a scrape never
// fails because of it.
func Collector(dir string) func(io.Writer) {
	return func(w io.Writer) {
		u, err := Of(dir)
		if err != nil {
			return
		}
		fmt.Fprintln(w, "# HELP ant_data_dir_bytes_free Bytes available to the service on the filesystem holding the data directory.\n# TYPE ant_data_dir_bytes_free gauge")
		fmt.Fprintf(w, "ant_data_dir_bytes_free %d\n", u.Free)
		fmt.Fprintln(w, "# HELP ant_data_dir_bytes_total Capacity in bytes of the filesystem holding the data directory.\n# TYPE ant_data_dir_bytes_total gauge")
		fmt.Fprintf(w, "ant_data_dir_bytes_total %d\n", u.Total)
	}
}
