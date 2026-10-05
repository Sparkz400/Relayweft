#compdef rw rw.exe
# zsh completion for rw (Relayweft). Save it on your $fpath:
#   rw completion zsh > "${fpath[1]}/_rw"      (then start a new shell)
# or load it after compinit in ~/.zshrc:
#   source <(rw completion zsh)

_rw() {
    local out directive l v d
    local -a lines cands
    out="$(rw __complete "${(@)words[2,CURRENT]}" 2>/dev/null)" || return 1
    lines=("${(@f)out}")
    directive="${lines[1]}"
    case "$directive" in
        files) _files; return ;;
        dirs) _files -/; return ;;
    esac
    for l in "${(@)lines[2,-1]}"; do
        [[ -n "$l" ]] || continue
        v="${l%%$'\t'*}"
        d=""
        [[ "$l" == *$'\t'* ]] && d="${l#*$'\t'}"
        # _describe splits value:description at the first unescaped colon.
        v="${v//:/\\:}"
        if [[ -n "$d" ]]; then
            cands+=("$v:$d")
        else
            cands+=("$v")
        fi
    done
    (( ${#cands} )) || return 1
    if [[ "$directive" == nospace ]]; then
        _describe -t values 'rw' cands -S ''
    else
        _describe -t values 'rw' cands
    fi
}

if [[ "${funcstack[1]}" == _rw ]]; then
    _rw "$@"
else
    compdef _rw rw rw.exe
fi
