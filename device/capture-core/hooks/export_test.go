package hooks

import "testing"

// SetAdapter registers a as tool's adapter until the test ends.
func SetAdapter(t testing.TB, tool string, a Adapter) {
	old, had := adapters[tool]
	adapters[tool] = a
	t.Cleanup(func() {
		if had {
			adapters[tool] = old
		} else {
			delete(adapters, tool)
		}
	})
}
