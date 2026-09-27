# The fort's aliases, sourced by /etc/bashrc and /etc/zshrc (modules/shell.nix).
#
# An alias reaches a PERSON at a pane and never an agent: Claude Code runs
# each Bash tool call in a non-interactive shell, and a non-interactive
# shell reads neither file. So `alias grep='rg'` below cannot change what
# an agent runs — do not read this list as a hazard to the tools, and do
# not put anything here expecting an agent to see it.
#
# Small on purpose (sfi/.fortfiles#6): a person on a fort runs git, tea and
# a few readers. Add an alias when a session on a fort wants one; an alias
# for a tool the fort does not carry is a typo waiting to happen.
# shellcheck shell=bash

# git
alias repo='git rev-parse --show-toplevel'
alias branch='git branch --show-current'
alias gs='git status'
alias gd='git diff'
alias gl='git log --date=short --pretty="%h  %cd  %s"'
alias gfs='git fetch; git status'

# readers — the module installs every tool named here.
# `ls` is NOT here: /etc/bashrc and /etc/zshrc emit
# environment.shellAliases after this file, and nixpkgs sets `ls` there. The
# module holds the eza alias in environment.shellAliases instead (#575).
alias cat='bat -p --paging=never'
alias grep='rg'
tree() {
  # tree [depth] [path]
  if [ $# -eq 2 ]; then eza -TL "$1" "$2"
  elif [ $# -eq 1 ]; then eza -TL 2 "$1"
  else eza -TL 2
  fi
}

alias ff='fastfetch'

# tools only some hosts carry (just is the agent's, from home/base.nix)
if command -v just >/dev/null 2>&1; then
  alias j='just'
fi

# typos
alias cim='vim'
alias clera='clear'
alias gti='git'
alias udo='sudo'
