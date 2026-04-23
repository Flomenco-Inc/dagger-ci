# dagger-ci

Shared [Dagger](https://dagger.io) module that packages the three standard CI
jobs every flo Terraform module runs:

| Function          | Replaces GHA job    | What it does                                                                    |
| ----------------- | ------------------- | ------------------------------------------------------------------------------- |
| `terraform-verify` | "Terraform verify" | `terraform fmt -check`, `terraform init -backend=false`, `validate`, `tflint`   |
| `pre-commit`      | "pre-commit"        | `pre-commit run --all-files` with terraform, tflint, terraform-docs, python      |
| `checkov`         | "Checkov"           | `checkov --framework terraform` → returns SARIF file                             |
| `all`             | (local only)        | Chains the three above. Intended for `dagger call all --src=.` on a laptop.     |

## Why this exists

Before this module, each Terraform repo's `.github/workflows/ci.yml` duplicated
60+ lines of `uses:` / `run:` steps — and each duplicate drifted independently
as tool versions bumped. This module is the single place where tool versions
and invocation flags live. Consumers call it in three lines:

```yaml
- uses: dagger/dagger-for-github@v8
- run: dagger call -m github.com/Flomenco-Inc/dagger-ci@v0.1.0 \
         terraform-verify --src=.
```

Renovate bumps `@v0.1.0` → `@v0.2.0` across all 18 module repos in one grouped
PR; tool-version churn is now O(1) instead of O(n-repos).

## Functions

### `terraform-verify --src=<dir> [--tf-version=1.9.8] [--tflint-version=v0.60.0]`

Fails fast on the first non-zero exit across: `terraform fmt -check -recursive
-diff`, `terraform init -backend=false -input=false`, `terraform validate
-no-color`, `tflint --init`, `tflint --format=compact`.

### `pre-commit --src=<dir> [--tf-version=1.9.8] [--tflint-version=v0.60.0] [--terraform-docs-version=v0.20.0]`

Runs `pre-commit run --all-files`. The module seeds a throwaway `git init`
inside the container because `pre-commit` refuses to run in a non-git
directory. CI already has a real git checkout, but local `dagger call` runs
from the current working directory (which may or may not be a git repo).

### `checkov --src=<dir> [--version=3.2.490] [--soft-fail=true]`

Runs Checkov against the source and returns the `results_sarif.sarif` file as
a Dagger `*File`. Consumers can `export` it and upload to GitHub code-scanning:

```bash
dagger call checkov --src=. export --path=./results_sarif.sarif
```

Findings are surfaced via SARIF; the container exits 0 by default (`--soft-fail`
is on) so CI jobs only fail on infrastructure problems, not policy hits. Flip
to `--soft-fail=false` once the skip list is fully curated (tracked in the
main migration plan).

### `all --src=<dir>`

Runs the three above sequentially. Local-dev convenience. In CI, prefer the
individual functions so logs attribute failures cleanly.

## Tool-version defaults

Defined as constants at the top of `main.go`:

- Terraform: `1.9.8`
- TFLint: `v0.60.0`
- terraform-docs: `v0.20.0`
- Checkov: `3.2.490`
- Python: `3.12`

To propose a bump, open a PR that edits the `const (...)` block. Renovate
groups Dagger-module version bumps and surfaces upstream tool releases.

## Local usage

Requires [Dagger CLI](https://docs.dagger.io/getting-started/installation/)
>= v0.20.6 and a running Docker/Podman daemon.

```bash
# From a Terraform module repo:
dagger call -m github.com/Flomenco-Inc/dagger-ci@v0.1.0 \
  all --src=.
```

For iterative development on the module itself:

```bash
cd dagger-ci
dagger develop                # regenerate internal/dagger/ SDK bindings
dagger functions              # list exposed functions + signatures
dagger call terraform-verify --src=../terraform-aws-network
```

## CI integration

A minimal GitHub Actions workflow that uses this module from a consumer repo:

```yaml
name: ci
on:
  pull_request:
    branches: [main]
  push:
    branches: [main]
jobs:
  verify:
    name: Terraform verify
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: dagger/dagger-for-github@v8
        with:
          version: v0.20.6
          call: call -m github.com/Flomenco-Inc/dagger-ci@v0.1.0 terraform-verify --src=.
  pre-commit:
    name: pre-commit
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: dagger/dagger-for-github@v8
        with:
          version: v0.20.6
          call: call -m github.com/Flomenco-Inc/dagger-ci@v0.1.0 pre-commit --src=.
  checkov:
    name: Checkov
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: dagger/dagger-for-github@v8
        with:
          version: v0.20.6
          call: call -m github.com/Flomenco-Inc/dagger-ci@v0.1.0 checkov --src=. export --path=./results_sarif.sarif
      - if: always() && hashFiles('results_sarif.sarif') != ''
        continue-on-error: true
        uses: github/codeql-action/upload-sarif@v4
        with:
          sarif_file: results_sarif.sarif
          category: checkov
```

The canonical version of this workflow lives in
`flo-account-admin/scripts/templates/module-ci.yml` and is rolled out to all
18 `terraform-aws-*` repos via `scripts/apply-ci-templates.sh`.

## What this module does NOT do

- **No AWS credentials handling.** This module is pre-apply CI only. Anything
  that calls AWS APIs (terragrunt plan, apply) belongs in
  [`dagger-terragrunt`](https://github.com/Flomenco-Inc/dagger-terragrunt).
- **No test runners** (go test, pnpm test, pytest). Deferred — flo's
  application code is not yet migrated, and the monorepo uses Turborepo for
  those already.
- **No release cutting.** Release automation lives in release-please + GitHub
  Actions, not Dagger.

## Versioning

- Patch: tool-version bump (Renovate).
- Minor: new function or new arg with default (backwards-compatible).
- Major: breaking signature change or function removal.

Consumers pin via `@vX.Y.Z`. Rolling tags (`@v0`) are not published — Dagger
modules resolve Git refs, and a floating tag would surface upstream breakage
to every module at once.
