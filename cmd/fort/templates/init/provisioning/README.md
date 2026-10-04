# provisioning

The inputs a fort's birth needs and the record of what was raised.

- `secrets.yaml` — the fleet secrets, sops-encrypted to the admins
  (`.sops.yaml` at the root has the rule). Create it with
  `sops provisioning/secrets.yaml` and the keys the verbs read:
  `gitea-token` (mint: the deploy key), `pulumi-passphrase` (provision),
  `linode-token` (a cloud fort), `restic-password` and the
  `rustfs-access-key` / `rustfs-secret-key` pair (a fort with a backups
  profile), `b2-minting-key-id` (a production backups profile).
- `console-passwords.yaml` — the console registry, written by
  `fort console-password`.
- `pulumi/` — the project file (the program is `fort-pulumi`, built
  beside `fort`), one `Pulumi.<host>.yaml` per stack, and `state/`, the
  committed file:// backend.
