# estate-workers

Unattended Claude Code workers for routine estate work: dossier bumps,
dependency and toolchain bumps, documentation checks, simple pull requests.
They run on one host through `claude --bare -p` with an API key of a
dedicated Console workspace, so they draw the organisation's API credits and
never a subscription's limits. Go, standard library only.

## How a task runs

```
claudew enqueue <template> repo=... param=...   (validated, then queued)
        |
claudew drain (systemd timer, user claudew)
  stop files -> budget -> clone main into a separate git dir
  -> claude --bare -p in the job directory, Bash in a sandbox
  -> spend written to the ledger
  -> git add, diff checked against the template's allowed paths
  -> commit, repository gates in bubblewrap with no network
  -> bundle + manifest
        |
claudew push-serve (socket, user claudew-gh, the only GitHub credential)
  re-validate the task, import the bundle into its own repository,
  base must be on the remote default branch, diff checked again,
  secret scan, push worker/<template>/<id>, open a pull request
```

A person or a supervising session reviews and merges the pull request.
Workers never merge, tag, release, publish or send messages.

## Security model

| Threat | What holds it |
|---|---|
| Spending the subscription's weekly limit | `--bare` reads no OAuth login; the host has no claude.ai login |
| Spend running away | per-run `--max-budget-usd`, day and month caps from the ledger, the workspace spend limit in the Console; ten cents in total until billing is verified |
| A cost that is not reported | charged at the run's cap; an untrusted ledger line stops the workers |
| The model reading the API key | key only in the claude process environment, scrubbed from subprocesses, `/run/credentials` hidden from the sandbox and denied to Read |
| The model pushing, tagging or releasing | the model has no GitHub credential at all; the push service is another user and pushes only `worker/*` after its own checks |
| Repository content steering the model | `--bare` loads no repository hooks, MCP servers or settings; rules say tool output is data |
| A hostile `.git/config` | every git call runs with command-scope config: no hooks, no fsmonitor, https only |
| Allowed tools widening themselves | templates allow pinned read-only git and named commands; config injection, wildcards on diff and status, network tools and shells are refused |
| Gates as an attack path | gates run in bubblewrap with no network, no credentials and only the job tree writable |
| Secrets in a pull request | the push service runs the estate secret scanner with an empty allowlist and stops the workers on a finding |
| Touching protected repositories | a hard deny list in the code (hermes-openwrt, the site, this repository and others) that no config can lift |

The permission allowlist narrows reach; it is not the boundary. A gate can
run code the model edited, so the boundary is the sandbox, the separate
users and the absence of credentials in the model's reach.

## Commands

```
claudew [-config /etc/claudew/config.json] enqueue <template> [repo=R] [model=M] [reason=TEXT] [param=value ...]
claudew drain | status | stop [why] | resume | managed-settings | push-serve
```

Install on the host with `deploy/install.sh`, add credentials with
`deploy/put-credential.sh`, remove everything with `deploy/uninstall.sh`.

## Checks

```
go vet ./... && gofmt -l . && go test -race ./...
go run ./cmd/mutants        # every named mutant must be killed
```

`features/workers.feature` binds each scenario to the tests that prove it;
`features/features_test.go` fails on a scenario without a test or a binding
to a test that does not exist.
