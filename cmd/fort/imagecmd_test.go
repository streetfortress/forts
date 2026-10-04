package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

func TestImageVariant(t *testing.T) {
	hosts, _, err := manifest.LoadAll("testdata")
	if err != nil {
		t.Fatal(err)
	}
	lab := imageVariant("lab-fort", hosts["lab-fort"])
	if lab.variant != "qcow" || lab.attr != "./nixos#lab-fort-qcow" || lab.ext() != ".qcow2" {
		t.Errorf("lab: %+v", lab)
	}
	cloud := imageVariant("cloud-fort", hosts["cloud-fort"])
	if cloud.variant != "linode" || cloud.attr != "./nixos#cloud-fort-linode" || cloud.ext() != ".img.gz" {
		t.Errorf("cloud: %+v", cloud)
	}
	metal := imageVariant("metal-fort", hosts["metal-fort"])
	if metal.variant != "baremetal" || metal.attr != "./nixos#metal-fort-baremetal" || metal.ext() != ".img" {
		t.Errorf("metal: %+v", metal)
	}
	// linode's key travels through the Metadata channel (#606), qcow's
	// through the NoCloud volume (#657); baremetal's channel is #658 and
	// has no name yet
	if cloud.channel() != "linode-metadata" || lab.channel() != "nocloud" || metal.channel() != "" {
		t.Errorf("channels: cloud=%q lab=%q metal=%q", cloud.channel(), lab.channel(), metal.channel())
	}
}

// TestPlainImage: the artifact is a 0600 copy of the build output, byte
// for byte, on every variant — no secret enters it (#659).
func TestPlainImage(t *testing.T) {
	hosts, _, err := manifest.LoadAll("testdata")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("nixos "), 100000)
	raw := filepath.Join(dir, "nixos.img")
	os.WriteFile(raw, payload, 0o444)
	gz := filepath.Join(dir, "nixos.img.gz")
	if n, err := gzipTo(raw, gz); err != nil || n <= 0 || n >= int64(len(payload)) {
		t.Fatalf("gzipTo: %d %v", n, err)
	}
	for _, tc := range []struct {
		host, src string
	}{
		{"lab-fort", raw},
		{"cloud-fort", gz},
		{"metal-fort", raw},
	} {
		v := imageVariant(tc.host, hosts[tc.host])
		out := filepath.Join(dir, tc.host+"-nixos"+v.ext())
		if err := plainImage(v, tc.host, hosts[tc.host], tc.src, out); err != nil {
			t.Errorf("%s: %v", tc.host, err)
			continue
		}
		want, _ := os.ReadFile(tc.src)
		got, _ := os.ReadFile(out)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: the artifact is not the build output", tc.host)
		}
		if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
			t.Errorf("%s: artifact must be 0600, got %o", tc.host, st.Mode().Perm())
		}
	}
}

func TestImageFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "nixos.qcow2"), []byte("x"), 0o644)
	if p, err := findImage(dir, ".qcow2"); err != nil || filepath.Base(p) != "nixos.qcow2" {
		t.Errorf("findImage: %q %v", p, err)
	}
	if _, err := findImage(dir, ".img.gz"); err == nil {
		t.Error("findImage should refuse when nothing matches")
	}

	payload := bytes.Repeat([]byte("nixos "), 100000)
	raw := filepath.Join(dir, "raw.img")
	os.WriteFile(raw, payload, 0o644)
	gz := filepath.Join(dir, "raw.img.gz")
	if n, err := gzipTo(raw, gz); err != nil || n <= 0 || n >= int64(len(payload)) {
		t.Errorf("gzipTo: %d %v", n, err)
	}
	if n, err := gunzipSize(gz); err != nil || n != int64(len(payload)) {
		t.Errorf("gunzipSize: %d %v", n, err)
	}
}

// gzipTo writes src gzipped to dst and returns the compressed size — a
// fixture for the linode cap check, which only ever reads a .gz.
func gzipTo(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	gz, err := gzip.NewWriterLevel(out, 6)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(gz, in); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	st, err := out.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
