package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// backendEnv is the half of the environment every pulumi run needs
// before any provider is known: the pinned plugins, the program, the
// state backend (forts#248) and the passphrase. The gates of provision
// and destroy ask the backend whether a stack exists before anything
// touches the provider, so this runs first; cloudEnv adds the provider.
func backendEnv(root string, b stateBackend) error {
	// The nixpkgs pulumi-bin bundle puts its own provider binaries on
	// PATH, and pulumi PREFERS ambient plugins — we ran a v6 provider
	// against v4-generated SDK code, which mis-shapes outputs (specs as
	// list-of-one) and invoke results (Disk=0). Always use the plugin
	// version pinned by go.mod.
	os.Setenv("PULUMI_IGNORE_AMBIENT_PLUGINS", "true")

	// The program pulumi runs is fort-pulumi, built beside this binary
	// (provisioning/pulumi/Pulumi.yaml names it, `binary: fort-pulumi`),
	// so the pair always match: put this binary's directory first on PATH
	// for the pulumi child.
	if exe, err := os.Executable(); err == nil {
		os.Setenv("PATH", filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	// The state backend is the estate's ([state] in estate.toml): the
	// committed file:// dir of THIS checkout by default, a bucket or a
	// directory outside the tree when the estate says so (state.go).
	// PULUMI_BACKEND_URL takes precedence over the login `pulumi login`
	// recorded in ~/.pulumi/credentials.json, so no fort command depends
	// on one, and no worktree's login redirects another checkout's state.
	if err := b.env(root); err != nil {
		return err
	}
	// Pulumi.yaml: passphrase secrets provider, passphrase itself
	// sops-managed.
	if os.Getenv("PULUMI_CONFIG_PASSPHRASE") == "" {
		v, err := secret(root, "pulumi-passphrase")
		if err != nil {
			return fmt.Errorf("PULUMI_CONFIG_PASSPHRASE not set and no pulumi-passphrase in provisioning/secrets.yaml")
		}
		os.Setenv("PULUMI_CONFIG_PASSPHRASE", v)
	}
	return nil
}

// cloudEnv puts the credentials a pulumi run needs into the environment
// for one provider (the manifest's, or — for destroy — the one the stack's
// checkpoint records: the manifest is gone by then)
// of this process — and so of the pulumi (and its provider) we exec.
// Explicit env wins; otherwise each value is sops-decrypted from
// provisioning/secrets.yaml, the same home as the forge's provisioning
// token: repo-driven, no per-machine env setup, and the values are
// never printed. backendEnv's half comes first and is idempotent, so a
// `preview --all` calling this once per fort is fine.
//
// libvirtURI is the hypervisor the fort records — its manifest's
// [libvirt] uri, or, for a destroy, the value the stack recorded at the
// provision (the manifest is gone by then). "" for every other provider.
func cloudEnv(root string, b stateBackend, provider string, libvirtURI string) error {
	if err := backendEnv(root, b); err != nil {
		return err
	}

	// The hypervisor, reached over ssh from wherever fort runs. The fort
	// names it in its manifest ([libvirt] uri, forts#27) and the CLI
	// passes that value to the provider as it stands — the manifest
	// records the machine, never a credential. The environment still
	// wins, as the escape hatch for a machine whose URI must carry one.
	if provider == "libvirt" {
		uri, err := resolveLibvirtURI(libvirtURIOverride, libvirtURI)
		if err != nil {
			return err
		}
		os.Setenv("LIBVIRT_DEFAULT_URI", uri)
		// The provider applies the domain's XSLT (the qcow2 driver fix,
		// pulumi/libvirt.go) by exec'ing xsltproc; the plugin inherits
		// our PATH, so the tool has to be resolved before pulumi starts.
		if err := tools.Ensure("xsltproc"); err != nil {
			return err
		}
	}
	if provider == "linode" && os.Getenv("LINODE_TOKEN") == "" {
		v, err := secret(root, "linode-token")
		if err != nil {
			return fmt.Errorf("LINODE_TOKEN not set and no linode-token in provisioning/secrets.yaml — add it with: sops --config .sops.yaml set provisioning/secrets.yaml '[\"linode-token\"]' '\"<token>\"'")
		}
		os.Setenv("LINODE_TOKEN", v)
	}
	return nil
}

// libvirtURIOverride is LIBVIRT_DEFAULT_URI as the operator set it,
// read once at startup. cloudEnv WRITES that variable — it is how the
// pulumi child and its provider learn the hypervisor — and `fort
// preview --all` calls cloudEnv once per fort: a later fort must read
// its own manifest, never the value an earlier fort left in the
// environment. Two forts on two hypervisors is the case forts#27 exists
// for.
var libvirtURIOverride = os.Getenv("LIBVIRT_DEFAULT_URI")

// resolveLibvirtURI picks the hypervisor the provider dials. The
// operator's LIBVIRT_DEFAULT_URI wins: it is the one place a URI with a
// credential path in it belongs, and it is how an operator reaches a
// hypervisor the manifest does not name yet. Otherwise the recorded URI
// stands — the fort's [libvirt] uri, or the stack's for a destroy.
// Neither one is a refusal the verb can work around: nothing says which
// machine to dial.
func resolveLibvirtURI(override, recorded string) (string, error) {
	if override != "" {
		return override, nil
	}
	if recorded == "" {
		return "", fmt.Errorf("no hypervisor for this fort: its [libvirt] table names no uri, and LIBVIRT_DEFAULT_URI is not set. Add uri = \"qemu+ssh://<hypervisor>/system\" to the manifest, or export LIBVIRT_DEFAULT_URI for this run")
	}
	return recorded, nil
}

func secret(root, key string) (string, error) {
	cmd, err := tools.Command("sops", "--config", filepath.Join(root, ".sops.yaml"),
		"-d", "--extract", `["`+key+`"]`,
		filepath.Join(root, "provisioning/secrets.yaml"))
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
