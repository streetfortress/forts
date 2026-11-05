package main

import "testing"

func TestSopsRefPath(t *testing.T) {
	dir := "flux/apps/lab/app"
	for rel, ok := range map[string]bool{
		"secrets.sops.yaml":               true,
		"../forts/pg/secrets.sops.yaml":   true,
		"../../other/forts/s.sops.yaml":   true,
		"../../../../outside/s.sops.yaml": false,
		"../../../../../etc/passwd":       false,
		"/etc/passwd":                     false,
	} {
		_, err := sopsRefPath(dir, rel)
		if (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", rel, err, ok)
		}
	}
}
