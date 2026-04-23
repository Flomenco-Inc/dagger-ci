// Package main exposes the Flomenco `dagger-ci` Dagger module.
//
// The module wraps the three CI jobs that every flo Terraform module currently
// runs as GitHub Actions steps (see
// flo-account-admin/scripts/templates/module-ci.yml):
//
//  1. terraform fmt / init / validate / tflint   → TerraformVerify
//  2. pre-commit run --all-files                 → PreCommit
//  3. checkov --framework terraform              → Checkov
//
// Packaging them as a Dagger module gives us two wins:
//
//   - **Local parity with CI.** Running `dagger call terraform-verify --src=.`
//     executes the exact same containerized workflow that CI executes. No more
//     "works on my machine / fails in CI" divergence.
//   - **Centralised tool versions.** Terraform, TFLint, terraform-docs, and
//     Checkov versions live here as defaults. Renovate can bump a single repo;
//     all downstream consumers pick up the new pin with zero churn.
//
// Module invariants:
//
//   - The module NEVER touches the host filesystem beyond what is explicitly
//     passed in as a *Directory argument. No ambient env, no AWS creds here —
//     this module is CI-only. AWS/apply logic belongs in `dagger-terragrunt`.
//   - All functions accept a `src *Directory` as the first arg. Consumers pass
//     `--src=.` to use the current working directory.
//   - Version pins are exposed as function arguments with `+default` so they
//     can be overridden ad-hoc while keeping a single source of truth.
package main

import (
	"context"
	"fmt"

	"dagger/dagger-ci/internal/dagger"
)

// Default tool versions. Centralised here so Renovate sees a single place to
// propose bumps. Downstream callers can still override via function args.
const (
	defaultTerraformVersion    = "1.9.8"
	defaultTFLintVersion       = "v0.60.0"
	defaultTerraformDocsVersion = "v0.20.0"
	defaultCheckovVersion      = "3.2.490"
	defaultPythonVersion       = "3.12"
)

// DaggerCi is the module's root object. All exported methods are callable as
// `dagger call <method-name>` from the CLI.
type DaggerCi struct{}

// TerraformVerify runs the "Terraform verify" CI job: fmt check, init with
// backend disabled (modules never need real state), validate, tflint --init,
// and tflint itself. Returns the combined stdout so CI logs show the full
// sequence in order.
//
// Fails fast on the first non-zero exit — same semantics as the GitHub
// Actions job.
func (m *DaggerCi) TerraformVerify(
	ctx context.Context,
	// Module source directory (typically `.` in CI, checked out repo root).
	src *dagger.Directory,
	// Terraform version, e.g. "1.9.8". Omit to use the module default.
	// +optional
	// +default="1.9.8"
	tfVersion string,
	// TFLint version, e.g. "v0.60.0". Omit to use the module default.
	// +optional
	// +default="v0.60.0"
	tflintVersion string,
) (string, error) {
	if tfVersion == "" {
		tfVersion = defaultTerraformVersion
	}
	if tflintVersion == "" {
		tflintVersion = defaultTFLintVersion
	}

	return m.terraformBase(src, tfVersion, tflintVersion).
		WithExec([]string{"sh", "-c", "set -eux; " +
			"terraform fmt -check -recursive -diff && " +
			"terraform init -backend=false -input=false && " +
			"terraform validate -no-color && " +
			"tflint --init && " +
			"tflint --format=compact"}).
		Stdout(ctx)
}

// PreCommit runs `pre-commit run --all-files` inside a container with
// terraform, tflint, terraform-docs, and python available. Matches the
// pre-commit GHA job 1:1.
//
// Requires the module source to contain a `.pre-commit-config.yaml`. If the
// file is missing the pre-commit CLI will error, which is the correct failure
// mode — silent no-op would be worse.
func (m *DaggerCi) PreCommit(
	ctx context.Context,
	// Module source directory.
	src *dagger.Directory,
	// +optional
	// +default="1.9.8"
	tfVersion string,
	// +optional
	// +default="v0.60.0"
	tflintVersion string,
	// +optional
	// +default="v0.20.0"
	terraformDocsVersion string,
) (string, error) {
	if tfVersion == "" {
		tfVersion = defaultTerraformVersion
	}
	if tflintVersion == "" {
		tflintVersion = defaultTFLintVersion
	}
	if terraformDocsVersion == "" {
		terraformDocsVersion = defaultTerraformDocsVersion
	}

	// pre-commit needs a `.git` directory to resolve file lists. We handle two
	// callers:
	//
	//   1. CI via actions/checkout@v5 — `.git` is already present, working tree
	//      is clean. `git init` is a no-op, `git add -A` stages nothing, and
	//      `git commit` would exit 1 with "nothing to commit" unless we pass
	//      `--allow-empty`. So we use `--allow-empty` to keep the invariant
	//      "HEAD exists after this block" regardless of caller state.
	//
	//   2. Local `dagger call pre-commit --src=.` against a scratch dir with no
	//      git history — `git init` creates the repo, `git add -A` stages the
	//      files, and `git commit` creates the initial commit (non-empty, so
	//      `--allow-empty` is a harmless pass-through).
	//
	// The snapshot commit ensures pre-commit's "changed files" logic sees the
	// full working tree as under-test; `--all-files` then covers everything.
	return m.preCommitBase(src, tfVersion, tflintVersion, terraformDocsVersion).
		WithExec([]string{"sh", "-c", "set -eux; " +
			"git init -q -b main && " +
			"git add -A && " +
			"git -c user.email=ci@flo -c user.name=ci commit -q --allow-empty -m 'ci snapshot' && " +
			"pre-commit run --all-files --show-diff-on-failure"}).
		Stdout(ctx)
}

// Checkov runs Checkov in terraform-framework mode against the given source
// and returns the SARIF output as a *File. CLI output is always printed to
// stdout of the returned container (useful for CI logs); the SARIF file is
// intended for upload to GitHub's code-scanning API (see the `checkov` job
// in module-ci.yml).
//
// softFail controls whether findings cause the container to exit non-zero.
// The default (`true`) matches the current CI behaviour — findings are
// surfaced via SARIF, and the CI job itself only fails on infrastructure
// problems (network, bad config file, etc.), not on policy violations. This
// is intentional: Checkov's coverage includes many checks that modules
// explicitly opt out of with inline `checkov:skip` comments, and we don't
// want a missed skip to block merges.
func (m *DaggerCi) Checkov(
	ctx context.Context,
	// Module source directory. Must contain a `.checkov.yml` config.
	src *dagger.Directory,
	// Checkov version, e.g. "3.2.490".
	// +optional
	// +default="3.2.490"
	version string,
	// Return exit code 0 even if findings exist. Default matches CI.
	// +optional
	// +default=true
	softFail bool,
) (*dagger.File, error) {
	if version == "" {
		version = defaultCheckovVersion
	}
	softFailFlag := ""
	if softFail {
		softFailFlag = "--soft-fail"
	}

	container := dag.Container().
		From("python:"+defaultPythonVersion+"-slim").
		WithExec([]string{"pip", "install", "--no-cache-dir", "checkov==" + version}).
		WithMountedDirectory("/src", src).
		WithWorkdir("/src").
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			"checkov --directory . --framework terraform "+
				"--config-file .checkov.yml "+
				"--output cli --output sarif --output-file-path . %s",
			softFailFlag,
		)})

	// Print CLI output first (surface-level logs in CI).
	if _, err := container.Stdout(ctx); err != nil {
		return nil, fmt.Errorf("checkov: %w", err)
	}

	// Checkov writes results_sarif.sarif into the working directory. We return
	// that file so CI can upload it to the code-scanning API.
	return container.File("/src/results_sarif.sarif"), nil
}

// All runs TerraformVerify, PreCommit, and Checkov in sequence against the
// given source. Returns combined stdout. Intended for local invocation:
//
//	dagger call all --src=.
//
// In CI prefer the individual functions so logs attribute failures cleanly to
// the offending job.
func (m *DaggerCi) All(
	ctx context.Context,
	src *dagger.Directory,
) (string, error) {
	out := ""

	s, err := m.TerraformVerify(ctx, src, "", "")
	out += "=== TerraformVerify ===\n" + s + "\n"
	if err != nil {
		return out, fmt.Errorf("terraform-verify: %w", err)
	}

	s, err = m.PreCommit(ctx, src, "", "", "")
	out += "=== PreCommit ===\n" + s + "\n"
	if err != nil {
		return out, fmt.Errorf("pre-commit: %w", err)
	}

	if _, err := m.Checkov(ctx, src, "", true); err != nil {
		return out, fmt.Errorf("checkov: %w", err)
	}
	out += "=== Checkov ===\n(SARIF returned; soft-fail mode)\n"

	return out, nil
}

// ---------------------------------------------------------------------------
// Internal helpers — not exposed as Dagger functions.
// ---------------------------------------------------------------------------

// terraformBase returns a container with terraform + tflint + git installed
// and the source directory mounted at /src (workdir).
//
// Base image is debian:stable-slim rather than alpine because HashiCorp's
// terraform binaries are glibc-linked. Alpine would require their musl
// variant or a muscl wrapper — not worth the friction.
func (m *DaggerCi) terraformBase(
	src *dagger.Directory,
	tfVersion, tflintVersion string,
) *dagger.Container {
	return dag.Container().
		From("debian:stable-slim").
		WithExec([]string{"sh", "-c", "set -eux; " +
			"apt-get update && apt-get install -y --no-install-recommends " +
			"ca-certificates curl unzip git && rm -rf /var/lib/apt/lists/*"}).
		// Terraform: pin via explicit download so the container is self-contained.
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			"set -eux; curl -fsSLo /tmp/tf.zip "+
				"https://releases.hashicorp.com/terraform/%[1]s/terraform_%[1]s_linux_amd64.zip && "+
				"unzip -q /tmp/tf.zip -d /usr/local/bin && "+
				"rm /tmp/tf.zip && terraform version",
			tfVersion,
		)}).
		// TFLint: official install script installs to /usr/local/bin.
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			"set -eux; curl -fsSL "+
				"https://raw.githubusercontent.com/terraform-linters/tflint/master/install_linux.sh | "+
				"TFLINT_VERSION=%s bash && tflint --version",
			tflintVersion,
		)}).
		WithMountedDirectory("/src", src).
		WithWorkdir("/src")
}

// preCommitBase extends terraformBase with python, pip, pre-commit, and
// terraform-docs. Used by the PreCommit function.
func (m *DaggerCi) preCommitBase(
	src *dagger.Directory,
	tfVersion, tflintVersion, terraformDocsVersion string,
) *dagger.Container {
	return m.terraformBase(src, tfVersion, tflintVersion).
		WithExec([]string{"sh", "-c", "set -eux; " +
			"apt-get update && apt-get install -y --no-install-recommends " +
			"python3 python3-pip python3-venv && " +
			"rm -rf /var/lib/apt/lists/*"}).
		WithExec([]string{"sh", "-c",
			"python3 -m pip install --break-system-packages --no-cache-dir pre-commit"}).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			"set -eux; curl -fsSLo /tmp/td.tgz "+
				"https://terraform-docs.io/dl/%[1]s/terraform-docs-%[1]s-linux-amd64.tar.gz && "+
				"tar -xzf /tmp/td.tgz -C /tmp && "+
				"install /tmp/terraform-docs /usr/local/bin/terraform-docs && "+
				"rm /tmp/td.tgz /tmp/terraform-docs && terraform-docs --version",
			terraformDocsVersion,
		)})
}
