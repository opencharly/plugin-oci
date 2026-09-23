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
// The go-containerregistry stack stays HERE (this candy), reached by charly core
// and candy/plugin-cache over verb:oci — never linked into spec or core.
package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	pb "github.com/opencharly/spec/proto"
)

// CacheTransferRequest is the verb:oci cache transport input: the local OCI
// layout directory and the registry reference. It rides ParamsJson in the same
// envelope as merge/inspect-user (this verb is a pure internal RPC keyed by the
// OciOp discriminator; it ships no authored schema).
type CacheTransferRequest struct {
	// Dir is the named cache's OCI Image Layout directory (the ArtifactStore root).
	Dir string `json:"dir"`
	// Ref is the registry reference: host/repo:tag (the tag names the cache).
	Ref string `json:"ref"`
	// Insecure allows a plain-HTTP registry (a localhost dev registry).
	Insecure bool `json:"insecure,omitempty"`
}

// CacheTransferReply is the transport result: the resolved digest and the count
// of entries (manifest descriptors) moved.
type CacheTransferReply struct {
	Digest  string `json:"digest"`
	Entries int    `json:"entries"`
	Ref     string `json:"ref"`
}

// cachePushLeg is oci_op=cache-push: read the named-cache OCI layout and push its
// index (and every referenced manifest/blob) to the registry. Lossless: the
// registry then holds a byte-identical OCI image whose index is the cache. A
// failure is a real Go error (the transport is NOT best-effort — the caller must
// see a push/pull failure, not a success-shaped reply).
func cachePushLeg(paramsJSON []byte) (*pb.InvokeReply, error) {
	var req CacheTransferRequest
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
	var req CacheTransferRequest
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
func runCachePush(req CacheTransferRequest) (CacheTransferReply, error) {
	lp, err := layout.FromPath(req.Dir)
	if err != nil {
		return CacheTransferReply{}, fmt.Errorf("open layout %s: %w", req.Dir, err)
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		return CacheTransferReply{}, fmt.Errorf("read layout index: %w", err)
	}
	ref, err := name.ParseReference(req.Ref, parseOpts(req.Insecure)...)
	if err != nil {
		return CacheTransferReply{}, fmt.Errorf("parse ref %q: %w", req.Ref, err)
	}
	if err := remote.WriteIndex(ref, ii, remoteOpts()...); err != nil {
		return CacheTransferReply{}, fmt.Errorf("push to %s: %w", ref, err)
	}
	reply := CacheTransferReply{Ref: ref.String()}
	if dgst, derr := ii.Digest(); derr == nil {
		reply.Digest = dgst.String()
	}
	if im, ierr := ii.IndexManifest(); ierr == nil {
		reply.Entries = len(im.Manifests)
	}
	return reply, nil
}

// runCachePull fetches req.Ref and writes the whole index into req.Dir.
func runCachePull(req CacheTransferRequest) (CacheTransferReply, error) {
	ref, err := name.ParseReference(req.Ref, parseOpts(req.Insecure)...)
	if err != nil {
		return CacheTransferReply{}, fmt.Errorf("parse ref %q: %w", req.Ref, err)
	}
	ii, err := remote.Index(ref, remoteOpts()...)
	if err != nil {
		return CacheTransferReply{}, fmt.Errorf("pull %s: %w", ref, err)
	}
	if err := os.MkdirAll(filepath.Dir(req.Dir), 0o755); err != nil {
		return CacheTransferReply{}, err
	}
	if _, err := layout.Write(req.Dir, ii); err != nil {
		return CacheTransferReply{}, fmt.Errorf("write layout %s: %w", req.Dir, err)
	}
	reply := CacheTransferReply{Ref: ref.String()}
	if dgst, derr := ii.Digest(); derr == nil {
		reply.Digest = dgst.String()
	}
	if im, ierr := ii.IndexManifest(); ierr == nil {
		reply.Entries = len(im.Manifests)
	}
	return reply, nil
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
