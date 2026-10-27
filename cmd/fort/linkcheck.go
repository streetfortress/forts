package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gopkg.in/yaml.v3"
)

// linkFindings (--all; forts#55): the links the manifests declare, judged
// with the estate in hand — what the manifest alone cannot know:
//
//   - a link's estate is not this one, its interface and zone are not
//     this mesh's (a host on two meshes gives each its own names);
//   - the host file sets sfi.links.<name> for every table (nix cannot
//     read the manifest; scaffold appends the block, docs/manifest.md);
//   - public_key is the half the sops secret derives. Deriving needs
//     the admin age key, which CI does not hold, so the key is checked
//     where the secrets file decrypts and reported as not verified where
//     it does not — a skip, never a failure (the decision on forts#55).
//
// The hub side — a fort that serves an estate a role and has no link to
// it — is servesFindings' (clustermon.go).
func linkFindings(root string, est estate.Estate, hosts map[string]manifest.Host, tool secretsTool) []finding {
	var out []finding
	links, unverified := 0, 0
	for _, host := range sortedNames(hosts) {
		h := hosts[host]
		if len(h.Links) == 0 {
			continue
		}
		path := manifest.Path(host)
		hostNix := filepath.Join("nixos/hosts", host, "default.nix")
		nix, _ := os.ReadFile(filepath.Join(root, hostNix))
		// The secrets file, once per host: the private halves.
		var have map[string]string
		var decryptErr error
		secretsPath := filepath.Join("nixos/secrets", host+".yaml")
		if exists(root, secretsPath) {
			raw, err := tool.Decrypt(filepath.Join(root, secretsPath))
			if err != nil {
				decryptErr = err
			} else if err := yaml.Unmarshal(raw, &have); err != nil {
				decryptErr = err
			}
		} else {
			decryptErr = fmt.Errorf("no %s", secretsPath)
		}
		for _, name := range h.LinkNames() {
			links++
			l := h.Links[name]
			at := "[links." + name + "]"
			switch {
			case estate.SameRepo(l.Estate, est.Source.Repo):
				out = append(out, finding{"error", path, at + " names this estate — a fort is on its own mesh by mesh_ip; a link reaches another estate's"})
			case l.InterfaceName() == est.InterfaceName():
				out = append(out, finding{"error", path, fmt.Sprintf("%s interface %s is this estate's own mesh interface (%s) — the other mesh needs a name of its own", at, l.InterfaceName(), estate.Path)})
			case l.ZoneName() == est.ZoneName():
				out = append(out, finding{"error", path, fmt.Sprintf("%s zone %s is this estate's own mesh zone — the other estate's names need a zone of their own", at, l.ZoneName())})
			}
			if nix != nil && !nixSetsAttr(string(nix), "sfi.links."+name) {
				out = append(out, finding{"error", hostNix, fmt.Sprintf("no sfi.links.%s — the manifest declares %s and nix cannot read it; `fort scaffold %s` appends the block", name, at, host)})
			}
			switch {
			case decryptErr != nil:
				unverified++
			case have[linkSecretKey(name)] == "":
				out = append(out, finding{"error", secretsPath, fmt.Sprintf("no %s — %s names a public key whose private half is not in the secrets file; `fort link add %s %s` mints one (a re-key: the other estate's remotes entry follows on its next `fort estate link`)", linkSecretKey(name), at, host, l.Estate)})
			default:
				k, err := wgKeyFrom(have[linkSecretKey(name)])
				switch {
				case err != nil:
					out = append(out, finding{"error", secretsPath, linkSecretKey(name) + ": " + err.Error()})
				case k.public != l.PublicKey:
					out = append(out, finding{"error", path, fmt.Sprintf("%s public_key %s is not what %s derives (%s) — the other estate dials a key this host does not hold; `fort link add %s %s` rewrites the table from the secret", at, abbrevKey(l.PublicKey), linkSecretKey(name), abbrevKey(k.public), host, l.Estate)})
				}
			}
		}
	}
	if links == 0 {
		return nil
	}
	if len(out) == 0 {
		switch {
		case unverified == 0:
			out = append(out, finding{"ok", manifest.Dir, fmt.Sprintf("%d link(s) — tables and host files agree, public keys match the sops secrets", links)})
		case unverified == links:
			out = append(out, finding{"ok", manifest.Dir, fmt.Sprintf("%d link(s) — tables and host files agree; public keys not verified (no age key here)", links)})
		default:
			out = append(out, finding{"ok", manifest.Dir, fmt.Sprintf("%d link(s) — tables and host files agree; %d public key(s) match the sops secrets, %d not verified (no age key here)", links, links-unverified, unverified)})
		}
	}
	return out
}
