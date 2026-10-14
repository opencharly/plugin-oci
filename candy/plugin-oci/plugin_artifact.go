// plugin_artifact.go — the verb:oci PLUGIN-ARTIFACT transport: push a plugin
// artifact (provider binary + candy charly.yml + .providers word manifest, under a
// multi-platform index) to a registry, and pull one back. The sibling of
// cache_transport.go, over the SAME proven bridge: push = layout.ImageIndex() →
// remote.WriteIndex; pull = remote.Index → layout.Write. No bespoke registry code.
//
// Where the artifact FORMAT lives: NOT here. The layout's blobs, manifest, index and
// oci-layout marker are assembled and read by the shared sdk mechanism, so the emitter
// (a plugin repo's release CI) and the consumer (charly's plugin resolution) agree by
// construction. This file MOVES a layout and VERIFIES what it moves; it never invents
// the bytes. What it does own is the registry-side identity: which media types make a
// layout a plugin artifact, which platform descriptor answers a request, and what a
// caller is told when nothing answers it.
//
// The transport is REF-AGNOSTIC: `ref` is any distribution-spec reference, so it also
// addresses a registry charly itself may serve. Nothing here assumes who runs it.
package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	pb "github.com/opencharly/spec/proto"
	"github.com/opencharly/spec/spec"
)

// pluginPushLeg is oci_op=plugin-push: read the plugin-artifact OCI layout at req.Dir,
// verify it IS a plugin artifact (a mislabelled or half-built layout must not reach a
// registry where a consumer would take it for one), select the platform(s) it carries,
// and push its index — `remote.WriteIndex` alone, which the RDD spike proved is
// sufficient (no per-manifest remote.Write; the tag rides the index descriptor's
// org.opencontainers.image.ref.name).
func pluginPushLeg(paramsJSON []byte) (*pb.InvokeReply, error) {
	var req spec.PluginArtifactTransferRequest
	if len(paramsJSON) > 0 {
		if err := json.Unmarshal(paramsJSON, &req); err != nil {
			return nil, fmt.Errorf("oci plugin-push: decode request: %w", err)
		}
	}
	reply, err := runPluginPush(req)
	if err != nil {
		return nil, fmt.Errorf("oci plugin-push: %w", err)
	}
	j, err := json.Marshal(reply)
	if err != nil {
		return nil, fmt.Errorf("oci plugin-push: encode reply: %w", err)
	}
	return &pb.InvokeReply{ResultJson: j}, nil
}

// pluginPullLeg is oci_op=plugin-pull: fetch the artifact index and write it into the
// local OCI layout directory (creating it), then verify and SELECT the platform this
// caller asked for. A pull that cannot answer the request is an ERROR naming what the
// artifact does carry — never a fall-through to whichever platform happens to be there.
func pluginPullLeg(paramsJSON []byte) (*pb.InvokeReply, error) {
	var req spec.PluginArtifactTransferRequest
	if len(paramsJSON) > 0 {
		if err := json.Unmarshal(paramsJSON, &req); err != nil {
			return nil, fmt.Errorf("oci plugin-pull: decode request: %w", err)
		}
	}
	reply, err := runPluginPull(req)
	if err != nil {
		return nil, fmt.Errorf("oci plugin-pull: %w", err)
	}
	j, err := json.Marshal(reply)
	if err != nil {
		return nil, fmt.Errorf("oci plugin-pull: encode reply: %w", err)
	}
	return &pb.InvokeReply{ResultJson: j}, nil
}

// runPluginPush verifies the layout at req.Dir and pushes its index to req.Ref. The
// tests drive the LEGS against an in-memory registry (default suite) or a real one
// (LIVE_REGISTRY) — the registry is reached through req.Ref, so no injection seam exists.
func runPluginPush(req spec.PluginArtifactTransferRequest) (spec.PluginArtifactTransferReply, error) {
	lp, err := layout.FromPath(req.Dir)
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("open layout %s: %w", req.Dir, err)
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("read layout index: %w", err)
	}
	im, err := ii.IndexManifest()
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("read index manifest: %w", err)
	}
	// The caller may ship a SUBSET (one platform of a multi-platform artifact): a push
	// request naming a platform asserts that platform is present and well formed, and
	// refuses to ship an index that does not carry it.
	selected, platforms, err := selectPlatform(im, req.Platform)
	if err != nil {
		return spec.PluginArtifactTransferReply{}, err
	}
	if err := validatePluginArtifact(lp, ii, selected); err != nil {
		return spec.PluginArtifactTransferReply{}, err
	}

	ref, err := name.ParseReference(req.Ref, parseOpts(req.Insecure)...)
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("parse ref %q: %w", req.Ref, err)
	}
	if err := remote.WriteIndex(ref, ii, remoteOpts()...); err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("push to %s: %w", ref, err)
	}
	reply := spec.PluginArtifactTransferReply{
		Ref:       ref.String(),
		Platform:  platformString(selected.Platform),
		Platforms: platforms,
	}
	if dgst, derr := ii.Digest(); derr == nil {
		reply.Digest = dgst.String()
	}
	return reply, nil
}

// runPluginPull fetches req.Ref's index into req.Dir, then verifies the layout it just
// wrote and selects the requested platform.
//
// It moves the WHOLE index. The RDD spike proved that path byte-identical
// (remote.Index → layout.Write preserved every raw blob); transferring only the
// selected platform's manifest and blobs is a measured question (a multi-platform
// artifact whose entries are ~30 MB each makes the difference worth knowing), and it is
// deliberately not guessed at here.
func runPluginPull(req spec.PluginArtifactTransferRequest) (spec.PluginArtifactTransferReply, error) {
	ref, err := name.ParseReference(req.Ref, parseOpts(req.Insecure)...)
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("parse ref %q: %w", req.Ref, err)
	}
	ii, err := remote.Index(ref, remoteOpts()...)
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("pull %s: %w", ref, err)
	}
	if err := os.MkdirAll(filepath.Dir(req.Dir), 0o755); err != nil {
		return spec.PluginArtifactTransferReply{}, err
	}
	if _, err := layout.Write(req.Dir, ii); err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("write layout %s: %w", req.Dir, err)
	}

	// Verify what LANDED, not what was fetched: the local layout is the thing the
	// consumer will read, and it is also where a truncated or tampered transfer shows up.
	lp, err := layout.FromPath(req.Dir)
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("reopen layout %s: %w", req.Dir, err)
	}
	local, err := lp.ImageIndex()
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("read pulled index: %w", err)
	}
	im, err := local.IndexManifest()
	if err != nil {
		return spec.PluginArtifactTransferReply{}, fmt.Errorf("read pulled index manifest: %w", err)
	}
	selected, platforms, err := selectPlatform(im, req.Platform)
	if err != nil {
		return spec.PluginArtifactTransferReply{}, err
	}
	if err := validatePluginArtifact(lp, local, selected); err != nil {
		return spec.PluginArtifactTransferReply{}, err
	}
	if dgst, derr := local.Digest(); derr == nil {
		return spec.PluginArtifactTransferReply{
			Ref:       ref.String(),
			Digest:    dgst.String(),
			Platform:  platformString(selected.Platform),
			Platforms: platforms,
		}, nil
	}
	return spec.PluginArtifactTransferReply{
		Ref:       ref.String(),
		Platform:  platformString(selected.Platform),
		Platforms: platforms,
	}, nil
}

// validatePluginArtifact asserts the layout IS a plugin artifact before it is pushed or
// served: the index's artifactType, the selected manifest's artifactType, its config media
// type, and the three LAYER ROLES the format requires (binary, candy manifest,
// .providers). One implementation for both legs (R2): a producer and a consumer
// disagreeing about "what is a valid plugin artifact" is exactly the drift that would ship
// a broken artifact from a green emitter.
//
// It reads the RAW manifest JSON of both the index and the selected manifest rather than
// the typed v1 structs: go-containerregistry v0.20.7's v1.Manifest / v1.IndexManifest carry
// no artifactType field (artifact support there is manifest-agnostic), so a typed read
// would silently see "" and accept anything. Reading the bytes is also what makes the check
// independent of the library's own model.
func validatePluginArtifact(lp layout.Path, ii v1.ImageIndex, selected v1.Descriptor) error {
	rawIndex, err := ii.RawManifest()
	if err != nil {
		return fmt.Errorf("read index bytes: %w", err)
	}
	var index struct {
		ArtifactType string `json:"artifactType"`
	}
	if err := json.Unmarshal(rawIndex, &index); err != nil {
		return fmt.Errorf("parse index bytes: %w", err)
	}
	if index.ArtifactType != spec.ArtifactTypePlugin {
		return fmt.Errorf("index artifactType %q is not %q", index.ArtifactType, spec.ArtifactTypePlugin)
	}

	img, err := lp.Image(selected.Digest)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", selected.Digest, err)
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		return fmt.Errorf("read manifest bytes %s: %w", selected.Digest, err)
	}
	var mf struct {
		ArtifactType string `json:"artifactType"`
		Config       struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
		Layers []pluginLayer `json:"layers"`
	}
	if err := json.Unmarshal(rawManifest, &mf); err != nil {
		return fmt.Errorf("parse manifest bytes %s: %w", selected.Digest, err)
	}
	if mf.ArtifactType != spec.ArtifactTypePlugin {
		return fmt.Errorf("manifest %s artifactType %q is not %q", selected.Digest, mf.ArtifactType, spec.ArtifactTypePlugin)
	}
	if mf.Config.MediaType != spec.MediaTypePluginConfig {
		return fmt.Errorf("manifest %s config media type %q is not %q", selected.Digest, mf.Config.MediaType, spec.MediaTypePluginConfig)
	}
	// The three layer ROLES, by the media type that identifies each. A missing role is a
	// HARD error: a consumer cannot materialize `<name>` + `<name>.providers` + the candy
	// manifest from an artifact that does not carry all three.
	for _, want := range []string{
		spec.MediaTypePluginBinary,
		spec.MediaTypePluginManifest,
		spec.MediaTypePluginProviders,
	} {
		if !hasLayerMediaType(mf.Layers, want) {
			return fmt.Errorf("manifest %s carries no %s layer (layers: %s)", selected.Digest, want, layerMediaTypes(mf.Layers))
		}
	}
	return nil
}

// pluginLayer is the layer subset validation compares: media type only.
type pluginLayer struct {
	MediaType string `json:"mediaType"`
}

// hasLayerMediaType reports whether any layer descriptor carries mediaType.
func hasLayerMediaType(layers []pluginLayer, mediaType string) bool {
	for _, l := range layers {
		if l.MediaType == mediaType {
			return true
		}
	}
	return false
}

// layerMediaTypes renders a manifest's layer media types for an error message, so a
// refusal names what IS there instead of only what is missing.
func layerMediaTypes(layers []pluginLayer) string {
	out := make([]string, 0, len(layers))
	for _, l := range layers {
		out = append(out, l.MediaType)
	}
	if len(out) == 0 {
		return "(none)"
	}
	return strings.Join(out, ", ")
}

// selectPlatform finds the descriptor answering `want` (an `os/arch[/variant]` string, or
// "" for THIS runtime's platform) and returns it, plus every platform the index carries
// so a refusal can name the alternatives.
//
// image-spec: "If multiple manifests match a client or runtime's requirements, the first
// matching entry SHOULD be used." Variant awareness is deliberately conservative: a
// descriptor that declares a `variant` is selected by an unqualified host ONLY when it is
// the sole match, and a host that declares its own variant must match it exactly — a
// v3-built binary on a baseline CPU is a SIGILL, and a silent pick is worse than a
// refusal.
func selectPlatform(im *v1.IndexManifest, want string) (v1.Descriptor, []string, error) {
	if want == "" {
		want = hostPlatform()
	}
	platforms := make([]string, 0, len(im.Manifests))
	for _, d := range im.Manifests {
		platforms = append(platforms, platformString(d.Platform))
	}
	if len(im.Manifests) == 0 {
		return v1.Descriptor{}, nil, fmt.Errorf("artifact index carries no manifests (want %s)", want)
	}

	matches := make([]v1.Descriptor, 0, len(im.Manifests))
	for _, d := range im.Manifests {
		if descriptorMatches(d.Platform, want) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return v1.Descriptor{}, platforms, fmt.Errorf("artifact carries no %s manifest (carries: %s)", want, strings.Join(platforms, ", "))
	case 1:
		return matches[0], platforms, nil
	}
	// Several os/arch matches: prefer an EXACT variant match, then the baseline entry
	// (no declared variant), then the spec's first-match rule.
	if v := variantOf(want); v != "" {
		for _, d := range matches {
			if d.Platform != nil && d.Platform.Variant == v {
				return d, platforms, nil
			}
		}
	}
	for _, d := range matches {
		if d.Platform != nil && d.Platform.Variant == "" {
			return d, platforms, nil
		}
	}
	return matches[0], platforms, nil
}

// descriptorMatches reports whether a descriptor's platform satisfies `want`
// (`os/arch[/variant]`). `os` and `architecture` must match exactly; a declared variant
// must be satisfied, never ignored.
func descriptorMatches(p *v1.Platform, want string) bool {
	if p == nil {
		return false
	}
	parts := strings.Split(want, "/")
	if len(parts) < 2 {
		return false
	}
	if p.OS != parts[0] || p.Architecture != parts[1] {
		return false
	}
	if len(parts) == 2 {
		// The caller named no variant. Its own variant, when it has one, is still
		// authoritative: an entry built for v3 must not answer a v1 CPU.
		host := variantOf(hostPlatform())
		return p.Variant == "" || p.Variant == host
	}
	// An explicit request accepts its own variant and any BASELINE entry — a
	// lower-level binary runs on a higher-level CPU — never a HIGHER level than the
	// one asked for, which is the direction that faults.
	return p.Variant == parts[2] || p.Variant == ""
}

// platformString renders a descriptor's platform as `os/arch[/variant]`, and "unknown"
// for a descriptor without one (a platform-less descriptor is legal in an index, but it
// can never answer a platform request).
func platformString(p *v1.Platform) string {
	if p == nil {
		return "unknown"
	}
	if p.Variant == "" {
		return p.OS + "/" + p.Architecture
	}
	return p.OS + "/" + p.Architecture + "/" + p.Variant
}

// variantOf extracts the variant from an `os/arch[/variant]` string ("" when absent).
func variantOf(platform string) string {
	parts := strings.Split(platform, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// hostPlatform is THIS runtime's platform in the image-spec `platform` vocabulary:
// GOOS/GOARCH, plus the micro-architecture level the Go toolchain selected (GOAMD64 v1–v4,
// GOARM64 v8.x) when the environment declares one. The env vars are the Go convention for
// exactly this fact, and they are also what an index's `variant` names.
func hostPlatform() string {
	p := runtime.GOOS + "/" + runtime.GOARCH
	switch runtime.GOARCH {
	case "amd64":
		if v := os.Getenv("GOAMD64"); v != "" {
			return p + "/" + v
		}
	case "arm64":
		if v := os.Getenv("GOARM64"); v != "" {
			return p + "/" + v
		}
	}
	return p
}
