# AGENTS.md — plugin-oci

Standalone plugin repo owning the OCI image engine (`verb:oci`) — layer merging,
remote-image adoption, and the OCI-manifest cache transport. The plugin is a Go
module at `candy/plugin-oci/` (module path
`github.com/opencharly/plugin-oci/candy/plugin-oci`); the root `charly.yml` only
declares `discover: candy` so the repo is a project and its candy is scanned.

Canonical files:

- `candy/plugin-oci/charly.yml` — the `plugin-oci:` candy entity (`plugin:`
  block, `plan:` checks).
- `candy/plugin-oci/` — the Go source: `plugin.go`, `merge.go` (the layer-merge
  engine), `inspect_user.go`, `cache_transport.go`, `schema/oci.cue`, and
  `cmd/serve/main.go`.
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.
- `README.md` — user overview only; never agent guidance.

## Load these skills first (R0)

- `/charly-internals:plugin` — the plugin authoring reference: the `plugin:`
  block, the unified Provider model, dual placement, the per-plugin CUE-schema
  contract.
- `/charly-build:merge` — `charly box merge` and layer reduction, the primary
  consumer of the merge engine.
- `/charly-internals:capabilities` — the OCI label contract the adopt-user probe
  and cache transport operate on.
- `/charly-internals:git-workflow` — before any git/PR action.

## Build / validate / test

- `go build ./...` in `candy/plugin-oci/` — compile the plugin module.
- `go test ./...` in `candy/plugin-oci/` — the plugin's Go tests
  (`TestMergeEngineGoldenParity` locks the byte-identical merge relocation;
  `TestCacheTransportDeterministic` locks the lossless layout round-trip;
  `TestCachePushPullLiveRoundTrip` is `LIVE_REGISTRY`-gated and skips cleanly
  when the credential is unset).
- `charly box validate` at the repo root — the structural check (the candy +
  `plugin:` block, CUE schema).
- The merge gate is the **org-wide** `charly/pr-validator` (required check
  `validate / validate`, defined in `opencharly/.github`); this repo has **no**
  per-repo candy gate.

## Modify this repo

- Edit the `plugin-oci:` candy entity, the Go source, and `schema/oci.cue`
  **together** — the schema is the served declaration surface.
- Keep the merge engine byte-identical to the former core relocation — the
  golden DiffID test (`TestMergeEngineGoldenParity`) is the guard.
- Keep `verb:oci` internal: it is never authored as an `oci:` check step and
  ships its own self-contained schema.

## Landing

Load `/charly-internals:git-workflow` before any git/PR action; it owns the
landing mechanics. The authoritative rulebook is the umbrella `AGENTS.md` in
`opencharly/opencharly` and `charly/AGENTS.md` in the charly repo.
