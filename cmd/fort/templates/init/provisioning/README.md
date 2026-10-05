# provisioning

The inputs the first provision of a fort needs, and the record of what
was raised.

- `secrets.yaml` — the fleet secrets, sops-encrypted to the admins
  (`.sops.yaml` at the root has the rule). Create it with
  `sops provisioning/secrets.yaml` and the keys the verbs read:
  `gitea-token` (mint: the deploy key), `pulumi-passphrase` (provision),
  `linode-token` (a cloud fort), `restic-password` (a fort with a
  `[[backups.pv]]` block), `b2-minting-key-id` (a pv block on `b2`).
- `console-passwords.yaml` — the console registry, one passphrase per
  fort: `fort mint` adds a fort's entry at the first provision, `fort
  console-password --rotate` replaces it, `fort dismantle` withdraws it.
- `pulumi/` — the project file (the program is `fort-pulumi`, built
  beside `fort`), one `Pulumi.<host>.yaml` per stack, and `state/`, the
  committed file:// backend.
