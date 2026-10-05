package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// adopt brings a playbook-era lab VM under the libvirt backend without a
// re-provision: the live domain and its volume are imported into a new stack
// (read-only against the hypervisor), then the program is previewed
// against them and the only changes allowed through are the live ones
// (autostart, running) — anything replace-forcing means the program's
// shape and the VM's differ, and adopt refuses rather than re-provision. The
// one `pulumi up` that follows (you confirm) converges autostart, which
// the playbook never set. Provenance: forts:imageId records the
// adoption, not an image. After this, `fort destroy` is the VM's exit.
func adopt(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort adopt <host> [--yes]")
	}
	host := args[0]
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm the converging pulumi up without a prompt")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := needsTTYOrYes("adopt", *yes); err != nil {
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
	if entry.Provider != "libvirt" {
		return fmt.Errorf("host %q: adopt is for lab VMs the playbook raised — the manifest needs a [libvirt] table (got [%s])", host, entry.Provider)
	}
	pulumiDir := filepath.Join(root, "provisioning", "pulumi")
	if hasStack(pulumiDir, host) {
		return fmt.Errorf("host %q already has a stack — nothing to adopt", host)
	}
	if err := cloudEnv(root, entry.Provider); err != nil {
		return err
	}
	runVirsh, err := tools.Runner("virsh")
	if err != nil {
		return err
	}
	uri := os.Getenv("LIBVIRT_DEFAULT_URI")
	virsh := func(args ...string) (string, error) {
		out, err := runVirsh(append([]string{"-c", uri}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}

	// The playbook copied images into the pool's directory behind
	// libvirt's back; a rescan makes the volume addressable.
	if _, err := virsh("pool-refresh", libvirtPoolName); err != nil {
		return fmt.Errorf("pool-refresh %s: %w", libvirtPoolName, err)
	}
	uuid, err := virsh("domuuid", host)
	if err != nil || uuid == "" {
		return fmt.Errorf("no domain %q on the hypervisor (virsh domuuid): %v", host, err)
	}
	key, err := virsh("vol-key", "--pool", libvirtPoolName, host+".qcow2")
	if err != nil || key == "" {
		return fmt.Errorf("no volume %s.qcow2 in pool %s (virsh vol-key): %v", host, libvirtPoolName, err)
	}
	fmt.Printf("%s   domain %s, volume %s\n", host, uuid, key)

	if err := initStack(pulumiDir, host); err != nil {
		return err
	}
	if err := runPulumi(pulumiDir, "config", "set", "imageId", "adopted-"+time.Now().UTC().Format("2006-01-02"), "--stack", host); err != nil {
		return err
	}
	const protect = "--protect=true" // every fort
	fmt.Println("== importing (reads the hypervisor, writes only local state)")
	for _, r := range [][]string{
		{"libvirt:index/volume:Volume", host + "-disk", key},
		{"libvirt:index/domain:Domain", host, uuid},
	} {
		if err := runPulumi(pulumiDir, "import", r[0], r[1], r[2], "--stack", host, "--yes", "--generate-code=false", "--non-interactive", protect); err != nil {
			return fmt.Errorf("import %s: %w (stack left in place; `pulumi stack rm --force -s %s` to start over)", r[1], err, host)
		}
	}

	fmt.Println("== previewing the program against the imported state")
	out, _ := outputPulumi(pulumiDir, "preview", "--stack", host, "--diff", "--json", "--non-interactive")
	if err := adoptable(out); err != nil {
		return fmt.Errorf("%w\nthe stack is left in place for inspection (`fort preview %s`); `pulumi stack rm --force -s %s` to undo the adoption", err, host, host)
	}
	fmt.Println("   only live properties differ (autostart/running) — converging them; you confirm:")
	if err := runPulumi(pulumiDir, append([]string{"up", "--stack", host}, confirmArgs(*yes)...)...); err != nil {
		return err
	}
	stage(root)
	fmt.Printf("\n== done. Staged the new stack — commit it as its own PR (\"%s: adopted\"). From here: fort preview / provision / destroy %s.\n", host, host)
	return nil
}

// adoptable inspects a `pulumi preview --json` and accepts it when every
// step is unchanged, or an in-place update of the domain touching only
// its live properties.
func adoptable(previewJSON string) error {
	var d struct {
		Steps []struct {
			Op           string `json:"op"`
			URN          string `json:"urn"`
			DetailedDiff map[string]struct {
				Kind string `json:"kind"`
			} `json:"detailedDiff"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(previewJSON), &d); err != nil || len(d.Steps) == 0 {
		return fmt.Errorf("could not read the preview (%v)", err)
	}
	live := map[string]bool{"autostart": true, "running": true}
	var bad []string
	for _, s := range d.Steps {
		name := s.URN[strings.LastIndex(s.URN, "::")+2:]
		switch s.Op {
		case "same":
		case "update":
			var props []string
			for k := range s.DetailedDiff {
				if !live[strings.SplitN(k, ".", 2)[0]] {
					props = append(props, k)
				}
			}
			sort.Strings(props)
			if len(props) > 0 {
				bad = append(bad, fmt.Sprintf("%s: update of %s", name, strings.Join(props, ", ")))
			}
		default:
			var props []string
			for k := range s.DetailedDiff {
				props = append(props, k)
			}
			sort.Strings(props)
			bad = append(bad, fmt.Sprintf("%s: %s (%s)", name, s.Op, strings.Join(props, ", ")))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("the program would change more than the live properties — the VM's shape differs from what the backend renders (a re-provision, not an adoption):\n  %s", strings.Join(bad, "\n  "))
	}
	return nil
}
