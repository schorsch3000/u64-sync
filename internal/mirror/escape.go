package mirror

import (
	"fmt"
	"strings"
)

var winReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

// escapeWindows makes a device file name storable on Windows: characters
// Windows forbids, '%' (the escape char, keeps the mapping injective), control
// characters and a trailing dot or space become %XX; reserved device names
// (CON, NUL, COM1, ...) get their first character escaped. C64 names often
// contain such characters. Compiled everywhere so it can be tested everywhere.
func escapeWindows(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		last := i == len(name)-1
		if c < 0x20 || strings.IndexByte(`<>:"/\|?*%`, c) >= 0 || (last && (c == '.' || c == ' ')) {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	out := b.String()
	base, _, _ := strings.Cut(out, ".")
	u := strings.ToUpper(strings.TrimRight(base, " "))
	if winReserved[u] || (len(u) == 4 && (strings.HasPrefix(u, "COM") || strings.HasPrefix(u, "LPT")) && u[3] >= '1' && u[3] <= '9') {
		return fmt.Sprintf("%%%02X", out[0]) + out[1:]
	}
	return out
}
