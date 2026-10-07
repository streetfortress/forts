# nixos/certs

`step-ca-root.pem`: the lab CA every fort trusts (`estate.nix`,
`security.pki.certificateFiles` — the trust is there when this file
exists, and absent when the estate has no CA). Put it here before the first
`fort image`; the same file goes to `flux/infrastructure/configs/
step-ca-root.pem`, where cert-manager's ClusterIssuer and `fort add`
read it. `fort init` cannot write it: the CA is the estate's.
