# nixos/certs

`ca-root.pem`: the estate's CA root, when it runs a private CA — every
fort trusts it (`estate.nix`, `security.pki.certificateFiles`; the trust
is there when this file exists, and absent when the estate has no CA).
Put it here before the first `fort image`. `fort init` cannot write it:
the CA is the estate's. A layer that renders an issuer from it says where
its copy goes.
