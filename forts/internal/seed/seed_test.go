package seed

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\nQyNTUxOQAAACB0ZXN0\n-----END OPENSSH PRIVATE KEY-----\n"
	testPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHRlc3Q sfi-fort-test\n"
)

func TestComposeParse(t *testing.T) {
	text, err := Compose(testKey, testPub)
	if err != nil {
		t.Fatal(err)
	}
	if text != testKey+testPub {
		t.Errorf("compose:\n%q", text)
	}
	// the inputs as sops hands them back: no trailing newline, or several
	text2, _ := Compose(strings.TrimRight(testKey, "\n"), testPub+"\n\n")
	if text2 != text {
		t.Errorf("compose is not newline-normalising:\n%q", text2)
	}
	priv, pub, err := Parse(text)
	if err != nil || priv != testKey || pub != testPub {
		t.Errorf("parse: %q %q %v", priv, pub, err)
	}
	if _, err := Compose("not a key", testPub); err == nil {
		t.Error("compose should refuse a non-PEM private key")
	}
	if _, err := Compose(testKey, "ssh-rsa AAAA"); err == nil {
		t.Error("compose should refuse a non-ed25519 public line")
	}
	if _, _, err := Parse(testKey); err == nil {
		t.Error("parse should refuse a seed with no public line")
	}
}

func TestUserData(t *testing.T) {
	ud, err := UserData(testKey, testPub)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(ud)
	if err != nil || string(raw) != testKey+testPub {
		t.Errorf("user data does not round-trip: %v", err)
	}
	if _, err := UserData(strings.Replace(testKey, "AAAA", strings.Repeat("A", 70000), 1), testPub); err == nil {
		t.Error("user data over the cap should refuse")
	}
}

// The contract with the host: the shell reader (nixos/modules/seed/
// reader.sh) splits the same text into the same two files, with the same
// modes. Skipped where there is no sh.
func TestReaderAgrees(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	reader, err := os.ReadFile("../../../nixos/modules/seed/reader.sh")
	if err != nil {
		t.Skip("reader.sh not at the expected path")
	}
	text, _ := Compose(testKey, testPub)
	root := t.TempDir()
	// the channel: the seed on stdout; `fail` never reached here
	script := string(reader) + "\nsfi_seed_channel() { cat " + filepath.Join(root, "seed") + "; }\nsfi_seed " + root + "\n"
	os.WriteFile(filepath.Join(root, "seed"), []byte(text), 0o600)
	cmd := exec.Command(sh, "-e", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reader: %v\n%s", err, out)
	}
	priv, _ := os.ReadFile(filepath.Join(root, "etc/ssh/ssh_host_ed25519_key"))
	pub, _ := os.ReadFile(filepath.Join(root, "etc/ssh/ssh_host_ed25519_key.pub"))
	if string(priv) != testKey || string(pub) != testPub {
		t.Errorf("reader wrote:\n%q\n%q", priv, pub)
	}
	if st, _ := os.Stat(filepath.Join(root, "etc/ssh/ssh_host_ed25519_key")); st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(root, "etc/ssh/ssh_host_ed25519_key.pub")); st.Mode().Perm() != 0o644 {
		t.Errorf("pub mode %o", st.Mode().Perm())
	}
	// a second run finds the key and does nothing
	out, err = exec.Command(sh, "-e", "-c", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "nothing to do") {
		t.Errorf("second run: %v\n%s", err, out)
	}
	// no seed: the reader exits 1 with the message
	empty := t.TempDir()
	script = string(reader) + "\nsfi_seed_channel() { return 1; }\nsfi_seed " + empty + "\n"
	out, err = exec.Command(sh, "-e", "-c", script).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "returned no seed") {
		t.Errorf("no seed: want exit 1 with the message; got %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(empty, "etc/ssh/ssh_host_ed25519_key")); err == nil {
		t.Error("no seed must write no key")
	}
}
