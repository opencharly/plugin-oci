# plugin-oci

The OCI image engine for OpenCharly — layer merging and remote-image adoption,
externalized from charly core as the `oci` verb.

The plugin owns the `go-containerregistry` stack: the layer-MERGE engine (with
full whiteout handling, plus the podman/skopeo daemon save/load) and the
remote-image adopt-user PROBE (`/etc/passwd` lookup at a configured uid). Both
run host-side and exec `podman`/`skopeo` themselves, so `go-containerregistry`
lives here and charly's core `go.mod` links it nowhere.

## What it provides

| Capability | Surface |
|---|---|
| `verb:oci` | the internal OCI RPC verb — ops `merge`, `inspect-user`, `cache-push`, `cache-pull` |

`verb:oci` is a pure **internal** RPC verb — never authored as an `oci:` check
step. It is keyed by an `OciOp` discriminator:

- `oci_op=merge` — decodes a `spec.MergeRequest`, returns a `spec.MergeReply`
  (layer counts + progress notes; a per-merge failure rides `Reply.Error`).
- `oci_op=inspect-user` — decodes a `spec.ImageUserInput`, returns a
  `spec.UserInfo`.
- `oci_op=cache-push` / `oci_op=cache-pull` — move a whole named
  `spec/cache.ArtifactStore` (already a standard OCI Image Layout) to and from an
  OCI registry, the transport for the OCI-manifest-native cache.

It is **dual-placement**: its importable provider package is compiled into charly
when listed in `compiled_plugins:` (the default — the merge and adopt-user probes
sit on the core build path), and the SAME provider is served out-of-process over
go-plugin gRPC by the `cmd/serve` shim when it is not.

## How it is reached

`charly box merge` (in `candy/plugin-box`) and `candy/plugin-build` reach it
directly via `Executor.InvokeProvider(verb:oci)` — the peer-dispatch leg. The one
remaining core consumer is `generate.go`'s adopt-user resolution, reached through
the `oci_plugin.go` shim.

## Layout

- `candy/plugin-oci/` — the plugin module: `plugin.go` (provider + meta),
  `merge.go` (the layer-merge engine), `inspect_user.go`, `cache_transport.go`,
  `schema/oci.cue` (the self-contained `#OciPlugin`), and `cmd/serve/main.go`.
- `charly.yml` — the root project manifest (`discover: candy`).
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.

## Related

- Owning skill: `/charly-build:merge` — `charly box merge` and layer reduction;
  `/charly-internals:plugin` for the provider model (the candy carries no
  `skill:` entity of its own; the gap is tracked in
  [opencharly/opencharly#291](https://github.com/opencharly/opencharly/issues/291)).
- [`opencharly/charly`](https://github.com/opencharly/charly) — the charly CLI.
