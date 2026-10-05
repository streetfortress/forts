package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/tools"
	"github.com/linode/linodego"
)

// imageFlow is the per-provider half of the provision flow (#42): how the
// bootstrap image — the plain build output of `fort image`, no secret
// inside (#659) — reaches the pulumi program, and how it goes away once
// the host exists. The program itself only ever sees `forts:imageId` —
// the value Stage returns, committed in Pulumi.<host>.yaml as provenance.
type imageFlow interface {
	// Stage makes the artifact available and returns the value recorded
	// as forts:imageId. It must leave the artifact retrievable if the
	// apply then fails (the retry runs without --image).
	Stage(ctx context.Context, entry manifest.Host, host, path string) (id string, err error)
	// Release deletes the artifact after a confirmed provision: provider-side
	// where one exists (Linode bills custom images), then the local copy.
	// Hygiene: the host does not need the image once it has booted.
	Release(ctx context.Context, id, path string) error
	// Leftover says what a human does with the artifact when the apply
	// did not confirm the host.
	Leftover(id string) string
}

var imageFlows = map[string]imageFlow{
	"linode":  &linodeImage{},
	"libvirt": &libvirtImage{},
}

// linodeImage uploads the .img.gz through the Images API, waits until
// it is usable, and deletes it provider-side once the instance exists.
type linodeImage struct{ client linodego.Client }

func (l *linodeImage) Stage(ctx context.Context, entry manifest.Host, host, path string) (string, error) {
	c, err := linodego.NewClientFromEnv(nil) // LINODE_TOKEN
	if err != nil {
		return "", err
	}
	l.client = *c
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fmt.Printf("uploading %s → %s (%s)…\n", path, host+"-bootstrap", entry.Linode().Region)
	img, err := l.client.UploadImage(ctx, linodego.ImageUploadOptions{
		Region: entry.Linode().Region,
		Label:  host + "-bootstrap",
		Image:  f,
	})
	if err != nil {
		return "", fmt.Errorf("image upload: %w", err)
	}
	// The instance create will fail while the image is still
	// processing; wait for it here rather than in the program.
	if _, err := l.client.WaitForImageStatus(ctx, img.ID, linodego.ImageStatusAvailable, 600); err != nil {
		return "", fmt.Errorf("image %s never became available (it stays on the account): %w", img.ID, err)
	}
	return img.ID, nil
}

func (l *linodeImage) Release(ctx context.Context, id, path string) error {
	if err := l.client.DeleteImage(ctx, id); err != nil {
		return fmt.Errorf("deleting provider-side image %s (delete it manually!): %w", id, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("deleting local image %s (delete it manually!): %w", path, err)
	}
	return nil
}

func (l *linodeImage) Leftover(id string) string {
	return "rerun without --image, or delete it: linode-cli images delete " + id
}

// libvirtImage: nothing to upload ahead of time — the pulumi program's
// Volume streams the qcow2 into the hypervisor's pool through the
// libvirt API, so the artifact's absolute path IS the id. Stage grows
// the qcow2's virtual size to the manifest's [shape] disk first (the
// provider refuses `size` next to `source`; the volume takes the
// image's virtual size — the playbook's order, kept). The volume becomes the
// root disk (not consumed), so Release removes only the local copy; the
// program ignores later changes to the source.
type libvirtImage struct{}

func (l *libvirtImage) Stage(ctx context.Context, entry manifest.Host, host, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	if filepath.Ext(abs) != ".qcow2" {
		return "", fmt.Errorf("%s: the libvirt backend takes the qcow2 fort image %s produced", path, host)
	}
	size := entry.Shape.WithLabDefaults().Disk
	fmt.Printf("growing %s to %s (qemu-img resize; the root fs autoResizes on first boot)…\n", abs, size)
	if err := qemuImg("resize", abs, size); err != nil {
		return "", fmt.Errorf("qemu-img resize: %w", err)
	}
	return abs, nil
}

// qemuImg runs qemu-img, one of the declared tools (internal/tools):
// the fort package carries it, a checkout run builds it from nixpkgs.
func qemuImg(args ...string) error {
	cmd, err := tools.Command("qemu-img", args...)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func (l *libvirtImage) Release(ctx context.Context, id, path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("deleting local image %s (delete it manually!): %w", path, err)
	}
	return nil
}

func (l *libvirtImage) Leftover(id string) string {
	return "rerun without --image (the volume may already exist: virsh vol-list images)"
}
