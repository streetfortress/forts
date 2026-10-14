# sfi.shell: the interactive shell every fort profile shares — the
# starship prompt (host, directory, branch, a UTC clock that ticks), fzf on
# Ctrl-R, vi mode, a few readers (eza, bat, fd, ripgrep) and a short
# aliases file. For the PERSON at a pane: an operator on a cluster fort as
# deployer, or someone at a herdr pane on a dev fort. An agent's Bash tool
# calls are non-interactive and read none of this.
#
# A FORT MARKS ITS OWN PANE (#582). The prompt is an option —
# sfi.shell.starshipSettings, an attrset — and the file starship reads is
# generated from it, so a fort's own default.nix sets one path and keeps
# every other key. [hostname].style is the label: bright-black on a dev
# fort, bright-yellow on a staging fort, bright-red on a production fort.
# The let-block below says why the module's own values are mkDefault
# definitions in `config` and not the option's `default`.
#
# ZSH IS THAT SHELL (#558, #576). The module turns programs.zsh on itself
# and makes zsh the default login shell, so deployer and agent read the
# same rc and both get the whole list above. It used to wait for a host to
# enable zsh; a cluster fort never did, so deployer read the bash branch
# and two promises of this header went unkept there — the clock did not
# tick and the vi mode showed no indicator (#576).
#
# The bash branch stays, reduced, for the contexts that are still bash:
# `sudo -i` on a host that enables ops/admin.nix is root's login shell,
# which is bash, and a person runs `bash` at a zsh prompt. Its prompt, keys, fzf and aliases
# work; the tick and the mode indicator cannot, for reasons that belong
# to the shell and not to this module (the bash block below says which).
#
# Why nix and not a runtime checkout (#557, sfi/.fortfiles#4): a cluster
# fort has no Home Manager user and clones nothing, so nix content is the
# only thing that reaches both profiles; and the rebuild is the FAST path
# on a fort — auto-rebuild watches main minutely, while clone-dotfiles
# pulls at boot and never again. The rate-of-change argument that put the
# shell in a dotfiles repo (sfi/.fortfiles#1) had that backwards.
#
# System level, not home-manager, for the same reason: /etc/zshrc and
# /etc/bashrc reach deployer (production.nix) and agent (the estate's dev role)
# alike, and a cluster fort has no Home Manager user at all. The
# zsh-vi-mode plugin is loaded HERE, not by Home Manager, so the module
# owns the whole key chain: the plugin, then fzf after it
# (zvm_after_init_commands), then the Ctrl-R widget that starts with an
# empty query.
#
# The guard is the import, like modules/ops/admin.nix: a host gets
# this only by naming the module and setting enable. The estate's dev role
# enables it for every dev fort; a cluster fort's own default.nix opts in (staging
# does). the production fort imports it too: #558 weighed the cost against the
# posture of production.nix, settled the zsh question above, and a person
# accepted the module on 2026-09-27 — that host's comment carries the
# numbers. Not in modules/ops/: this is not a verb.
#
# Left out of the personal profile on purpose: the kubectl/KUBECONFIG
# block (fd over a ~/.kube that a fort does not have printed two errors
# before every prompt — sfi/.fortfiles#7), Homebrew/zplug/install.py, fnm,
# the alacritty theme sync, tmux.conf, claude-settings-sync, nvim's
# config. A fort has none of what those serve. fastfetch left the list for
# a different reason (#582, the package set below says it): the logo costs
# an image and codec stack, and a dev fort already has the tool from
# home/base.nix.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.shell;
  aliases = "/etc/sfi/shell/aliases.sh";

  # THE PROMPT CONFIG IS AN ATTRSET, not a checked-in TOML file (#582): a
  # fort sets one path in its own default.nix and keeps every other key.
  # pkgs.formats.toml renders the merged attrset back into the file
  # starship reads, so the module still owns one prompt and a fort owns its
  # own label. The rendered file is not byte-identical to the TOML that
  # used to live in modules/shell/starship.toml — the generator writes
  # ordinary double-quoted strings where that file used a folded
  # multi-line string and TOML literal strings — and starship reads the
  # same values from it.
  starshipFormat = pkgs.formats.toml { };

  # The shared prompt. These values reach the option as ONE mkDefault PER
  # LEAF (the config block below), and the option's own `default` stays
  # empty on purpose: the module system drops an option default as soon as
  # one definition exists, so a fort that set
  # `sfi.shell.starshipSettings.hostname.style` would have taken the whole
  # rest of the prompt with it. With per-leaf defaults a fort's plain
  # assignment wins the path it names, and nothing else moves (lib.mkForce
  # also works, and is not necessary).
  #
  # The fort label lives in [hostname].style: bright-black is a dev fort
  # (the default here), sfi-stage-d95cc36 sets bright-yellow and
  # the production fort sets bright-red. A person holds several fort shells
  # open, and the prompt is the only label on each one.
  starshipDefaults = {
    "$schema" = "https://starship.rs/config-schema.json";
    add_newline = false;

    # One module per line. The TOML file wrote this as a folded multi-line
    # string with backslash continuations, which is the same single-line
    # value.
    format = lib.concatStrings [
      "$line_break"
      "$username"
      "$hostname"
      "$kubernetes"
      "$directory"
      "$fill"
      "$python"
      "$git_branch"
      "$git_state"
      "$git_status"
      "$time"
      "$line_break $character"
    ];

    username = {
      show_always = true;
      style_user = "bright-blue";
      format = "[$user]($style)";
    };

    hostname = {
      ssh_only = false;
      style = "bright-black";
      format = "[ ⛫ $hostname]($style) ";
    };

    directory = {
      style = "blue";
      truncation_length = 5;
      truncate_to_repo = false;
      format = "$path(bright-white)";
    };

    character = {
      success_symbol = "[>](bold bright-black) ";
      error_symbol = "[>](bold bright-black) ";
      vimcmd_symbol = "[:](bold bright-red) ";
    };

    fill.symbol = " ";

    git_branch = {
      format = "[$branch(:$remote_branch)]($style) ";
      style = "bright-blue";
    };

    # A backslash before a bracket or a parenthesis is starship's escape
    # for the literal character, so every one of these strings carries a
    # real backslash. The TOML file wrote them as literal strings ('…');
    # Nix needs the doubled backslash to mean the same byte.
    git_state.format = "[$state \\($progress_current/$progress_total\\)]($style) ";

    git_status = {
      style = "green";
      untracked = "[$count U](red)[ / ](white)";
      modified = "[$count M](yellow)";
      staged = "[ / ](white)[$count S](green)";
      diverged = "\\[!!!\\] ";
      ahead = "\\[$count ahead\\] ";
      behind = "\\[$count behind\\] ";
      format = "[$ahead_behind](purple)$untracked$modified$staged --(bright-black) ";
    };

    cmd_duration = {
      format = "[\\($duration\\)]($style)";
      style = "bright-yellow";
    };

    time = {
      disabled = false;
      style = "bright-black";
      utc_time_offset = "0";
      format = "[$time UTC]($style)";
    };

    shell = {
      disabled = false;
      format = " [$indicator]($style)";
    };

    python = {
      disabled = false;
      format = "[\\[($virtualenv) $version\\]](green) | ";
    };

    kubernetes = {
      disabled = false;
      format = "[ ⎈ ](blue)[($context)](bright-blue) : ";
    };
  };

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
  options.sfi.shell = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "The fort's interactive shell for a person at a pane: zsh with starship, a ticking UTC clock, an fzf history, vi mode, eza/bat/fd/ripgrep and the aliases file. This makes zsh the default login shell of the host. A bash context keeps the prompt, the keys and the aliases, but its clock stamps the draw time and its vi mode has no indicator (#576).";
    };

    starshipSettings = lib.mkOption {
      type = starshipFormat.type;
      default = { };
      example = lib.literalExpression ''{ hostname.style = "bright-red"; }'';
      description = "The starship prompt, as the attrset this module renders to /etc/sfi/shell/starship.toml. The module sets every key of the shared prompt at mkDefault priority, so a fort's own default.nix can set one path and keep the rest: `sfi.shell.starshipSettings.hostname.style = \"bright-red\"` marks a production pane and changes nothing else (#582). The keys are starship's own (https://starship.rs/config/).";
    };
  };

  config = lib.mkIf cfg.enable {
    # The readers a person needs at a pane, and nothing whose closure a
    # cluster fort pays for twice. fastfetch is NOT here (#582): it pulls
    # imagemagick, librsvg, x265, libjxl and libpulseaudio to draw a logo —
    # 219.5 MiB over 99 store paths, against 41.8 MiB over 9 for the six
    # tools below. #558 accepted this module for the production fort on the
    # 41.8 MiB figure, which it measured one day before fastfetch joined
    # the list (#566). A dev fort still has fastfetch and its `ff` alias
    # through home/base.nix; a cluster fort has neither, and aliases.sh
    # guards the alias.
    environment.systemPackages = with pkgs; [ starship fzf eza bat fd ripgrep vim ];

    # The prompt config and the aliases as files in /etc: STARSHIP_CONFIG
    # is how starship finds a config outside ~/.config, and a session
    # variable reaches login shells and the user manager alike. The prompt
    # file is generated from cfg.starshipSettings, so the fort that
    # overrode a key gets its own file; the aliases are still a file in
    # this repo.
    sfi.shell.starshipSettings = lib.mapAttrsRecursive (_path: lib.mkDefault) starshipDefaults;
    environment.etc."sfi/shell/starship.toml".source =
      starshipFormat.generate "starship.toml" cfg.starshipSettings;
    environment.etc."sfi/shell/aliases.sh".source = ./shell/aliases.sh;
    environment.sessionVariables.STARSHIP_CONFIG = "/etc/sfi/shell/starship.toml";

    # $EDITOR: vim ships above so a fort with no Home Manager profile
    # gets a real editor at the pane. nixpkgs sets EDITOR = mkDefault
    # "nano" (programs/environment.nix); this plain assignment wins over
    # that mkDefault, the same way the `ls` alias below wins over
    # nixpkgs's own mkDefault. home/base.nix's Home Manager EDITOR =
    # "nvim" is a different, later-sourced variable, so this line does
    # not touch an estate's Home Manager accounts — it is for deployer
    # (production.nix, no Home Manager user) and any other zsh account
    # this module reaches without one (#630).
    environment.variables.EDITOR = "vim";

    # zsh's own first-run check — not a NixOS default; the generated
    # /etc/zshrc carries none — runs on every login where none of
    # ~/.zshenv, ~/.zprofile, ~/.zshrc, ~/.zlogin exist. Home Manager
    # creates ~/.zshrc for an estate's accounts that have one (programs.zsh.enable,
    # home/base.nix); an account with no Home Manager profile —
    # deployer, production.nix's mkForce — never gets one, and the
    # message repeats on every SSH login (#630). An empty ~/.zshrc
    # silences it: zsh checks only that one of the four files exists,
    # never its content. tmpfiles' `f` line creates a file if it is
    # missing and never truncates one that is already there, so this is
    # a no-op for the three accounts Home Manager already covers.
    systemd.tmpfiles.rules = lib.mapAttrsToList
      (name: u: "f ${u.home}/.zshrc 0644 ${name} ${u.group} -")
      (lib.filterAttrs
        (name: u: u.shell == pkgs.zsh && !((config.home-manager.users or { }) ? ${name}))
        config.users.users);

    # zsh, and zsh as the login shell: the module owns both halves of the
    # promise instead of waiting for a host to enable zsh (#558, #576). An
    # account that does not pin a shell — deployer (production.nix), agent
    # (the dev role) — follows users.defaultUserShell, because nixpkgs
    # gives every isNormalUser `useDefaultShell = true`
    # (config/users-groups.nix). A fort that does not import this module
    # keeps the nixpkgs default of bashInteractive.
    programs.zsh.enable = true;
    users.defaultUserShell = pkgs.zsh;

    # root keeps bash, deliberately: `sudo -i` (ops/admin.nix) is root's
    # login shell, and a fort's break-glass shell must not depend on the
    # ergonomics module. nixpkgs sets root's shell
    # with mkDefault, so this assignment wins.
    users.users.root.shell = pkgs.bashInteractive;

    # zsh: the prompt of the person at a pane. The order is the point, and
    # the let-block above says why for each part.
    programs.zsh.interactiveShellInit =
      zshViMode + zshStarship + zshClock + zshKeys + ''
        source ${aliases}
      '';

    # bash: the reduced prompt, for a bash context — root's shell under
    # `sudo -i`, a `bash` run at a zsh prompt, an account that pins bash.
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
