package proc

// GitGuard goes before the arguments of every git command sy runs in a
// folder an agent wrote to (a pool worktree, the project folder after a
// step). An agent in a sandbox cannot write the repository's git folder,
// but it can write a submodule's .git file in the work tree and point it
// at a folder it made, with a config of its own. These keep git on this
// machine from acting on such a config: no fsmonitor command, and no
// recursion into submodules (where the config's filters and hooks would
// apply). They pass on to the git processes git starts itself.
var GitGuard = []string{"-c", "core.fsmonitor=false", "-c", "submodule.recurse=false"}
