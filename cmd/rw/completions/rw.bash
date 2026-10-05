# bash completion for rw (Relayweft). Load it in ~/.bashrc:
#   source <(rw completion bash)
# or save it where bash-completion finds it:
#   rw completion bash > ~/.local/share/bash-completion/completions/rw
# Works with bash 3.2 (macOS) and later, with or without bash-completion.

_rw_complete() {
    # The words up to the cursor, split at blanks only: COMP_WORDS also
    # splits at = and :, which --prefer worker=claude and
    # --route worker=codex:gpt-6.1-sol need whole.
    local line="${COMP_LINE:0:COMP_POINT}"
    local -a words
    read -r -a words <<< "$line"
    case "$line" in
        *[[:space:]]) words+=("") ;;
    esac
    [ "${#words[@]}" -ge 2 ] || return 0
    local cur="${words[${#words[@]}-1]}"
    # Bash replaces only its own last word, which starts after any = or
    # : in cur: strip what comes before it from each candidate.
    local token="${COMP_WORDS[COMP_CWORD]}"
    local pre=""
    case "$cur" in
        *"$token") pre="${cur%"$token"}" ;;
    esac

    local out
    out="$(rw __complete "${words[@]:1}" 2>/dev/null)" || return 0
    local directive="" l v tab=$'\t'
    COMPREPLY=()
    while IFS= read -r l; do
        if [ -z "$directive" ]; then
            directive="$l"
            continue
        fi
        v="${l%%"$tab"*}"
        case "$v" in
            "$pre"*) COMPREPLY+=("${v#"$pre"}") ;;
        esac
    done <<< "$out"

    case "$directive" in
        files)
            # complete -o default: readline completes file names.
            COMPREPLY=()
            ;;
        dirs)
            COMPREPLY=()
            if type compopt >/dev/null 2>&1; then
                compopt -o filenames
            fi
            while IFS= read -r l; do
                [ -n "$l" ] && COMPREPLY+=("$l")
            done <<< "$(compgen -d -- "$token")"
            ;;
        nospace)
            if type compopt >/dev/null 2>&1; then
                compopt -o nospace
            fi
            ;;
    esac
    return 0
}

complete -o default -F _rw_complete rw rw.exe
