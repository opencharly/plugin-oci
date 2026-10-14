// cache_transport.go — the verb:oci registry TRANSPORT for a named charly
// ArtifactStore: push a whole OCI-Image-Layout cache to a registry, and pull one
// back. It is the "read and write the generated cache into an OCI registry"
// half of the OCI-manifest-native cache (the headless core landed in spec#148:
// spec/cache's ArtifactStore is already a standard OCI Image Layout, so a cache
// IS an OCI image — its config blob is the entry validity, its layers the
// payloads, its index the named store).
//
// Because the on-disk form is a real OCI layout, the transport is a thin,
// lossless bridge with go-containerregistry: push = layout.ImageIndex() →
// remote.WriteIndex; pull = remote.Index → layout.Write. No bespoke format, no
// bespoke registry code — the Docker cache principle (content-addressed blobs +
// a manifest/index entry point) reused verbatim.
//
// The go-containerregistry stack stays HERE (this candy): callers reach the
// transport over verb:oci, so it is never linked into spec or core.
package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	pb "github.com/opencharly/spec/proto"
	"github.com/opencharly/spec/spec"
)

// The cache-transfer wire pair is NOT declared here: it is CUE-single-sourced in
// spec/schema/oci.cue (#CacheTransferRequest / #CacheTransferReply, spec#149) and
// decoded as spec.CacheTransferRequest / spec.CacheTransferReply. The two plugins
// on this wire — this transport and candy/plugin-cache's `charly cache push/pull`
// leaves — share ONE declaration, so neither can drift from the other (R3). The
// pair rides ParamsJson in the same envelope as merge/inspect-user (verb:oci is a
// pure internal RPC keyed by the OciOp discriminator; it ships no authored schema).

// cachePushLeg is oci_op=cache-push: read the named-cache OCI layout and push its
// index (and every referenced manifest/blob) to the registry. Lossless: the
// registry then holds a byte-identical OCI image whose index is the cache. A
// failure is a real Go error (the transport is NOT best-effort — the caller must
// see a push/pull failure, not a success-shaped reply).
func cachePushLeg(paramsJSON []byte) (*pb.InvokeReply, error) {
	var req spec.CacheTransferRequest
	if len(paramsJSON) > 0 {
		if err := json.Unmarshal(paramsJSON, &req); err != nil {
			return nil, fmt.Errorf("oci cache-push: decode request: %w", err)
		}
	}
	reply, err := runCachePush(req)
	if err != nil {
		return nil, fmt.Errorf("oci cache-push: %w", err)
	}
	j, err := json.Marshal(reply)
	if err != nil {
		return nil, fmt.Errorf("oci cache-push: encode reply: %w", err)
	}
	return &pb.InvokeReply{ResultJson: j}, nil
}

// cachePullLeg is oci_op=cache-pull: fetch the registry image's index and write
// it into the local OCI layout directory (creating it). A pulled named cache
// reads back through the SAME spec/cache ArtifactStore with no conversion.
func cachePullLeg(paramsJSON []byte) (*pb.InvokeReply, error) {
	var req spec.CacheTransferRequest
	if len(paramsJSON) > 0 {
		if err := json.Unmarshal(paramsJSON, &req); err != nil {
			return nil, fmt.Errorf("oci cache-pull: decode request: %w", err)
		}
	}
	reply, err := runCachePull(req)
	if err != nil {
		return nil, fmt.Errorf("oci cache-pull: %w", err)
	}
	j, err := json.Marshal(reply)
	if err != nil {
		return nil, fmt.Errorf("oci cache-pull: encode reply: %w", err)
	}
	return &pb.InvokeReply{ResultJson: j}, nil
}

// runCachePush reads the layout at req.Dir and pushes it to req.Ref. The tests
// drive the real legs (cachePushLeg/cachePullLeg) against an in-memory registry
// (default suite) or a real registry (LIVE_REGISTRY) — the registry is reached
// through req.Ref, so no injection seam exists.
func runCachePush(req spec.CacheTransferRequest) (spec.CacheTransferReply, error) {
	lp, err := layout.FromPath(req.Dir)
	if err != nil {
		return spec.CacheTransferReply{}, fmt.Errorf("open layout %s: %w", req.Dir, err)
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		return spec.CacheTransferReply{}, fmt.Errorf("read layout index: %w", err)
	}
	ref, err := name.ParseReference(req.Ref, parseOpts(req.Insecure)...)
	if err != nil {
		return spec.CacheTransferReply{}, fmt.Errorf("parse ref %q: %w", req.Ref, err)
	}
	if err := remote.WriteIndex(ref, ii, remoteOpts()...); err != nil {
		return spec.CacheTransferReply{}, fmt.Errorf("push to %s: %w", ref, err)
	}
	reply := spec.CacheTransferReply{Ref: ref.String()}
	if dgst, derr := ii.Digest(); derr == nil {
		reply.Digest = dgst.String()
	}
	if im, ierr := ii.IndexManifest(); ierr == nil {
		reply.Entries = len(im.Manifests)
	}
	return reply, nil
}

// refNameAnnotation is the OCI annotation a layout descriptor carries to name the
// reference it was fetched from. It is the same annotation `remote.WriteIndex` sets
// on a pushed index descriptor, so a pulled single-image entry is addressable by the
// ref it came from rather than by position.
const refNameAnnotation = "org.opencontainers.image.ref.name"

// runCachePull fetches req.Ref and writes it into req.Dir as an OCI layout.
//
// ONE FETCH, DISPATCHED ON THE DESCRIPTOR'S MEDIA TYPE. A registry serves two shapes and
// the cache transport must carry both: an INDEX/list (what a named ArtifactStore is) and a
// SINGLE IMAGE (what a registry serves for an ordinary image tag). The earlier
// `remote.Index(ref)`-only form could not fetch the second — measured by the ingest leg
// (`plugin-cache`): `remote.Index(single-image)` fails with
//
//	unexpected media type for ImageIndex(): application/vnd.docker.distribution.manifest.v2+json; call Image() instead
//
// and it is a hard error, not a fallback, so nothing downstream could work around it.
//
// The dispatch is keyed on the MEDIA TYPE and is deliberately NOT `ImageIndex()`-then-
// fall-back-to-`Image()`: on an index ref `desc.Image()` does not fail, it silently
// resolves the index to whichever single platform it picks, so a fall-back would quietly
// turn every index pull into a one-platform image. `remote.Get` fetches the manifest once
// and its descriptor says which shape arrived; each arm then writes the layout shape it
// owns. No retry, no second client, no fall-through branch.
func runCachePull(req spec.CacheTransferRequest) (spec.CacheTransferReply, error) {
	ref, err := name.ParseReference(req.Ref, parseOpts(req.Insecure)...)
	if err != nil {
		return spec.CacheTransferReply{}, fmt.Errorf("parse ref %q: %w", req.Ref, err)
	}
	desc, err := remote.Get(ref, remoteOpts()...)
	if err != nil {
		return spec.CacheTransferReply{}, fmt.Errorf("pull %s: %w", ref, err)
	}
	if err := os.MkdirAll(filepath.Dir(req.Dir), 0o755); err != nil {
		return spec.CacheTransferReply{}, err
	}
	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		ii, ierr := desc.ImageIndex()
		if ierr != nil {
			return spec.CacheTransferReply{}, fmt.Errorf("read index %s: %w", ref, ierr)
		}
		if _, werr := layout.Write(req.Dir, ii); werr != nil {
			return spec.CacheTransferReply{}, fmt.Errorf("write layout %s: %w", req.Dir, werr)
		}
		// The reply for an index records the INDEX's own digest and its entry count —
		// the shape `charly cache status`/`push` report, unchanged by this dispatch.
		reply := spec.CacheTransferReply{Ref: ref.String()}
		if dgst, derr := ii.Digest(); derr == nil {
			reply.Digest = dgst.String()
		}
		if im, ierr := ii.IndexManifest(); ierr == nil {
			reply.Entries = len(im.Manifests)
		}
		return reply, nil
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		img, ierr := desc.Image()
		if ierr != nil {
			return spec.CacheTransferReply{}, fmt.Errorf("read image %s: %w", ref, ierr)
		}
		if werr := writeImageLayout(req.Dir, img, ref.String()); werr != nil {
			return spec.CacheTransferReply{}, fmt.Errorf("write layout %s: %w", req.Dir, werr)
		}
		reply := spec.CacheTransferReply{Ref: ref.String(), Entries: 1}
		if dgst, derr := img.Digest(); derr == nil {
			reply.Digest = dgst.String()
		}
		return reply, nil
	default:
		return spec.CacheTransferReply{}, fmt.Errorf("pull %s: unsupported manifest media type %q (want an image index/list or an image manifest)", ref, desc.MediaType)
	}
}

// writeImageLayout writes ONE pulled image as an OCI layout whose top-level index carries
// that image as its single descriptor, annotated with the ref it was fetched from.
//
// This is the shape `layout.AppendImage` owns (`charly registry cache add` used it for the
// same reason), and it is what makes the layout readable by the same index-driven reader
// on both arms: `spec/cache`'s Layout reads `index.json`'s manifests, so a layout whose
// `index.json` is a bare image MANIFEST — which is exactly what writing a single image
// without an index produces — has no index to read. Measured: the append shape yields
// `index.json` mediaType application/vnd.oci.image.index.v1+json with exactly one
// descriptor pointing at the image manifest digest, i.e. one root descriptor per layout.
func writeImageLayout(dir string, img v1.Image, ref string) error {
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		return err
	}
	return lp.AppendImage(img, layout.WithAnnotations(map[string]string{refNameAnnotation: ref}))
}

// parseOpts translates the Insecure flag to a name option: a plain-HTTP
// registry (a localhost dev registry) needs name.Insecure. The remote options
// need no equivalent — name.Insecure is what selects the scheme.
func parseOpts(insecure bool) []name.Option {
	if insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

// remoteOpts is the fixed remote client option set: context + the docker-config
// keychain (credential helpers). The plain-HTTP case is handled by name.Insecure
// in parseOpts, so this takes no argument.
func remoteOpts() []remote.Option {
	return []remote.Option{
		remote.WithContext(context.Background()),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	}
}
