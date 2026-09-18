package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

func TestImageVariant(t *testing.T) {
	hosts, _, err := manifest.LoadAll("testdata")
	if err != nil {
		t.Fatal(err)
	}
	lab := imageVariant("lab-fort", hosts["lab-fort"])
	if lab.variant != "qcow" || lab.attr != "./nixos#lab-fort-qcow" || lab.rootDev != "/dev/sda1" || lab.ext() != ".qcow2" {
		t.Errorf("lab: %+v", lab)
	}
	cloud := imageVariant("cloud-fort", hosts["cloud-fort"])
	if cloud.variant != "linode" || cloud.attr != "./nixos#cloud-fort-linode" || cloud.rootDev != "/dev/sda" || cloud.ext() != ".img.gz" {
		t.Errorf("cloud: %+v", cloud)
	}
	metal := imageVariant("metal-fort", hosts["metal-fort"])
	if metal.variant != "baremetal" || metal.attr != "./nixos#metal-fort-baremetal" || metal.rootDev != "/dev/sda2" || metal.ext() != ".img" {
		t.Errorf("metal: %+v", metal)
	}
	s := guestfishScript("/dev/sda1", "/w")
	for _, want := range []string{"run\nmount /dev/sda1 /\n", "upload /w/key /etc/ssh/ssh_host_ed25519_key\n", "upload /w/key.pub /etc/ssh/ssh_host_ed25519_key.pub\n", "chmod 0600 /etc/ssh/ssh_host_ed25519_key\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
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
	back := filepath.Join(dir, "back.img")
	if n, err := gunzipTo(gz, back); err != nil || n != int64(len(payload)) {
		t.Errorf("gunzipTo: %d %v", n, err)
	}
	if got, _ := os.ReadFile(back); !bytes.Equal(got, payload) {
		t.Error("round trip differs")
	}
	if st, _ := os.Stat(back); st.Mode().Perm() != 0o600 {
		t.Errorf("work files must be 0600, got %o", st.Mode().Perm())
	}

	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o700)
	os.WriteFile(filepath.Join(work, "key"), []byte("secret"), 0o600)
	shredDir(work)
	if _, err := os.Stat(work); err == nil {
		t.Error("shredDir should remove the dir")
	}
}
