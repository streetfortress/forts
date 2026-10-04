# nixos/certs

`step-ca-root.pem`: the lab CA every fort trusts (`lab-host.nix`,
`security.pki.certificateFiles`). Put it here before the first
`fort image`; the same file goes to `flux/infrastructure/configs/
step-ca-root.pem`, where cert-manager's ClusterIssuer and `fort add`
read it. `fort init` cannot write it: the CA is the estate's.
