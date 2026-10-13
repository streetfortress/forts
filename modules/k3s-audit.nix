# The API server's audit log (#626): the record a bounded admin window
# needs (#618). The gate's journal line records the open, the reason, the
# duration and the role, and nothing after; this is the "after" — every
# request the API server sees, by whom, from where, and for a write by a
# person, with the body. Off by default: every cluster fort audits the
# same WAY, and not every fort pays the same disk for it (a 4 GB Linode
# SKU with a 25 GB disk and the lab hub with 450 GB do not carry the
# same log), so a fort opts in through its manifest — [cluster]
# audit_logging = true — and fort scaffold renders the one line below.
# A test in cmd/fort keeps the manifest and the host file equal.
#
# k3s runs the API server in its own process, so the path is a host
# path. It is under /var/log on purpose: a monitoring layer's log shipper
# DaemonSet already mounts /var/log, so the stream to Loki
# (flux/infrastructure/controllers/fluent-bit) needs no new volume and
# costs nothing on a fort without the file. Loki keeps every stream 30
# days; a longer life for this one is #618's decision, not this
# module's.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.k3s.auditLog;
  dir = "/var/log/kube-audit";
  policyFile = "/etc/rancher/k3s/audit-policy.yaml";

  # The control plane talking to itself is the bulk of every API log and
  # not what the log exists to record. These are the users k3s's own
  # components authenticate as, and the groups its nodes and the
  # platform namespaces' service accounts carry. A namespace is
  # platform here when a controller converges it and no person works in
  # it: cnpg-system runs the CNPG operator and its barman-cloud plugin
  # (flux/infrastructure/controllers/cnpg), which were the two largest
  # sources in the stage fort's first hours of the stream (#653).
  platformUsers = [
    "system:apiserver"
    "system:kube-controller-manager"
    "system:kube-scheduler"
    "system:kube-proxy"
    "system:k3s-controller"
    "system:k3s-supervisor"
  ];
  platformGroups = [
    "system:nodes"
    "system:serviceaccounts:kube-system"
    "system:serviceaccounts:flux-system"
    "system:serviceaccounts:monitoring"
    "system:serviceaccounts:cnpg-system"
  ];

  # Rules apply first match wins, so the noise is dropped first and the
  # secret-bearing resources are capped before any body-logging rule can
  # reach them. omitStages keeps one event per request (the completed
  # one, with the response code); omitManagedFields keeps the bodies
  # that are logged to what a person wrote.
  policy = {
    apiVersion = "audit.k8s.io/v1";
    kind = "Policy";
    omitStages = [ "RequestReceived" ];
    omitManagedFields = true;
    rules = [
      # Probes, discovery and metrics: not a record of anything.
      {
        level = "None";
        nonResourceURLs = [ "/healthz*" "/readyz*" "/livez*" "/version" "/openapi/*" "/api" "/apis" "/metrics" ];
      }
      # The controllers' own reads.
      {
        level = "None";
        verbs = [ "get" "list" "watch" ];
        users = platformUsers;
      }
      {
        level = "None";
        verbs = [ "get" "list" "watch" ];
        userGroups = platformGroups;
      }
      # Never a body for the resources that carry a secret: the token a
      # `kubectl create token` returns (serviceaccounts/token), the data
      # of a Secret, a TokenReview's token, and ConfigMaps, which are
      # bulky and sometimes hold what should have been a Secret.
      #
      # A Lease is here for the other reason: it carries no secret, it
      # carries nothing a person wrote. Its body is a holder identity
      # and a renew timestamp, rewritten every few seconds for as long
      # as an operator runs. The group below quiets the lease of an
      # operator that runs in a platform namespace; this rule reaches
      # the one it cannot — CNPG gives each Cluster its own service
      # account in the *tenant's* namespace (forts on this fort),
      # and that account holds the primary-election lease, so a
      # namespace list would need an edit for every new database
      # (#653). The who, the verb and the name stay in the event.
      {
        level = "Metadata";
        resources = [
          { group = ""; resources = [ "secrets" "configmaps" "serviceaccounts/token" ]; }
          { group = "authentication.k8s.io"; resources = [ "tokenreviews" ]; }
          { group = "coordination.k8s.io"; resources = [ "leases" ]; }
        ];
      }
      # The controllers' writes: who, what, when — flux reconciling and
      # the operators converging are not a person's hand.
      {
        level = "Metadata";
        users = platformUsers;
      }
      {
        level = "Metadata";
        userGroups = platformGroups;
      }
      # Every write by anyone else, with the request and the response:
      # this is the record #618 needs.
      {
        level = "RequestResponse";
        verbs = [ "create" "update" "patch" "delete" "deletecollection" ];
      }
      # Everything else: who, what, when, from where.
      {
        level = "Metadata";
      }
    ];
  };
in
{
  options.sfi.k3s.auditLog = {
    enable = lib.mkEnableOption "the k3s API server's audit log under ${dir}, shipped to Loki by a monitoring layer's log shipper";
  };

  config = lib.mkIf cfg.enable {
    # Real YAML, not toJSON: a person reads this file on the host.
    environment.etc."rancher/k3s/audit-policy.yaml".source =
      (pkgs.formats.yaml { }).generate "audit-policy.yaml" policy;

    # Rotation caps the log at 400 MB on disk: 100 MB a file, three
    # rotated files kept, none older than a week. The stage fort measures
    # the real rate for a week before #618 settles the caps.
    services.k3s.extraFlags = [
      "--kube-apiserver-arg=audit-policy-file=${policyFile}"
      "--kube-apiserver-arg=audit-log-path=${dir}/audit.log"
      "--kube-apiserver-arg=audit-log-maxsize=100"
      "--kube-apiserver-arg=audit-log-maxbackup=3"
      "--kube-apiserver-arg=audit-log-maxage=7"
    ];

    # Root-only: the file names every principal and, for a write, the
    # body. fluent-bit reads it as root through its read-only /var/log
    # mount.
    systemd.tmpfiles.rules = [ "d ${dir} 0700 root root -" ];
  };
}
