# fish completion for rw (Relayweft). Save it:
#   rw completion fish > ~/.config/fish/completions/rw.fish
# or load it for this session:
#   rw completion fish | source

function __rw_complete
    # fish 4 names it --tokens-expanded (-x); older fish only has -o.
    set -l tokens (commandline -xpc 2>/dev/null; or commandline -opc)
    set -e tokens[1]
    set -l cur (commandline -ct)
    set -l out (rw __complete $tokens "$cur" 2>/dev/null)
    or return
    set -l directive $out[1]
    set -e out[1]
    switch "$directive"
        case files
            __fish_complete_path "$cur"
        case dirs
            __fish_complete_directories "$cur" ""
        case '*'
            printf '%s\n' $out
    end
end

complete -c rw -f -a '(__rw_complete)'
