# fort init — create an estate

```
fort init --name <estate> --repo <git url> --admin <age1…> [--admin …] [--operator-key <"ssh-ed25519 … <device>"> …] --forts <flake url> [--known-hosts <line>] [--acme-email <mailbox>] [--mesh <cidr>] [--interface <name>] [--zone <name>] [--hub <role>=<fort>@<estate git url> … [--forge-name <name>]] [--forts <flake url>] [--write]
```

`init` renders, into the root of a git checkout, the files every other
verb assumes are there. It is the first verb an estate runs and the only
one that runs with no `estate.toml`. Like [`scaffold`](scaffold.md) it is a
pure render: no secrets, no network, nothing overwritten — a re-run keeps
every file that exists.

## What it renders

```
● estate.toml                     the estate: name and the operator keys (if given), then [source], [mesh] and
                                  [acme] (as given), then the hubs by role (if any)
● README.md                       the layout and the first fort
● justfile                        the estate's own verbs, which are not the CLI: fetch-kubeconfig (one fort's
                                  read-only ops kubeconfig) and updatekeys (re-encrypt every sops file)
● .sops.yaml                      the flux rules: provisioning/, the shared infrastructure rule,
                                  the `# fort-managed` marker fort mint appends cluster recipients under
● nixos/.sops.yaml                the per-host rules: the catch-all with the admins; mint adds a block per fort above it
● nixos/flake.nix                 the estate flake: sfi/forts as inputs.forts, nixpkgs and sops-nix following it,
                                  hosts auto-discovered under hosts/, an image package per host, fort re-exported
● nixos/certs/README.md           where the lab CA goes
● mesh/peers.json                 empty; mint adds the first peer
● provisioning/README.md          what secrets.yaml holds
● provisioning/pulumi/Pulumi.yaml the project file: `runtime: go`, `binary: fort-pulumi`
● flux/infrastructure/            the shared flux layer: controllers + configs, minus the two files below
● .gitea/workflows/checks.yaml    the gate: `nix run ./nixos#fort -- check --all` + a nix eval of every host; a layer's step lands after (layers.md)
● nixos/hosts, nixos/secrets, flux/apps, flux/clusters, provisioning/pulumi/state   (.gitkeep)
```

## What it cannot write

The estate's own, named in the report:

- `nixos/certs/ca-root.pem` — the estate's CA root, when it runs a private CA. `estate.nix` trusts the first file on every host of the estate where the file exists; an estate with no CA provides neither.
- `flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml` — the edge Alertmanager's webhook.
- `provisioning/secrets.yaml` — the fleet secrets (`provisioning/README.md` lists the keys).

## estate.toml

The loose keys (`name`, `operator_keys`) come before the first table:
TOML reads a loose key after a table header as that table's key
(forts#161).

```toml
name = "second-estate"
operator_keys = [            # the devices that hold `deployer` on every fort of this estate
  "ssh-ed25519 AAAAC3Nza… macbook-air",
]

[source]                     # where the estate's tree comes from (forts#161)
repo = "ssh://git@gitea.zen.lofi:30022/sfi/second-estate"   # flux's source, auto-rebuild's pull
known_hosts = "[forge.example]:22 ssh-ed25519 AAAA…"   # the forge's SSH host key: every clone a fort makes pins it (fort init --known-hosts)
# forge = "gitea"            # the forge's kind; absent, the host of repo derives it (github.com: github)
# auth = "ssh"               # the clone route; absent, the scheme of repo derives it: ssh:// is ssh, https:// is none

[mesh]                       # the estate's WireGuard mesh (mesh.md)
cidr = "10.2.0.0/24"   # the subnet; fort check keeps every mesh address in it, and its prefix is what the interfaces render
interface = "wg-second"   # the mesh interface's name on every host; absent: wg0
zone = "second-estate.mesh"   # every host on the mesh is <host>.<zone> (mesh.md); absent: <name>.mesh

[acme]
email = "ops@example"   # set, the shared layer carries an ACME ClusterIssuer; absent, no public issuer
# directory = "https://acme-v02.api.letsencrypt.org/directory"   # absent: Let's Encrypt. A private directory
                             # (a step-ca) goes with the estate's CA file (nixos/certs/ca-root.pem)

[hubs.forge]                  # the roles another fort serves this estate (hub.md)
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra"   # the estate that holds the hub, by its git URL
fort = "sfi-fort-zenbook"
name = "gitea.zen.lofi"       # the forge's one name on every mesh

[hubs.monitoring]            # a layer's role (layers.md): the core renders nothing for it
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra"   # this estate's own repo URL for an in-estate hub
fort = "central"
```

`scaffold` renders the FluxInstance source and `sfi.auto-rebuild.repoUrl`
from `[source] repo`. Two clone routes exist, and the scheme of `repo`
picks one (forts#167):

- **ssh** — `ssh://git@<forge>:<port>/<owner>/<name>`. Flux and the
  rebuild loop clone with the deploy key `fort mint` registers on the
  forge, and both pin the forge's SSH host key (`known_hosts`). The
  forge's token (`gitea-token`, `github-token`) is what mint and destroy
  need.
- **none** — `https://<forge>/<owner>/<name>`, the anonymous route: a
  public repo. Flux's source-controller, nix's flake fetcher and the
  rebuild loop's `git ls-remote` clone it with no credential, and TLS
  trust comes from the system roots (the estate's own CA under
  `nixos/certs` is additive, so a lab estate on this route still
  works). No deploy key is minted or registered, no forge token is
  read, no host key is pinned, the FluxInstance names no `pullSecret`,
  and `nixos-pull --from <ip>` is refused — an address in the forge's
  place fails the certificate check. `tests/public-estate` is that
  estate.

`auth` names the route explicitly when you want it written down; the
scheme and the route must agree (an `ssh://` repo with `auth = "none"`
and an `https://` repo with `auth = "ssh"` are both refused), and
`auth = "token"` names a route that does not exist. The hubs are roles
([hub.md](hub.md)): `--hub
<role>=<fort>@<estate git url>` once per role (the fort comes first
because the URL carries an `@` of its own, and the forge's name is
`--forge-name gitea.zen.lofi`). The forge is the one role the core
renders for. Any other role is a name a layer or the estate serves: the
core writes the table and nothing else, and the layer's init renders
what the role asks of each fort ([layers](layers.md)). The
`[[serves.<role>]]` tables of a hub's estate are a hand edit after init.

The forge's kind derives from the host of `repo` — github.com is
`github`, every other host is `gitea` — and `forge` overrides it
(forts#1). The kind picks the deploy-key API `fort mint` and `fort
destroy` speak and the token they read from `provisioning/secrets.yaml`
(`gitea-token`, `github-token`); `fort check --all` names both. A hosted
forge with a public name is the whole declaration: no `[hubs.forge]`, no
mesh pin, no lab CA — `tests/github-estate` is that estate. A
fort-hosted forge is `[hubs.forge]` ([hub.md](hub.md)).

`--forts` is required: the framework's flake URL the estate pins, at a
release tag. `--known-hosts` is the forge's SSH host key as one
known_hosts line (`ssh-keyscan -p <port> <forge>` prints it); the
framework ships none (forts#139), so on the ssh route a fort of an
estate that names none does not evaluate, and `fort check --all` says
so. On the anonymous route the key is unread: init asks for none, and
the gate reports "no host key to pin" (and warns if one is named
anyway). `--acme-email` makes
the shared layer carry an ACME ClusterIssuer for the estate's public
names; without it, no public issuer is rendered. The issuer speaks to
`[acme] directory`, Let's Encrypt's production directory when the estate
names none.

`[hubs.forge]` buys the forge pin. `modules/lab-dns.nix` pins the forge's
one name on every mesh (`forge.streetfortress.cloud`) to the address `fort` has in
`mesh/peers.json`, in `/etc/hosts` and in the cluster DNS, on every fort
of the estate (forts#26; [mesh.md](mesh.md)). The name must sit under a
domain (forts#172). The address is written in
that entry and nowhere else. An estate that names no forge host gets no
pin, and `fort check --all` says so.

Adding a hub role after init is a hand edit of `estate.toml` for the
core, and the layer's own verbs for what the role renders
([layers](layers.md)); `flux/infrastructure` is the estate's once
rendered, and `cluster-vars.yaml` is never overwritten by scaffold (it
appends the `ESTATE_NAME` line and nothing else; [scaffold.md](scaffold.md)).

## The operators

`deployer` is the operator account of every fort: the ops verbs
([ops.md](ops.md)) are pinned to it, and `ops/console.nix` opens the
console door for it.
**The framework ships no operator key** (forts#81). `operator_keys` is
the one place a key enters an estate — one line per device, in
`authorized_keys` form, with the device as the key's comment — and
`modules/estate.nix` reads the list into
`sfi.ops.deployer.authorizedKeys`, which `modules/ops/deployer.nix`
puts in the account's `authorized_keys` and nothing else adds to.

The list was the framework's until forts#81, and that was a defect: a
NixOS list option merges across modules, so an estate that named its own
devices *added* them to SFI's seven. SFI held the operator account of
every fort this framework built, an estate for another team included,
unless that estate wrote `lib.mkForce` — and nothing asked it to. The
list is the estate's now, and the estate that holds a fort is the team
that operates it.

- `fort init --operator-key "ssh-ed25519 AAAA… macbook-air"`, once per
  device, writes the list; a later device is a hand edit of this file.
- A key with no comment names no device. `fort check --all` warns:
  vetting trims the list by device.
- **An estate that names none has forts that do not evaluate.** The
  assertion in `ops/deployer.nix` refuses a host whose operator account
  no device can reach — the ops verbs would be present and
  unreachable. `fort check --all` reports the same gap against
  `estate.toml`, before the eval.
- One fort that must carry a different set sets
  `sfi.ops.deployer.authorizedKeys` in its own host file, which
  *replaces* the estate's list for that host. `check` names the line: a
  device added to the estate does not reach that fort.

Who may OPERATE is this list. Who may DISPATCH — the agent accounts of a
dev fort — is the estate's dev role, with a list of its own: two
questions, two lists.

## After init

1. `nix flake lock ./nixos` — pin the framework. `--override-input forts
   path:../forts` develops against a local checkout.
2. Provide the three files above and create `provisioning/secrets.yaml`.
3. `forts/<host>.toml` ([manifest.md](manifest.md)), then `fort scaffold`,
   `fort mint`, a PR, `fort image`, `fort provision`.
