package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// image builds a host's bootstrap image (#42, was
// provisioning/inject-host-key.sh). The nix build is generic: the
// sops-encrypted secrets ride inside it, and the host key that decrypts
// them at boot (sops-nix age.sshKeyPaths) must never enter the nix store.
// How the key reaches the host depends on the variant (#605's decision):
//
//   - linode: no graft (#606). The artifact is the plain build output,
//     ~/.cache/forts/<host>-nixos.img.gz, with the #42 size caps asserted
//     (custom-image upload limits). The key travels as metadata.user_data
//     on the instance (`fort provision`) and the initrd places it before
//     activation (nixos/modules/seed.nix). This verb reads no secret for
//     a linode host and needs neither sops nor guestfish.
//   - qcow, baremetal: the graft, until their NoCloud channel lands. The
//     key is read straight from the sops-encrypted repo store (fort mint's
//     custody model: no plaintext key material at rest anywhere) and
//     guestfish mounts the root explicitly (not virt-customize: libguestfs
//     OS inspection cannot identify a NixOS guest): /dev/sda1 for the qcow
//     variant's single msdos partition, /dev/sda2 for the baremetal
//     variant's GPT (the ESP is partition 1). The output is a
//     SECRET-BEARING, single-use artifact `fort provision <host> --image`
//     consumes and deletes on success — or, for a [baremetal] host, the
//     one a human writes to the machine's disk and then deletes
//     (docs/forts/baremetal.md).
//
// An author-side verb: it touches no provider. Tools: nix (the build;
// skipped with --from); for a grafted variant also sops, and guestfish
// from PATH or, failing that, from nixpkgs (ensureTool, as for virsh /
// xsltproc / kustomize; the declared-prerequisites question is #160's).
func image(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort image <host> [--from <built image>] [--out <dir>]")
	}
	host := args[0]
	fs := flag.NewFlagSet("image", flag.ContinueOnError)
	from := fs.String("from", "", "an already-built image (result/nixos.qcow2, result/*.img.gz, result/nixos.img) — skips the nix build")
	outDir := fs.String("out", "", "where the artifact goes (default ~/.cache/forts)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	entry, err := manifest.Load(root, host)
	if err != nil {
		return err
	}
	v := imageVariant(host, entry)
	if *outDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		*outDir = filepath.Join(home, ".cache", "forts")
	}
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}

	// 1. the generic image: built, or handed in
	src := *from
	if src == "" {
		if _, err := exec.LookPath("nix"); err != nil {
			return fmt.Errorf("nix not on PATH — the image build needs it (or pass --from <built image>)")
		}
		fmt.Printf("== nix build %s (KVM; any x86_64 Nix host)\n", v.attr)
		if src, err = nixBuildImage(root, v); err != nil {
			return err
		}
	} else if filepath.Ext(src) != v.ext() {
		return fmt.Errorf("%s: %s is the %s variant — want a %s image (%s)", src, host, v.variant, v.ext(), v.attr)
	}
	if _, err := os.Stat(src); err != nil {
		return err
	}
	out := filepath.Join(*outDir, host+"-nixos"+v.ext())

	// 2a. no graft: the build output is the artifact (#606)
	if !v.grafts() {
		return plainImage(v, host, src, out)
	}

	// 2b. the graft: the host's identity, from the sops file mint wrote
	if err := ensureTool("guestfish", "nixpkgs#libguestfs-with-appliance"); err != nil {
		return err
	}
	key, pub, err := hostKeyPair(sopsCLI{root: root}, root, host)
	if err != nil {
		return err
	}

	// 3. the work dir: private, shredded on exit whatever happens
	work, err := os.MkdirTemp(*outDir, ".inject-"+host+".")
	if err != nil {
		return err
	}
	defer shredDir(work)
	if err := os.WriteFile(filepath.Join(work, "key"), []byte(key), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, "key.pub"), []byte(pub), 0o600); err != nil {
		return err
	}
	img := filepath.Join(work, "nixos.img")
	fmt.Printf("== copying %s\n", filepath.Base(src))
	if err := copyPrivate(src, img); err != nil {
		return err
	}

	// 4. the graft
	fmt.Printf("== injecting /etc/ssh/ssh_host_ed25519_key via guestfish (%s)\n", v.rootDev)
	cmd := exec.Command("guestfish", "--rw", "-a", img)
	cmd.Stdin = strings.NewReader(guestfishScript(v.rootDev, work))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("guestfish: %w", err)
	}

	// 5. the artifact
	if err := os.Rename(img, out); err != nil {
		return err
	}
	fmt.Printf("== done. Single-use secret-bearing artifact: %s\n", out)
	if b := entry.Baremetal(); b != nil {
		// No provider consumes this one: the writer is a human at the
		// machine, and the artifact's deletion is a step of the runbook.
		disk := b.Disk
		if disk == "" {
			disk = "<disk>"
		}
		fmt.Printf("== next: write it to %s from a live ISO (docs/forts/baremetal.md), then delete it — no provider consumes it\n", disk)
		return nil
	}
	fmt.Printf("== next: fort provision %s --image %s   (deletes it on success)\n", host, out)
	return nil
}

// plainImage is the linode path: assert the #42 caps on the build output
// and copy it to the artifact path `fort provision --image` expects. The
// copy holds no secret; it goes under the same 0700 cache dir as the
// grafted variants' artifacts, and provision still deletes it on success
// (the single-use lifecycle is #605's step 3).
func plainImage(v variantSpec, host, src, out string) error {
	n, err := gunzipSize(src)
	if err != nil {
		return err
	}
	fmt.Printf("   uncompressed: %d MiB (cap %d)\n", n>>20, linodeRawCap>>20)
	if n > linodeRawCap {
		return fmt.Errorf("image exceeds Linode's 6 GiB uncompressed cap")
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	fmt.Printf("   gzipped: %d MiB (cap %d)\n", st.Size()>>20, linodeGzCap>>20)
	if st.Size() > linodeGzCap {
		return fmt.Errorf("image exceeds Linode's 5 GiB gzip cap")
	}
	fmt.Printf("== copying %s → %s\n", filepath.Base(src), out)
	if err := copyPrivate(src, out); err != nil {
		return err
	}
	fmt.Printf("== done. Plain build output, no secret inside: %s\n", out)
	fmt.Printf("== next: fort provision %s --image %s   (the host key arrives through the Metadata channel at first boot)\n", host, out)
	return nil
}

// The #42 caps: Linode's custom-image upload limits.
const (
	linodeRawCap = 6 << 30
	linodeGzCap  = 5 << 30
)

// variantSpec is what the manifest's provider table decides about the
// image: the flake attribute, the root device guestfish mounts, and the
// artifact's file kind.
type variantSpec struct {
	host, variant, attr, rootDev string
}

func imageVariant(host string, h manifest.Host) variantSpec {
	switch {
	case h.Cloud():
		return variantSpec{host, "linode", "./nixos#" + host + "-linode", "/dev/sda"}
	case h.Baremetal() != nil:
		// the raw-efi variant: GPT, ESP first, root second (#287)
		return variantSpec{host, "baremetal", "./nixos#" + host + "-baremetal", "/dev/sda2"}
	}
	return variantSpec{host, "qcow", "./nixos#" + host + "-qcow", "/dev/sda1"}
}

// grafts says whether the variant's artifact carries the host key. The
// linode variant reads its seed from the Metadata channel at first boot
// (#606); qcow and baremetal keep the graft until their NoCloud channel
// lands (#605's order).
func (v variantSpec) grafts() bool { return v.variant != "linode" }

func (v variantSpec) ext() string {
	switch v.variant {
	case "linode":
		return ".img.gz"
	case "baremetal":
		return ".img"
	}
	return ".qcow2"
}

// nixBuildImage builds the attribute (progress on stderr) and returns the
// image file inside the output path: nixos.qcow2, or the one *.img.gz.
func nixBuildImage(root string, v variantSpec) (string, error) {
	cmd := exec.Command("nix", "build", "--no-link", "--print-out-paths", v.attr)
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix build %s: %w", v.attr, err)
	}
	return findImage(strings.TrimSpace(string(out)), v.ext())
}

// findImage: the one image file of the given kind in a build output.
func findImage(dir, ext string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*"+ext))
	if len(matches) != 1 {
		return "", fmt.Errorf("%s: want exactly one *%s, found %d", dir, ext, len(matches))
	}
	return matches[0], nil
}

// guestfishScript: mount the root explicitly and upload the pair.
func guestfishScript(rootDev, work string) string {
	return strings.Join([]string{
		"run",
		"mount " + rootDev + " /",
		"mkdir-p /etc/ssh",
		"upload " + filepath.Join(work, "key") + " /etc/ssh/ssh_host_ed25519_key",
		"upload " + filepath.Join(work, "key.pub") + " /etc/ssh/ssh_host_ed25519_key.pub",
		"chmod 0600 /etc/ssh/ssh_host_ed25519_key",
		"chmod 0644 /etc/ssh/ssh_host_ed25519_key.pub",
		"",
	}, "\n")
}

func gunzipTo(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", src, err)
	}
	defer gz.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, gz)
}

// gunzipSize is the uncompressed length of a .gz, read through once —
// the #42 raw cap without a copy on disk.
func gunzipSize(src string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", src, err)
	}
	defer gz.Close()
	return io.Copy(io.Discard, gz)
}

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

// copyPrivate copies a (possibly read-only nix store) file to a 0600 copy.
func copyPrivate(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// shredDir overwrites every file in dir with zeros once and removes it —
// the work dir held the plaintext key and a key-bearing image. Best
// effort, as shred is on a journaling or CoW filesystem.
func shredDir(dir string) {
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			io.CopyN(f, zeroReader{}, info.Size())
			f.Close()
		}
		return nil
	})
	os.RemoveAll(dir)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
