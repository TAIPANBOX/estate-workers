# CLAUDE.md, working instructions for estate-workers

Process and invariants only. English throughout, no long dashes.

## Loop

1. Branch off `main`. Never let a worker edit this repository (it is on the
   hard deny list on purpose).
2. Every new test runs red first against the code without the change.
3. `go vet ./... && test -z "$(gofmt -l .)" && go test -race ./...`
4. `go run ./cmd/mutants`: every mutant killed. A new money, reach or
   credential check gets a mutant in the same change.
5. A new behaviour gets a scenario in `features/workers.feature` with a
   `# test:` binding.

## Invariants

1. **A run never uses a subscription login.** argv starts with `--bare -p`.
   *(test: `TestArgsAreBareAndPrintAndNeverCarryTheKey`, mutant M07)*
2. **The API key is never in argv and appears once in the run's
   environment, which is built from nothing.**
   *(test: `TestEnvIsBuiltFromNothingAndCarriesTheKeyOnlyOnce`)*
3. **No run starts unless spend so far plus its own cap fits every cap;**
   ten cents in total until billing is verified.
   *(tests in `internal/budget`, mutants M01 to M04)*
4. **The ledger over-counts, never under-counts.** Unknown cost is charged
   at the cap; an untrusted line stops the workers.
   *(test: `TestBilledCostFailsClosed`, `TestMalformedLedgerStops`)*
5. **The model never holds a GitHub credential.** Only the push service,
   as its own user, mints a one-repository token.
   *(test: `TestRepoTokenAsksForOneRepoAndTwoPermissions`; structural:
   `deploy/systemd`)*
6. **Nothing leaves the host unless the push service's own checks pass:**
   base on the remote default branch, bundle head equals manifest head,
   allowed paths, regular files, size limits, secret scan.
   *(tests in `internal/push`, mutants M15 to M18)*
7. **Templates cannot widen their own tools.**
   *(test: `TestTemplateRefusesBroadOrDangerousTools`, mutants M24 to M26)*
8. **Hard-denied repositories stay out whatever the config says.**
   *(test: `TestHardDeniedRepoCannotBeListed`, mutant M29)*
9. **Fable and Mythos never run; Opus needs a reason.**
   *(tests in `internal/models`, mutants M27, M28)*
10. **Git never runs a hook or an fsmonitor from a repository.**
    *(test: `TestHostileRepoConfigCannotRunCommands`, mutant M32)*
11. **A stop file halts a running model within seconds and its cost is
    still recorded.**
    *(test: `TestAStopFileCancelsARunningModelAndTheSpendIsStillRecorded`, mutant M35)*
12. **The gates cannot change what is pushed.** They run in a throwaway
    clone of the committed branch; the delivered tree and git directory are
    never theirs.
    *(test: `TestGatesRunInADisposableCloneOfTheBranch`, mutant "the gates run in the delivered tree")*
13. **A template's environment is only NAME_CONFIG pointing at its own
    files,** which the model cannot edit and the gates read only.
    *(tests: `TestTemplateFilesAndEnvStayNarrow`, `TestTemplateFilesReachTheModelAndTheGates`;
    mutants "a template variable may ...")*
14. **Every program a gate starts is installed by the installer.**
    *(test: `TestInstallerProvidesEveryGateProgram`)*
15. **The model's shell cannot make a stop appear.** Claude Code's sandbox
    creates a denied file that is missing while a command runs, so the
    auto stop lives in a denied directory and no deny names a single file.
    *(tests: `TestDenyNamesNoSingleFileTheSandboxWouldCreate`,
    `TestAutoStopLivesInADeniedDirectoryThatExistsBeforeAnyRun`)*

## Escalate, do not push through

- Anything that merges, tags, releases or publishes.
- Widening the App's permissions or the sandbox network allowlist.
- Raising a cap or adding a model.
