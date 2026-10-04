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

// image builds a host's bootstrap image (#42). The nix build is generic:
// the sops-encrypted secrets ride inside it, and the host key that
// decrypts them at boot (sops-nix age.sshKeyPaths) must never enter the
// nix store. The key does not enter the image either (#605's decision,
// #659): it travels through a channel the platform provides, handed over
// by `fort provision` when the apply creates the host, and the initrd
// places it before activation (nixos/modules/seed.nix). So the artifact
// is the plain build output on every variant — this verb reads no
// secret and needs nix only (skipped with --from):
//
//   - linode: ~/.cache/forts/<host>-nixos.img.gz, with the #42 size caps
//     asserted (custom-image upload limits). The channel is
//     metadata.user_data on the instance (#606).
//   - qcow: ~/.cache/forts/<host>-nixos.qcow2, a copy of the build's
//     nixos.qcow2 (`fort provision` grows that copy to the manifest's
//     disk before the upload, which is why it is a copy and not the store
//     path). The channel is a NoCloud cidata volume pulumi-libvirt
//     attaches to the domain (#657).
//   - baremetal: ~/.cache/forts/<host>-nixos.img, the raw-efi build, which
//     a human writes to the machine's disk (docs/forts/baremetal.md). The
//     channel — the same cidata layout on a USB stick — is #658 and is
//     not built yet, so a baremetal birth waits for it.
//
// The graft this verb performed until #659 (guestfish writing the key
// into the image; the baremetal variant kept it last) is gone, and with
// it the single-use rule: the artifact holds nothing a copy could leak.
// An author-side verb: it touches no provider.
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

	// 2. the artifact: the build output, copied
	return plainImage(v, host, entry, src, out)
}

// plainImage copies the build output to the artifact path `fort
// provision --image` expects, after the #42 caps for a linode image. The
// copy holds no secret; it goes under the 0700 cache dir because the
// dir predates #659 and nothing else of the operator's lives there. A
// copy and not the store path itself because provision's libvirt flow
// grows the qcow2 in place (qemu-img resize) before the upload, and the
// store is read-only.
func plainImage(v variantSpec, host string, entry manifest.Host, src, out string) error {
	if v.variant == "linode" {
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
	}
	fmt.Printf("== copying %s → %s\n", filepath.Base(src), out)
	if err := copyPrivate(src, out); err != nil {
		return err
	}
	fmt.Printf("== done. Plain build output, no secret inside: %s\n", out)
	if b := entry.Baremetal(); b != nil {
		// No provider consumes this one: the writer is a human at the
		// machine. The host key's channel for this platform is #658.
		disk := b.Disk
		if disk == "" {
			disk = "<disk>"
		}
		fmt.Printf("== next: write it to %s from a live ISO (docs/forts/baremetal.md); the host key's channel for baremetal is #658, not built yet\n", disk)
		return nil
	}
	fmt.Printf("== next: fort provision %s --image %s   (the host key arrives through the %s channel at first boot)\n", host, out, v.channel())
	return nil
}

// The #42 caps: Linode's custom-image upload limits.
const (
	linodeRawCap = 6 << 30
	linodeGzCap  = 5 << 30
)

// variantSpec is what the manifest's provider table decides about the
// image: the flake attribute and the artifact's file kind.
type variantSpec struct {
	host, variant, attr string
}

func imageVariant(host string, h manifest.Host) variantSpec {
	switch {
	case h.Cloud():
		return variantSpec{host, "linode", "./nixos#" + host + "-linode"}
	case h.Baremetal() != nil:
		// the raw-efi variant: GPT, ESP first, root second (#287)
		return variantSpec{host, "baremetal", "./nixos#" + host + "-baremetal"}
	}
	return variantSpec{host, "qcow", "./nixos#" + host + "-qcow"}
}

// channel names the seed channel of a variant (nixos/modules/seed.nix's
// names), for the verb's next-step line. The baremetal channel is #658
// and has no name yet.
func (v variantSpec) channel() string {
	switch v.variant {
	case "linode":
		return "linode-metadata"
	case "qcow":
		return "nocloud"
	}
	return ""
}

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
