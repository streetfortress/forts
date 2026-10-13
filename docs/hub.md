# The hub — a relationship between estates

A **hub** is a fort that serves other forts a role. The forts it serves
may be its own estate's or another's, so the hub is a relationship
between two estates, and `estate.toml` declares it at both ends
(forts#38). The core knows **one role by name: the forge**, the gitea
every fort clones from. Every other role is a name the estate chooses
and the ports the hub opens on the link for it (forts#105): a layer's
role (a monitoring hub's remote_write and push ports, sfi/fort-layers)
or the estate's own (an inference endpoint, a database a hub reads). The
core renders nothing for it. What the hub runs, and what each served
fort renders to reach it, is the layer's or the estate's flux
([layers](layers.md)).

## estate.toml

The served estate names each role another fort serves it:

```toml
[hubs.forge]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/estate-hub"   # the estate that holds the hub, by its git URL
fort = "central"                                           # the hub fort
name = "gitea.zen.lofi"                                    # the forge's one name on every mesh (forge only)

[hubs.monitoring]            # a layer's role: the core renders nothing for it
estate = "ssh://git@gitea.zen.lofi:30022/sfi/estate-hub"
fort = "central"
```

`estate` is the hub's estate by its git URL — the `repo` its own
`estate.toml` carries, so an estate has one spelling everywhere. This
estate's own URL is the in-estate case. The hub's address on this mesh
is its entry in `mesh/peers.json`: its manifest's `mesh_ip` when the hub
is a fort of this estate, its `remotes` entry when it is another's — the
link that puts it on this mesh ([links.md](links.md)). Its name on this
mesh is `<fort>.<zone>` ([mesh.md](mesh.md)); the forge alone keeps a
name of its own, pinned to that address by `modules/lab-dns.nix`.

The hub's estate declares the other half, by role — a fort of its own,
the estates it serves that role, and the ports the link opens:

```toml
[[serves.forge]]
fort = "central"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging", "ssh://git@gitea.zen.lofi:30022/sfi/production"]

[[serves.monitoring]]
fort = "central"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging"]
ports = [30990, 31100]
```

The role is the key at both ends, so `[hubs.monitoring] fort = "central"`
in staging and `[[serves.monitoring]] fort = "central"` in the hub's
estate are one sentence read from either side. `ports` is required for
every role but the forge, and refused on the forge: its pair (30022 and
443) is the core's. Two entries under one role are two hubs. The one
invariant is the served side's — a served estate names one fort per
role — so an estate URL appears at most once across the entries of a
role. An entry for this estate itself is a refusal: an estate consumes
through its `[hubs.*]` tables and provides through `serves`.

## A role is a small contract

| role | the served estate renders | the hub opens on the link | `fort check` |
|---|---|---|---|
| `forge` | the forge's address pinned to its name — `modules/lab-dns.nix` derives `sfi.lab-dns.giteaAddress` from `fort` and `mesh/peers.json` (forts#26) | 30022, 443 | the address derives; a host file that sets it by hand is a warning ([mesh.md](mesh.md)) |
| any other | nothing — the hub is `<fort>.<zone>`; a layer renders its own files, in its own directory | the entry's `ports` | both ends name the role; the ports exist; the link exists (the warning on `fort link add`) |

A role that needs a render on the served side — a dead man's switch file
the hub applies, a ClusterIssuer, a DNS record — is a layer's. The layer
owns `layers/<layer>/` in the estate, provides `fort-<layer>`, and its
scaffold renders what the role asks of each fort ([layers](layers.md)).
The core's line is this table: a name, the ports, the link.

## The link

`modules/links.nix` derives a link's `openTCPPorts` from its `roles`: the
forge's pair, and the `ports` of every other role's serves entry that
names this host and that estate. A link named after a served estate
(`sfi.links.staging`) takes `roles` and `repo` from those entries
([links.md](links.md)). `fort link add` says which roles the link gets,
and warns when no entry names the host for that estate.
