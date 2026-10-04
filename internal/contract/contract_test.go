package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = "contract: v1\nhttp: {service: app, port: 3000}\n"

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The rename's whole point: a chart released before #464 still loads,
// a renamed one loads, and when a chart carries both the new name wins.
func TestLoadNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"current", map[string]string{FileName: minimal}, FileName},
		{"legacy", map[string]string{LegacyFileName: minimal}, LegacyFileName},
		{"both", map[string]string{FileName: minimal, LegacyFileName: "contract: v1\n"}, FileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for n, b := range tc.files {
				write(t, dir, n, b)
			}
			c, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if c.File != tc.want {
				t.Errorf("File = %q, want %q", c.File, tc.want)
			}
			if tc.name == "both" && c.HTTP == nil {
				t.Error("read the legacy file's content, not the current name's")
			}
		})
	}
}

// Neither name: absent is not an error (#461). Load returns the empty
// v1 contract with Present false — the chart declares nothing — and
// Read still reports a not-exist error that names the file to write.
func TestLoadAbsent(t *testing.T) {
	dir := t.TempDir()
	if Present(dir) {
		t.Error("Present on an empty dir")
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load on a dir with no contract: %v", err)
	}
	if c.Present {
		t.Error("c.Present on a dir with no contract")
	}
	if c.Version != "v1" || c.HTTP != nil || c.Metrics != nil || len(c.Secret.Keys) != 0 || len(c.Requires) != 0 {
		t.Errorf("want the empty v1 contract, got %+v", c)
	}
	_, _, rerr := Read(dir)
	if !os.IsNotExist(rerr) {
		t.Fatalf("Read err = %v, want a not-exist error", rerr)
	}
	if !strings.Contains(rerr.Error(), FileName) {
		t.Errorf("error = %q, want it to name %q — the name to write", rerr, FileName)
	}
}

// A contract that IS there and does not parse stays an error: absent
// and malformed are different facts (#461).
func TestLoadMalformedIsStillAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, FileName, "contract: v1\nsecret: [not, a, mapping]\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("want an error for a contract that does not parse")
	} else if os.IsNotExist(err) {
		t.Errorf("a malformed contract reported as absent: %v", err)
	}
}

// Errors name the file they came from, so a legacy chart's complaint
// points at the file that is actually in its tarball.
func TestErrorNamesTheFileItRead(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, LegacyFileName, "contract: v2\n")
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want a version error")
	}
	if !strings.HasPrefix(err.Error(), LegacyFileName+":") {
		t.Errorf("error = %q, want it to lead with %q", err, LegacyFileName)
	}
}
