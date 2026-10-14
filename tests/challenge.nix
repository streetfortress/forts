# The admin-key challenge as sudo's authentication (modules/ops/challenge.nix,
# modules/ops/admin.nix; forts#115), driven end to end in a VM:
#
#   - the first unpinned sudo on a terminal prints a block encrypted to the
#     admin age key, and the decrypted nonce opens the terminal;
#   - inside the window the same terminal's sudo asks nothing; another
#     terminal's does; `sudo -k` closes it;
#   - a wrong answer is refused, a pinned verb never asks, a verb the host
#     names in sfi.ops.challenge.verbs does, and the rehearsal verb leaves
#     no window behind;
#   - deployer's kubectl unlocks itself through `sudo admin-unlock`,
#     and the credential it mints is the ops-admin service account for the
#     timeout and nothing longer.
#
# The key pair below is a throwaway made for this test; it decrypts
# nothing anywhere else. Run with
#   nix build .#checks.x86_64-linux.challenge
# on a host with KVM (the fixture needs a cluster, so it is a VM test,
# and CI's eval-only gate does not run it).
{ pkgs, lib, ... }:

let
  testPub = "age1e2v2zc5xxsn7em9amsyh5p4zpeyllcnduwvyznftpkd87w79f53sa9rck3";
  testKey = "AGE-SECRET-KEY-1KTC4D765FY8594JD0K4Q7MYFQJUUDVLUL4X30LAUPUZC8D4P8YTSQ8FY2U";

  # ceremony.exp <good|bad> <command…>: run the command on a pty as the
  # calling user, answer every challenge sudo issues with the decrypted
  # nonce (good) or garbage (bad), and exit with the command's status.
  # The challenge block arrives through sudo's conversation on the pty,
  # which is the point of the `stdout` option on the first pam_exec line.
  ceremony = pkgs.writeText "ceremony.exp" ''
    set mode [lindex $argv 0]
    set cmd [lrange $argv 1 end]
    set timeout 120
    set challenges 0
    match_max 20000
    spawn {*}$cmd
    expect {
      -re {-----BEGIN AGE ENCRYPTED FILE-----.*?-----END AGE ENCRYPTED FILE-----} {
        set block $expect_out(0,string)
        expect -re {nonce for deployer@fort: }
        incr challenges
        if {$mode eq "good"} {
          set f [open /tmp/ceremony-block.age w]
          puts $f $block
          close $f
          set nonce [string trim [exec age -d -i /etc/test-admin-key.txt /tmp/ceremony-block.age]]
          send -- "$nonce\r"
        } else {
          send -- "not-the-nonce\r"
        }
        exp_continue
      }
      eof {}
    }
    set rc [lindex [wait] 3]
    puts "\nceremony.exp: challenges=$challenges exit=$rc"
    exit $rc
  '';
in
{
  name = "challenge";

  nodes.fort = { config, pkgs, ... }: {
    imports = [ ../modules/ops/admin.nix ];

    networking.hostName = "fort";
    # The fixture estate: estate.toml names the operator key deployer.nix
    # requires; the recipients are overridden below, so its .sops.yaml is
    # read by nothing here.
    sfi.estate.root = ./estate;

    sfi.ops.challenge = {
      recipients = [ testPub ];
      timeout = 10;
    };
    sfi.ops.admin.enable = true;

    # A pinned verb, and a verb the host puts behind the challenge.
    sfi.ops.commands.hello = {
      description = "a pinned verb";
      script = "echo hello from root";
    };
    sfi.ops.commands.gated = {
      description = "a verb this host gates";
      script = "echo the gated verb ran";
    };
    sfi.ops.challenge.verbs = [ "gated" ];

    # A cluster for the kubectl half. Nothing needs to schedule: the API
    # server, the service account and the TokenRequest are the test.
    services.k3s = {
      enable = true;
      role = "server";
      extraFlags = [ "--disable=traefik,servicelb,metrics-server,local-storage" ];
    };

    environment.systemPackages = [ pkgs.expect pkgs.age ];
    environment.etc."test-admin-key.txt" = { text = testKey; mode = "0444"; };
    environment.etc."ceremony.exp".source = ceremony;

    virtualisation = {
      memorySize = 2048;
      cores = 2;
      diskSize = 4096;
    };
  };

  testScript = ''
    import base64
    import json
    import shlex

    def as_deployer(cmd):
        """Run cmd as deployer with no terminal: the shape of a script or a
        pipe, where sudo cannot ask."""
        return fort.execute("su - deployer -c " + shlex.quote(cmd + " 2>&1"))

    def ceremony(mode, cmd):
        """Run cmd as deployer on a pty, answering sudo's challenges."""
        return as_deployer(f"expect /etc/ceremony.exp {mode} bash -c {shlex.quote(cmd)}")

    fort.wait_for_unit("multi-user.target")

    with subtest("the PAM stack of the sudo service is the two pam_exec lines and no pam_unix"):
        pam = fort.succeed("cat /etc/pam.d/sudo")
        auth = [l for l in pam.splitlines() if l.startswith("auth ")]
        assert len(auth) == 3, auth
        assert "sfi-challenge-issue" in auth[0] and "requisite" in auth[0], auth
        assert "sfi-challenge-verify" in auth[1] and "expose_authtok" in auth[1], auth
        assert "pam_deny" in auth[2], auth
        assert "pam_unix" not in "\n".join(auth), auth

    with subtest("a pinned verb runs with no terminal and no challenge"):
        rc, out = as_deployer("sudo -n hello")
        assert rc == 0 and "hello from root" in out, (rc, out)

    with subtest("an unpinned sudo with no terminal is refused, not asked"):
        rc, out = as_deployer("sudo -n true")
        assert rc != 0 and "password is required" in out, (rc, out)
        rc, out = as_deployer("sudo -n gated")
        assert rc != 0 and "password is required" in out, (rc, out)

    with subtest("the admin key opens the terminal, and the window holds for the next sudo on it"):
        rc, out = ceremony("good", "sudo id -u && sudo -n gated && sudo -n -i id -un")
        assert rc == 0, (rc, out)
        assert "challenges=1 " in out, out
        assert "the gated verb ran" in out and "\nroot" in out.replace("\r", ""), out

    with subtest("another terminal has no window of its own"):
        rc, out = as_deployer("sudo -n true")
        assert rc != 0 and "password is required" in out, (rc, out)

    with subtest("sudo -k closes the window"):
        rc, out = ceremony("good", "sudo true && sudo -k && sudo -n true")
        assert rc != 0 and "password is required" in out, (rc, out)

    with subtest("a wrong answer is refused, three times, and nothing runs"):
        rc, out = ceremony("bad", "sudo gated")
        assert rc != 0, (rc, out)
        assert "challenges=3 " in out, out
        assert "not the nonce" in out, out
        assert "the gated verb ran" not in out, out

    with subtest("the rehearsal runs the ceremony every time and leaves no window"):
        rc, out = ceremony("good", "sudo challenge && sudo -n true")
        assert rc != 0, (rc, out)
        assert "success: the admin key answered" in out, out
        assert "password is required" in out, out

    with subtest("the journal holds the ceremony"):
        journal = fort.succeed("journalctl -t sfi-ops-challenge --no-pager")
        assert "challenge ISSUED" in journal and "challenge PASSED" in journal, journal
        assert "challenge FAILED (mismatch)" in journal, journal

    with subtest("the stored nonce is root-only and gone after the check"):
        fort.succeed("stat -c '%U %a' /run/sfi-challenge | grep -qx 'root 700'")
        fort.succeed("test -z \"$(ls -A /run/sfi-challenge)\"")

    fort.wait_for_unit("k3s.service")
    fort.wait_until_succeeds("k3s kubectl -n ops-admin get serviceaccount ops-admin", timeout=300)

    with subtest("root's kubectl is the real one, untouched"):
        fort.succeed("kubectl version --client")

    with subtest("deployer's kubectl unlocks itself through the challenge and runs as ops-admin"):
        rc, out = ceremony("good", "kubectl auth whoami")
        assert rc == 0, (rc, out)
        assert "challenges=1 " in out, out
        assert "unlocking (sudo admin-unlock)" in out, out
        assert "kubectl is cluster-admin for deployer" in out, out
        assert "system:serviceaccount:ops-admin:ops-admin" in out, out

    with subtest("the minted credential is the operator's file, and it serves with no terminal"):
        fort.succeed("stat -c '%U %a' /home/deployer/.kube/admin.config | grep -qx 'deployer 600'")
        rc, out = as_deployer("kubectl get namespace ops-admin")
        assert rc == 0 and "ops-admin" in out, (rc, out)

    with subtest("the token expires with the timeout"):
        token = fort.succeed("sed -n 's/^ *token: *//p' /home/deployer/.kube/admin.config").strip()
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        claims = json.loads(base64.urlsafe_b64decode(payload))
        life = claims["exp"] - claims["iat"]
        assert life == 10 * 60, claims

    with subtest("an expired credential sends kubectl back to the unlock, which a terminal-less call cannot pass"):
        fort.succeed("sed -i 's/^\\( *token: *\\).*/\\1eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjF9.x/' /home/deployer/.kube/admin.config")
        rc, out = as_deployer("kubectl get ns")
        assert rc != 0 and "unlocking" in out and "password is required" in out, (rc, out)

    with subtest("the unlock is in the journal"):
        journal = fort.succeed("journalctl -t sfi-admin-unlock --no-pager")
        assert "admin credential minted: user=deployer sa=ops-admin/ops-admin role=cluster-admin" in journal, journal
  '';
}
