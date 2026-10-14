# The ADMIN rung, for hosts that opt in (#115): `sudo -i` for the host,
# the real kubectl for the cluster, both behind the admin-key challenge
# that is sudo's authentication on a fort (ops/challenge.nix).
#
#   sudo -i                   # a root shell; the ceremony once per terminal and window
#   kubectl get pvc -A        # cluster-admin; the first call on a terminal unlocks
#   kubectl delete cluster -n forts db
#   sudo admin-unlock     # the unlock by hand: mints the kubectl credential
#
# No admin wrappers (forts#151). The two verbs this replaces, an admin
# shell and an admin kubectl, each carried a copy of a tool
# behind a NOPASSWD line — the shell module handed deployer ALL NOPASSWD,
# so a gate anywhere else read as a control and was none. Here the host
# adds one sudoers line, `deployer ALL=(ALL) ALL` WITHOUT NOPASSWD, and
# sudo's PAM stack is the gate: the first unpinned sudo on a terminal
# runs the ceremony, and sudo's timestamp (sfi.ops.challenge.timeout,
# timestamp_type=tty) is the window. The pinned verbs stay NOPASSWD: that
# rule is placed BEFORE theirs (lib.mkBefore), because sudo takes the
# tags of the LAST matching line, and an ALL line after the pinned ones
# would put every verb behind the challenge.
#
# kubectl is native, behind one unlock. deployer's PATH carries a
# `kubectl` (this module's, ahead of k3s's symlink by priority) that
# execs the real one when ~/.kube/admin.config holds a credential that
# has not expired, and runs `sudo admin-unlock` first when it does
# not. The unlock is a PASSWD verb, so it passes through the same PAM
# stack — the ceremony, or the open window of this terminal — and then,
# as root, mints the credential: a kubeconfig whose token comes from the
# TokenRequest API for a cluster-admin service account, with
# expirationSeconds = the timeout. The API server enforces the expiry; a
# lingering terminal holds nothing after it. Why not root's kubeconfig
# directly: a file permission has no window, and a credential valid for
# the timeout alone is the only "when needed" with a meaning.
#
# The service account lives in its own namespace, not kube-system, for
# the audit log's sake: k3s-audit.nix drops the reads and caps the writes
# of every kube-system service account as platform noise, and the admin
# window is the one actor whose every request that log exists to record
# (#618). Outside that list, its reads are Metadata and its writes carry
# the body. The account has no standing token (automount off, no
# Secret): a token exists only between an unlock and its expiry.
#
# The read-only kubeconfig (ops-kubeconfig.nix) stays what an operator
# machine uses from outside, and what every login shell on the host
# reads by default: this module's kubectl steps in for deployer alone,
# and `--kubeconfig <file>` on the command line bypasses it. The guard is
# the import, as before: a host gets this only by naming the module in
# its own default.nix and setting enable, with the reason in the comment
# beside it. Every sudo is logged with its argument list; a shell's
# commands are not, and the module does not claim otherwise.
{ config, lib, pkgs, ... }:
let
  cfg = config.sfi.ops.admin;
  timeout = config.sfi.ops.challenge.timeout;

  ns = "ops-admin";
  sa = "ops-admin";
  credential = ".kube/admin.config"; # under the operator's home

  # Applied by k3s at start (services.k3s.manifests, the path
  # ops-kubeconfig.nix uses for ops-ro), so the account exists before
  # flux does and needs no estate file.
  manifest = pkgs.writeText "ops-admin.yaml" ''
    # The admin rung's service account (sfi/forts, modules/ops/admin.nix).
    # No standing token: admin-unlock mints one through the
    # TokenRequest API for sfi.ops.challenge.timeout minutes.
    apiVersion: v1
    kind: Namespace
    metadata:
      name: ${ns}
    ---
    apiVersion: v1
    kind: ServiceAccount
    metadata:
      name: ${sa}
      namespace: ${ns}
    automountServiceAccountToken: false
    ---
    apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRoleBinding
    metadata:
      name: ${sa}
    roleRef:
      apiGroup: rbac.authorization.k8s.io
      kind: ClusterRole
      name: cluster-admin
    subjects:
      - kind: ServiceAccount
        name: ${sa}
        namespace: ${ns}
  '';

  realKubectl = lib.getExe pkgs.kubectl;

  # deployer's kubectl. Everyone else, root included, gets the real one
  # untouched: root's break-glass stays an explicit
  # KUBECONFIG=/etc/rancher/k3s/k3s.yaml (ops-kubeconfig.nix).
  kubectlWrapper = pkgs.writeShellApplication {
    name = "kubectl";
    runtimeInputs = [ pkgs.coreutils pkgs.gnused pkgs.gnugrep ];
    text = ''
      real=${realKubectl}
      if [ "$(id -un)" != deployer ]; then
        exec "$real" "$@"
      fi
      for a in "$@"; do
        case $a in
          --kubeconfig|--kubeconfig=*) exec "$real" "$@" ;;
        esac
      done

      cred="$HOME/${credential}"

      # The token's own expiry, read from its JWT payload: the file is
      # valid while the API server would still accept it, with 30 s of
      # margin for the call about to be made.
      valid() {
        [ -r "$cred" ] || return 1
        tok=$(sed -n 's/^ *token: *//p' "$cred" | head -n1)
        [ -n "$tok" ] || return 1
        payload=$(printf '%s' "$tok" | cut -d. -f2 | tr '_-' '/+')
        case $(( ''${#payload} % 4 )) in
          2) payload="$payload==" ;;
          3) payload="$payload=" ;;
        esac
        exp=$(printf '%s' "$payload" | base64 -d 2>/dev/null | grep -o '"exp":[0-9]*' | cut -d: -f2)
        [ -n "$exp" ] && [ "$exp" -gt $(( $(date +%s) + 30 )) ]
      }

      if ! valid; then
        echo "kubectl on $(hostname) is the admin rung: unlocking (sudo admin-unlock)" >&2
        /run/wrappers/bin/sudo /run/current-system/sw/bin/admin-unlock
      fi

      export KUBECONFIG="$cred"
      # `kubectl edit` runs KUBE_EDITOR, else EDITOR, else `vi`, and a
      # fort has none of the three on PATH (#555). An edit writes desired
      # state, which git owns here: flux re-applies the object on its
      # interval and the hand edit goes (#554). Say so when it is typed.
      export KUBE_EDITOR="''${KUBE_EDITOR:-''${EDITOR:-${lib.getExe' pkgs.vim "vi"}}}"
      if [ "''${1-}" = edit ]; then
        echo "note: flux applies most objects on this fort again from git; an edit of such an object lasts one reconcile interval (#554)" >&2
      fi
      exec "$real" "$@"
    '';
  };
in
{
  imports = [ ./ops-commands.nix ];

  options.sfi.ops.admin.enable = lib.mkOption {
    type = lib.types.bool;
    default = false;
    description = ''
      The ADMIN rung on this host: `deployer ALL=(ALL) ALL` behind the
      admin-key challenge, so `sudo -i` is the root shell and, on a
      cluster fort, `kubectl` is cluster-admin behind `sudo
      admin-unlock`. One ceremony opens a terminal for
      sfi.ops.challenge.timeout minutes. For a host whose operators are
      the fleet admins; a disposable fort, a migration window.
    '';
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      # Before the pinned NOPASSWD rules (see the header): sudo's tags
      # come from the last line that matches.
      security.sudo.extraRules = lib.mkBefore [{
        users = [ "deployer" ];
        commands = [{ command = "ALL"; options = [ "PASSWD" ]; }];
      }];
    }

    (lib.mkIf config.services.k3s.enable {
      services.k3s.manifests.ops-admin.source = manifest;

      environment.systemPackages = [ (lib.hiPrio kubectlWrapper) ];

      sfi.ops.commands.admin-unlock = {
        description = "ADMIN RUNG: mint the cluster-admin credential kubectl uses, good for ${toString timeout} minutes; deployer's kubectl runs this itself when it holds none";
        challenge = true;
        runtimeInputs = [ pkgs.kubectl pkgs.coreutils pkgs.util-linux pkgs.glibc ];
        script = ''
          if [ "$#" -ne 0 ]; then
            echo "usage: sudo admin-unlock   (no arguments; writes ~/${credential} for the calling operator)" >&2
            exit 64
          fi
          who=''${SUDO_USER:-}
          if [ -z "$who" ] || [ "$who" = root ]; then
            echo "error: run this as the operator, through sudo — the credential is written into the caller's home" >&2
            exit 64
          fi
          home=$(getent passwd "$who" | cut -d: -f6)
          if [ -z "$home" ] || [ ! -d "$home" ]; then
            echo "error: no home directory for $who" >&2
            exit 1
          fi

          export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
          token=$(kubectl -n ${ns} create token ${sa} --duration=${toString timeout}m)
          server=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.server}')
          ca=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
          [ -n "$token" ] && [ -n "$server" ] && [ -n "$ca" ]

          group=$(id -gn "$who")
          install -d -m 0700 -o "$who" -g "$group" "$home/.kube"
          umask 077
          tmp="$home/${credential}.tmp"
          cat > "$tmp" <<EOF
          apiVersion: v1
          kind: Config
          clusters:
            - name: ${config.networking.hostName}
              cluster:
                server: $server
                certificate-authority-data: $ca
          users:
            - name: ${sa}@${config.networking.hostName}
              user:
                token: $token
          contexts:
            - name: ${config.networking.hostName}
              context:
                cluster: ${config.networking.hostName}
                user: ${sa}@${config.networking.hostName}
          current-context: ${config.networking.hostName}
          EOF
          chown "$who:$group" "$tmp"
          mv "$tmp" "$home/${credential}"

          expires=$(date -u -d "@$(( $(date +%s) + ${toString timeout} * 60 ))" +%FT%TZ)
          logger -t sfi-admin-unlock -p auth.notice \
            "admin credential minted: user=$who sa=${ns}/${sa} role=cluster-admin expires=$expires" || {
            rm -f "$home/${credential}"
            echo "error: the journal took no audit line — credential withdrawn" >&2
            exit 70
          }
          echo "kubectl is cluster-admin for $who until $expires (journal: -t sfi-admin-unlock). rm ~/${credential} to drop it early."
        '';
      };
    })
  ]);
}
