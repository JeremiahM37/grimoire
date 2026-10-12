package memory

import "testing"

// The staleness floor's wording test: positives name something true of the
// machine right now; negatives are measurements, limits and plain history.
func TestFloorShapeTable(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"grimoire v1.15.0 is the release", true},
		{"lectern 2.3.1 has the fix", true},
		{"the homelab agent is deployed on aiserver", true},
		{"the sidecar is installed", true},
		{"the gateway is now running", true},
		{"the proxy is currently listening", true},
		{"the lab work lives in a worktree at /mnt/bulk", true},
		{"the scratch build is at /tmp/build.log", true},
		{"the gate is 0.01", false},
		{"bounds are 128 MiB", false},
		{"the cache is redis", false},
		{"the cutover is planned for next week", false},
		{"the user prefers tabs", false},
		{"the team sits downstairs", false},
	}
	for _, c := range cases {
		if got := FloorShape(c.text); got != c.want {
			t.Errorf("FloorShape(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
