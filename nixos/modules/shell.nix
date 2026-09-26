# sfi.shell: the interactive shell every fort profile shares — the
# starship prompt (host, directory, branch, a UTC clock that ticks), fzf on
# Ctrl-R, vi mode, a few readers (eza, bat, fd, ripgrep) and a short
# aliases file. For the PERSON at a pane: an operator on a cluster fort as
# deployer, or someone at a herdr pane on a dev fort. An agent's Bash tool
# calls are non-interactive and read none of this.
#
# Why nix and not a runtime checkout (#557, sfi/.fortfiles#4): a cluster
# fort has no Home Manager user and clones nothing, so nix content is the
# only thing that reaches both profiles; and the rebuild is the FAST path
# on a fort — auto-rebuild watches main minutely, while clone-dotfiles
# pulls at boot and never again. The rate-of-change argument that put the
# shell in a dotfiles repo (sfi/.fortfiles#1) had that backwards.
#
# System level, not home-manager, for the same reason: /etc/bashrc and
# /etc/zshrc reach deployer (bash, production.nix) and agent (zsh,
# dev-fort.nix) alike. The zsh half is guarded on programs.zsh.enable —
# a cluster fort runs bash and gets no zsh — and the zsh-vi-mode plugin
# is loaded HERE, not by Home Manager, so the module owns the whole key
# chain: the plugin, then fzf after it (zvm_after_init_commands), then
# the Ctrl-R widget that starts with an empty query.
#
# The guard is the import, like modules/ops/root.nix: a host gets this
# only by naming the module and setting enable. dev-fort.nix enables it
# for every dev fort; a cluster fort's own default.nix opts in (staging
# does; production is #558's decision). Not in modules/ops/: this is not
# a verb.
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
    description = "The fort's interactive shell for a person at a pane: starship with a ticking UTC clock, fzf history, vi mode, eza/bat/fd/ripgrep and the aliases file — for bash always, for zsh when programs.zsh is enabled.";
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = with pkgs; [ starship fzf eza bat fd ripgrep fastfetch ];

    # The prompt config and the aliases as files in this repo: STARSHIP_CONFIG
    # is how starship finds a config outside ~/.config, and a session
    # variable reaches login shells and the user manager alike.
    environment.etc."sfi/shell/starship.toml".source = ./shell/starship.toml;
    environment.etc."sfi/shell/aliases.sh".source = ./shell/aliases.sh;
    environment.sessionVariables.STARSHIP_CONFIG = "/etc/sfi/shell/starship.toml";

    # bash: deployer's shell on a cluster fort, and the other shell on a
    # dev fort. vi mode here too — the operator is the same person at
    # both prompts.
    programs.bash.interactiveShellInit = ''
      set -o vi
      eval "$(${pkgs.starship}/bin/starship init bash)"
      eval "$(${pkgs.fzf}/bin/fzf --bash)"
      . ${aliases}
    '';

    # zsh: only where a host enables it (dev-fort.nix does; a cluster fort
    # does not, and /etc/zshrc is not rendered there at all).
    programs.zsh.interactiveShellInit = lib.mkIf config.programs.zsh.enable (
      zshViMode + zshStarship + zshClock + zshKeys + ''
        source ${aliases}
      ''
    );
  };
}
