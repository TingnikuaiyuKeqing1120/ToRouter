package gateway

import "strings"

// A usage row reports losses from the whole attempt, in request/response order.
// A repeated carrier or adaptation is still one kind of loss, rather than a
// reason to duplicate the diagnostic marker.
func mergeDegradations(existing string, added []string) string {
	kinds := append(strings.Split(existing, ","), added...)
	seen := make(map[string]bool, len(kinds))
	merged := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if kind != "" && !seen[kind] {
			seen[kind] = true
			merged = append(merged, kind)
		}
	}
	return strings.Join(merged, ",")
}
