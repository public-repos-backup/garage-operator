# Contributing to Garage Operator

Thank you for helping improve Garage Operator. Changes here can affect
stateful workloads, persistent volumes, cluster layout, and distributed data,
so the project values reproducible evidence, explicit safety decisions, and
tests that exercise the real deployment path.

## Choose the workflow before writing code

| Change | Start with | Design record |
| --- | --- | --- |
| Typo, documentation clarification, or small non-behavioral CI cleanup | A focused pull request is enough | Not needed |
| Focused bug fix or user-visible behavior change | Use an existing issue or open one with a reproduction, expected behavior, and actual behavior | Needed when the fix crosses one of the risk boundaries below |
| New API or feature, architectural change, migration, or data-safety change | Open an issue and agree on the direction before substantial implementation | Required |

A design review is required when a change affects any of the following:

- CRD shape, defaulting, validation, served versions, or conversion behavior;
- ownership or naming of StatefulSets, PVCs, Services, Secrets, or other
  long-lived resources;
- finalizers, deletion, retention, adoption, or migration behavior;
- Garage layout, federation, replication-factor changes, or recovery paths;
- controller boundaries, permissions, or a broad cross-component workflow.

When in doubt, open an issue with the smallest useful reproduction and proposed
user experience. A maintainer can then confirm whether a design record is
needed. Do not post credentials, private endpoints, or unsanitized production
logs.

### Design records

Place project design records in:

```text
docs/design/YYYY-MM-DD-<short-topic>-design.md
```

This is a tool-neutral project directory. It is available to every contributor
and is not reserved for maintainers or for any particular coding assistant.

A useful design record covers:

- the problem and evidence for the current behavior;
- constraints and relevant upstream Garage semantics;
- the proposed API and reconciliation behavior;
- alternatives considered and why they were not selected;
- compatibility, migration, rollback, and data-safety implications;
- failure modes, observability, and a test plan;
- documentation work and intentionally deferred follow-ups.

The design may be reviewed in its own pull request or at the start of a draft
implementation pull request. In either case, agree on the design before
building a large implementation around it.

## Set up a development environment

Fork the repository, clone your fork, and branch from the latest `main`:

```bash
git clone https://github.com/<your-user>/garage-operator.git
cd garage-operator
git remote add upstream https://github.com/rajsinghtech/garage-operator.git
git fetch upstream
git switch -c fix/short-description upstream/main
```

The required Go version is declared in `go.mod`. Run `make help` for the
complete target list. Common local-development targets are:

```bash
make dev-up
make dev-test
make dev-status
make dev-logs
make dev-load
make dev-down
```

These targets use Docker, Kind, kubectl, and Helm. Build and test helper
binaries are installed under `bin/` by the Makefile when possible. You can also
install the optional linting pre-commit hook with `make install-hooks`.

## Make a focused, complete change

- Keep a pull request centered on one problem. Include complementary changes
  that address the same root cause or prevent the same failure mode, but leave
  unrelated refactors and features for separate work.
- Reproduce bugs before changing behavior. When operator behavior depends on
  Garage internals or Admin API semantics, verify it against the
  [upstream Garage source and documentation](https://git.deuxfleurs.fr/Deuxfleurs/garage)
  rather than relying on assumptions.
- Preserve compatibility. `GarageCluster` uses `v1beta2` as its current storage
  version while `v1beta1` remains served. API changes must cover defaulting,
  validation, conversion round trips, CRDs, schemas, samples, and upgrades as
  applicable.
- Treat storage ownership, node identity, layout, finalizers, and destructive
  annotations as data-safety boundaries. Document upgrade and rollback
  behavior whenever one changes.
- Add the narrowest useful unit or envtest regression near the affected code,
  then add end-to-end coverage for behavior that only appears in a real
  cluster.
- Update user-facing documentation and samples in the same pull request as a
  user-facing change.

### Generated files

Do not hand-edit generated deepcopy code, CRDs, Helm CRD copies, or JSON
schemas. After changing API types or kubebuilder markers, run:

```bash
make manifests generate
```

Commit all resulting generated changes. CI runs `make verify-generate` from a
clean checkout and fails if generated artifacts do not match the Go source.

## Test the change

Use the checks that match the affected surface:

| Changed surface | Expected local validation |
| --- | --- |
| Go, webhooks, or controllers | `make test` and `make lint` |
| API types or kubebuilder markers | `make manifests generate`, `make test`, and `make lint` |
| Helm templates or chart behavior | `make helm-lint` and `make helm-template` |
| CR samples or schemas | `make validate-manifests` |
| Reconciliation or user-visible cluster behavior | The most focused applicable end-to-end target |
| Documentation only | Render the Markdown and verify every changed link and command |

`make test` runs generation, formatting, vetting, envtest-backed tests, and the
non-e2e Go test suite. Inspect `git diff` afterward so formatting or generation
changes are not missed.

For ordinary operator behavior, add a labeled Ginkgo scenario under
`test/e2e/` and run the narrowest useful label:

```bash
make test-e2e GINKGO_LABEL_FILTER='management-handle'
```

The scripts under `hack/e2e-*.sh` are appropriate when a scenario needs a
special topology or external component that does not fit the shared Ginkgo
environment. Existing entry points include:

```bash
make test-e2e-cluster
make test-e2e-cosi
make test-e2e-multicluster
make test-e2e-ipv6
make test-e2e-external-gateway
```

Prefer `test/e2e/` for normal controller and API coverage. If a new standalone
script is necessary, expose it through a Make target and a CI job so it remains
part of the supported test suite.

Run every relevant check you reasonably can before opening the pull request.
In the pull request, list the exact commands run and call out anything not run,
with the reason. All required GitHub Actions checks must pass before merge.

## Commits and pull requests

Use concise Conventional Commit subjects for commits and pull request titles:

```text
feat(cluster): add management handle readiness
fix(storage): preserve PVC labels during migration
test(e2e): cover external gateway reconnection
docs: explain the contribution workflow
ci: verify generated CRDs stay in sync
```

A pull request should:

- link its issue with `Fixes #<number>` when it fully resolves that issue;
- explain the root cause or design rationale, not only the file changes;
- describe compatibility, migration, operational, and data-safety impact;
- list validation commands and relevant runtime evidence;
- include generated artifacts, docs, and samples required by the change;
- remain open for review and all required workflows before merge.

Keep the branch current with `main` while addressing review feedback. Maintainers
normally use the pull request title as the squash-merge subject, so keep it
accurate and in Conventional Commit form.

## Releases and version fields

Regular feature, fix, documentation, and dependency pull requests must not bump
release versions. In particular, leave these fields unchanged unless a
maintainer explicitly asks for a release change:

- `charts/garage-operator/Chart.yaml` `version`;
- `charts/garage-operator/Chart.yaml` `appVersion`;
- `charts/garage-operator/values.yaml` `image.tag`.

Functional changes elsewhere in the chart are normal pull request content.
After a change is merged and the `main` workflows pass, a maintainer cuts a
release from a clean `main` checkout with:

```bash
make release VERSION=vX.Y.Z
```

That target updates the three version fields together, creates the release
commit and tag, and pushes them. Tag workflows verify the versions and publish
the image, Helm chart, install manifest, and GitHub release.

## AI-assisted contributions

AI-assisted work is welcome, and no particular assistant is required. The
contributor remains responsible for every submitted line and claim:

- review generated changes rather than accepting them blindly;
- validate behavior against source, documentation, tests, or a reproduction;
- remove secrets, private environment details, and local session artifacts;
- report only tests and commands that actually ran;
- follow this public workflow regardless of tool-specific instructions.

## DCO, CLA, and license

The project currently requires neither a Contributor License Agreement nor a
Developer Certificate of Origin sign-off. Signed commits and `Signed-off-by`
trailers are optional.

By submitting a contribution, you agree that it is your original work, or work
you have the right to submit, and that it is licensed under the repository's
[Apache License 2.0](LICENSE).

## Getting help

Use a GitHub issue for contribution questions, design discussion, and
reproducible bug reports. A small draft pull request is also useful when code or
test output communicates the question more clearly than prose.
