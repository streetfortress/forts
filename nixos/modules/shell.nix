# sfi.shell: the interactive shell every fort profile shares — the
# starship prompt (host, directory, branch, a UTC clock that ticks), fzf on
# Ctrl-R, vi mode, a few readers (eza, bat, fd, ripgrep) and a short
# aliases file. For the PERSON at a pane: an operator on a cluster fort as
# deployer, or someone at a herdr pane on a dev fort. An agent's Bash tool
# calls are non-interactive and read none of this.
#
# ZSH IS THAT SHELL (#558, #576). The module turns programs.zsh on itself
# and makes zsh the default login shell, so deployer and agent read the
# same rc and both get the whole list above. It used to wait for a host to
# enable zsh; a cluster fort never did, so deployer read the bash branch
# and two promises of this header went unkept there — the clock did not
# tick and the vi mode showed no indicator (#576).
#
# The bash branch stays, reduced, for the contexts that are still bash:
# `sudo sfi-root` execs `bash -l` (ops/root.nix), and a person runs `bash`
# at a zsh prompt. Its prompt, keys, fzf and aliases work; the tick and the
# mode indicator cannot, for reasons that belong to the shell and not to
# this module (the bash block below says which).
#
# Why nix and not a runtime checkout (#557, sfi/.fortfiles#4): a cluster
# fort has no Home Manager user and clones nothing, so nix content is the
# only thing that reaches both profiles; and the rebuild is the FAST path
# on a fort — auto-rebuild watches main minutely, while clone-dotfiles
# pulls at boot and never again. The rate-of-change argument that put the
# shell in a dotfiles repo (sfi/.fortfiles#1) had that backwards.
#
# System level, not home-manager, for the same reason: /etc/zshrc and
# /etc/bashrc reach deployer (production.nix) and agent (dev-fort.nix)
# alike, and a cluster fort has no Home Manager user at all. The
# zsh-vi-mode plugin is loaded HERE, not by Home Manager, so the module
# owns the whole key chain: the plugin, then fzf after it
# (zvm_after_init_commands), then the Ctrl-R widget that starts with an
# empty query.
#
# The guard is the import, like modules/ops/root.nix: a host gets this
# only by naming the module and setting enable. dev-fort.nix enables it
# for every dev fort; a cluster fort's own default.nix opts in (staging
# does). #558 accepted the module for sfi-prod-439bf72 and settled the zsh
# question above; the production host's own import is a separate change.
# Not in modules/ops/: this is not a verb.
#
# Left out of the personal profile on purpose: the kubectl/KUBECONFIG
# block (fd over a ~/.kube that a fort does not have printed two errors
# before every prompt — sfi/.fortfiles#7), Homebrew/zplug/install.py, fnm,
# the alacritty theme sync, tmux.conf, claude-settings-sync, nvim's
# config. A fort has none of what those serve.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.shell;
  aliases = "/etc/sfi/shell/aliases.sh";

  # zsh: starship, with the personal profile's guard against a stale
  # zle-keymap-select widget (starship#3418; harmless where it never
  # fires, kept so the two profiles start the prompt the same way).
  zshStarship = ''
    if [[ "''${widgets[zle-keymap-select]#user:}" == "starship_zle-keymap-select" ||
          "''${widgets[zle-keymap-select]#user:}" == "starship_zle-keymap-select-wrapped" ]]; then
      zle -N zle-keymap-select ""
    fi
    eval "$(${pkgs.starship}/bin/starship init zsh)"
  '';

  # The clock: starship's [time] block shows the UTC time at the moment the
  # prompt was drawn; TMOUT=1 + TRAPALRM redraws it once a second so it
  # ticks. The WIDGET case list is what keeps fzf and an incremental
  # search from flickering — the redraw is skipped while one of them owns
  # the line editor.
  zshClock = ''
    TMOUT=1
    TRAPALRM() {
      case "$WIDGET" in
        *fzf* | expand-or-complete | self-insert | up-line-or-beginning-search | down-line-or-beginning-search | backward-delete-char | .history-incremental-search-backward | .history-incremental-search-forward)
          : ;;
        *)
          zle reset-prompt ;;
      esac
    }
  '';

  # vi mode first (before starship, the order the laptops run and the one
  # starship's vicmd symbol is known to work in), then fzf after it
  # (zsh-vi-mode rebinds the keymap on its own init at the first prompt,
  # so anything bound before it is lost — the plugin's
  # zvm_after_init_commands is the hook for exactly this), then
  # Ctrl-R as a history search that starts with an EMPTY query instead of
  # the current buffer (the buffer is restored if the search is abandoned).
  # FZF_ALT_C/CTRL_T are emptied: cd-picker and file-picker are noise on a
  # fort; the history search is the one binding that matters.
  zshViMode = ''
    source ${pkgs.zsh-vi-mode}/share/zsh-vi-mode/zsh-vi-mode.plugin.zsh
  '';
  zshKeys = ''
    export FZF_ALT_C_COMMAND="" FZF_CTRL_T_COMMAND=""
    zvm_after_init_commands+=('source <(${pkgs.fzf}/bin/fzf --zsh)')
    zvm_after_init_commands+=('
      _sfi_fzf_history_empty_query() {
        local orig=$LBUFFER
        LBUFFER=""
        zle fzf-history-widget
        [[ -z $LBUFFER ]] && LBUFFER=$orig
      }
      zle -N _sfi_fzf_history_empty_query
      bindkey "^R" _sfi_fzf_history_empty_query
    ')
  '';
in
{
  options.sfi.shell.enable = lib.mkOption {
    type = lib.types.bool;
    default = false;
    description = "The fort's interactive shell for a person at a pane: zsh with starship, a ticking UTC clock, an fzf history, vi mode, eza/bat/fd/ripgrep and the aliases file. This makes zsh the default login shell of the host. A bash context keeps the prompt, the keys and the aliases, but its clock stamps the draw time and its vi mode has no indicator (#576).";
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = with pkgs; [ starship fzf eza bat fd ripgrep fastfetch ];

    # The prompt config and the aliases as files in this repo: STARSHIP_CONFIG
    # is how starship finds a config outside ~/.config, and a session
    # variable reaches login shells and the user manager alike.
    environment.etc."sfi/shell/starship.toml".source = ./shell/starship.toml;
    environment.etc."sfi/shell/aliases.sh".source = ./shell/aliases.sh;
    environment.sessionVariables.STARSHIP_CONFIG = "/etc/sfi/shell/starship.toml";

    # zsh, and zsh as the login shell: the module owns both halves of the
    # promise instead of waiting for a host to enable zsh (#558, #576). An
    # account that does not pin a shell — deployer (production.nix), agent
    # (dev-fort.nix) — follows users.defaultUserShell, because nixpkgs
    # gives every isNormalUser `useDefaultShell = true`
    # (config/users-groups.nix). A fort that does not import this module
    # keeps the nixpkgs default of bashInteractive.
    programs.zsh.enable = true;
    users.defaultUserShell = pkgs.zsh;

    # root keeps bash, deliberately: `sudo sfi-root` execs `bash -l`
    # (ops/root.nix), and a fort's break-glass shell must not depend on the
    # ergonomics module. nixpkgs sets root's shell with mkDefault, so this
    # assignment wins.
    users.users.root.shell = pkgs.bashInteractive;

    # zsh: the prompt of the person at a pane. The order is the point, and
    # the let-block above says why for each part.
    programs.zsh.interactiveShellInit =
      zshViMode + zshStarship + zshClock + zshKeys + ''
        source ${aliases}
      '';

    # bash: the reduced prompt, for a bash context — `sudo sfi-root`'s
    # `bash -l`, a `bash` run at a zsh prompt, an account that pins bash.
    # The prompt, the vi keys, fzf and the aliases all work. Two facts of
    # the zsh prompt cannot follow them here (#576 measured both):
    #   - The clock stamps the time of the DRAW and never ticks after it.
    #     A tick needs a timer that redraws a prompt already on screen:
    #     zsh has TMOUT + TRAPALRM + `zle reset-prompt`, bash has no
    #     equivalent, and TMOUT in bash ends the shell instead.
    #   - The character shows the insert symbol in both vi modes.
    #     `starship init bash` passes no --keymap to `starship prompt`
    #     (`starship init zsh` passes --keymap="$KEYMAP"), so starship
    #     never learns the mode and vimcmd_symbol never renders. readline
    #     can print an indicator of its own (`set show-mode-in-prompt on`),
    #     but it prints at the start of the LAST prompt line, which is the
    #     line starship's character owns — two symbols for one fact. The
    #     module does not set it.
    programs.bash.interactiveShellInit = ''
      set -o vi
      eval "$(${pkgs.starship}/bin/starship init bash)"
      eval "$(${pkgs.fzf}/bin/fzf --bash)"
      . ${aliases}
    '';

    # /etc/zshrc writes promptInit AFTER interactiveShellInit, and nixpkgs
    # defaults that option to `prompt suse`. starship's zsh init assigns
    # PROMPT once (the bash init assigns PS1 inside PROMPT_COMMAND, which is
    # why only zsh lost the prompt), so `prompt suse` replaced it and every
    # dev fort showed the suse theme (#575). Empty is safe: zsh has
    # prompt_sp and prompt_cr on by default, so the theme was the only
    # thing that block added.
    programs.zsh.promptInit = "";

    # `ls` cannot live in aliases.sh: both /etc/bashrc and /etc/zshrc emit
    # environment.shellAliases after interactiveShellInit, and nixpkgs sets
    # `ls = "ls --color=tty"` there (#575). The nixpkgs value is a
    # mkDefault, so this one wins and `l` and `ll` survive.
    environment.shellAliases.ls = "eza -la --group-directories-last";
  };
}
