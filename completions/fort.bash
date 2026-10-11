# fort.bash — bash completion for the fort CLI (forts#113).
#
# The script holds no knowledge of its own: `fort __complete` answers
# every <TAB>. See completions/_fort for the contract — one candidate per
# line, a last line that starts with ':' being the directive.
#
# Install: the fort package puts this in
# share/bash-completion/completions/fort, which bash-completion reads on
# demand. By hand: source it from ~/.bashrc.

_fort() {
  local cur=${COMP_WORDS[COMP_CWORD]}
  local line directive=plain
  local -a candidates=()

  # COMP_WORDS[0] is `fort`, so the index is COMP_CWORD-1 and the words
  # are everything after the command name, the current one included.
  while IFS= read -r line; do
    if [[ $line == :* ]]; then
      directive=${line#:}
    else
      candidates+=( "$line" )
    fi
  done < <(fort __complete "$(( COMP_CWORD - 1 ))" "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null)

  case $directive in
    files)
      mapfile -t COMPREPLY < <(compgen -f -- "$cur")
      compopt -o filenames
      return;;
    dirs)
      mapfile -t COMPREPLY < <(compgen -d -- "$cur")
      compopt -o filenames
      return;;
    nospace)
      compopt -o nospace;;
  esac
  mapfile -t COMPREPLY < <(compgen -W "$(printf '%s\n' "${candidates[@]}")" -- "$cur")
}

# bash splits a word on every character of COMP_WORDBREAKS, so `--version=1`
# arrives as three words. `--version 1` is the form the CLI documents and
# the form this completes.
complete -F _fort fort
