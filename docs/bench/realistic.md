# Realistic multi-file benchmark

[realistic.yaml](realistic.yaml) contains ten real Relayweft changes. Each starts
at a fixed historical parent commit and has regression tests from the known
solution. The agent sees a behavior-oriented prompt and the starting source,
without the repository history, remote, solution objects or hidden tests.

| Task | What it exercises |
|---|---|
| affected-module-index | Jest dependency coverage and Gradle layouts |
| resume-undo-coverage | Resumed tasks retaining earlier file provenance |
| azure-api-edge-cases | Azure issues, reviews, identity and token scoping |
| dashboard-auth-failures | Authentication failures versus quota limits |
| bitbucket-api-edge-cases | Pagination, reviews, URL/auth checks and generated text |
| process-ancestry | Agent environment and process ancestry across wrappers |
| pool-lock-probes | Concurrent lock probes and preservation of unfinished work |
| package-manager-paths | Installation ownership and safe updater decisions |
| setup-safe-backups | Backups, permissions, broken CLIs and interrupted setup |
| routing-evidence-thresholds | Statistical evidence and consistent routing reports |

Every solution changes at least two production files. The suite spans CLI,
orchestrator, OS integration, service adapters and reporting. It is one Go
project's history, so it does not measure general performance across languages
or unfamiliar repositories. Public historical solutions may also occur in a
model's training data; no claim of an uncontaminated benchmark is made.

## Current scoring quarantine

Four historical cases are disabled for new model runs: affected-module-index,
azure-api-edge-cases, dashboard-auth-failures and process-ancestry. Their manifest
entries state the scoring problem. The runner prints skipped tasks, refuses an
explicit `--only` request for a disabled task, and spends no quota on it. The
historical ten-task table and matrix below describe the original design; only
six cases are currently eligible. `--validate` still allows diagnosis, but a
known-solution pass alone does not repair an unspecified internal contract.
See [the adoption gate](../benefit-trial.md) before drawing quality conclusions.

## Validate the corpus without models

Use a full checkout containing the referenced commits, with the Go version in
`go.mod` and Git 2.38 or later:

```sh
go build -o rw ./cmd/rw
./rw bench --file docs/bench/realistic.yaml --validate --check-timeout 3m
```

On Windows use `rw.exe`/`.\rw.exe`. Validation requires the known solution to
pass and its base to fail after protected tests are restored. It clears the
workspace between both checks. On 6 October 2026 all ten cases passed this
validation on Windows. Focused Go test filters avoid unrelated historical
failures; the manifest lists the exact commands. A passing benchmark check is
not a claim that the entire historical test suite passes.

## Compare the modes

```sh
./rw doctor
./rw bench --config docs/bench/realistic-config.yaml --file docs/bench/realistic.yaml
```

The matrix is ten tasks × four modes × three repetitions = **120 runs**. It
uses real provider quota and asks before starting. Use `--only TASK_NAME` for a
pilot, or use [pilot.yaml](pilot.yaml) for exactly one task × four modes × one
repetition. `--only` alone retains the manifest's three repetitions. The provided configuration
sets stop thresholds of 200,000 fresh tokens and $10 API-equivalent cost per
run. `budget.reserve: true` reserves each starting agent's median usage estimate
and holds 20% of each limit for review/fixes in routed tasks with final review.
Parallel candidates wait for running agents to finish accounting; an extra
candidate that still cannot fit is skipped explicitly, keeping completed work.
These are admission estimates, so an in-flight agent can still exceed a limit;
20% does not guarantee enough for every fix cycle. Those estimates do not represent
subscription charges. Provider quota pressure can still stop or
change routes; retain the session logs and `rw doctor` output with results.

- **single:** one Codex agent, without planning or review.
- **routed:** normal role-based planning, work and review.
- **routed-tiers:** routed work with difficulty-based model tiers enabled.
- **routed-bestof:** two candidate routes per writing step, with selection and review.

`fair: true` disables learned routes, prior-note/context hand-off and implicit
tiers/best-of behavior. The named mode enables only its own option. Ignored
workspace files are deleted before every run, preventing answers or generated
files from carrying over. Mode order rotates across tasks and repetitions.
Dependency downloads, compiler caches outside the workspace and provider load
are not reset; order rotation reduces but cannot remove those effects. This
mode is isolation for comparison, not a security sandbox against a malicious
agent with unrestricted filesystem access.

Protected test files are restored before the final check even if an agent
modified or deleted them. Success means that check passed; `agent_ok` records
the agent pipeline's own outcome separately. Markdown summarizes pass rates,
time and tokens. JSON includes every task, mode, repetition, check outcome,
agent outcome, full-run wall time, cost and failure note. Wall time includes
setup, agents and checks; failed/timed-out attempts remain in the denominator.

Results are now checkpointed before each run, after its agents finish and after
its check. The version-2 JSON adds status, base/test commits, scoring command and
an artifact directory. Each run keeps `solution.patch` (including untracked and
binary changes), kept candidate patches with a branch index, `events.jsonl`,
full `check.txt` and setup output when setup ran. Evidence is outside the
disposable workspace. If saving evidence fails, the matrix stops before another
reset. A `running` or `checking` row is unfinished, not a completed score. Missing
final CLI usage is marked `incomplete` and totals are lower bounds.

The first pilot task also supplies public `agent_checks`: existing affected
tests and a `gofmt -l` probe. These are separate from the hidden scoring command.
Claude executes them through its normal CLI before planning or implementation;
successful tool results are required. A denial, failed command, missing result,
or two-minute probe timeout stops that run before implementation. Probe usage
and time count toward its cost and budget. Codex receives the same public checks
and allowed commands but has no Claude-specific permission probe. Other tasks
can opt in with their own short, passing `agent_checks`; use `verify.preflight`
for normal project runs. Choose probes that pass on the starting tree, since an
expected red test cannot demonstrate a successful preflight. An allowed probe
does not prove every later shell command will be allowed.

The global defaults keep preflight and reservations off for compatibility; the
provided pilot configuration enables reservations and scoped formatter rules.
The original pilot below used the previous behavior. A
[four-mode rerun](2026-10-06-pilot-rerun.md) with these settings also scored
0/4. Reported usage stayed below the token thresholds and a best-of candidate
landed, but all routed modes stopped before completing required work. Resolve
the remaining plan-budget and command-variation issues before expanding.

The suite and its comparison/reporting path are covered with scripted agents.
A [four-mode, one-task pilot on 6 October 2026](2026-10-06-reliability-benefits.md#benefit-experiment)
completed with **0/4 protected-check passes**. All three routed attempts hit
the token budget; best-of also had unreported usage from a cancelled candidate.
The other nine tasks and the repeated matrix remain unrun. Neither pilot nor
historical solution/base validation establishes a general advantage for
routing, tiers or best-of-N.
