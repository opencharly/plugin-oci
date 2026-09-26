// container_disk_emit.go — the verb:oci containerDisk EMITTER: turn a
// materialized guest disk into a bootable containerDisk OCI image and push it
// to a registry (KubeVirt / Cua Fleet shape).
//
// A containerDisk is a scratch OCI image whose single layer is a tar holding
// the guest disk at an in-image path (the KubeVirt default the directory scan
// finds is /disk/disk.img, the same path the published cua-omarchy-workspace
// artifact carries). The layer media type is the load-bearing detail: Cua
// Fleet requires application/vnd.oci.image.layer.v1.tar+gzip, while a buildah
// `FROM scratch` + ADD defaults to the UNCOMPRESSED application/…v1.tar. The
// go-containerregistry stack is the one place charly can SET the media type
// explicitly (S2: buildah cannot, crane can and it round-trips through the
// library charly links HERE) — so the emitter lives in this candy, and callers
// (plugin-vm's `charly vm box publish`) reach it over verb:oci.
//
// The disk is streamed into a gzip tar layer through an io.Pipe (never staged
// or copied), wrapped by empty.Image + mutate, labelled with the caller's
// metadata, and pushed with remote.Write (the DefaultKeychain handles registry
// auth, the same as the cache transport).
package oci

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/compression"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencharly/sdk/deploykit"
	pb "github.com/opencharly/spec/proto"
)

// ContainerDiskEmitRequest is the verb:oci container-disk-emit input. It rides
// ParamsJson in the same envelope as merge/inspect-user/cache-push (this verb
// ships no authored schema — a pure internal RPC keyed by the OciOp
// discriminator). It is declared here, not taken from spec, because the spec
// wire pair (#ContainerDiskEmitRequest, spec#178 → v0.2026269.2203) is not yet
// consumable: spec main carries the C7 checkstep change (#177) that the current
// released sdk cannot compile against until sdk#309 lands. The envelope is a
// stable JSON contract the caller (candy/plugin-vm) mirrors.
type ContainerDiskEmitRequest struct {
	// DiskPath is the host path of the materialized guest disk (qcow2/raw).
	DiskPath string `json:"disk_path"`
	// InImagePath is the in-layer path the disk is stored at. Must be a
	// container-absolute path; the KubeVirt/Cua default is /disk/disk.img.
	// Empty selects that default.
	InImagePath string `json:"in_image_path,omitempty"`
	// Labels are the OCI config labels to carry (e.g. spec.LabelVmBox's JSON).
	Labels map[string]string `json:"labels,omitempty"`
	// Ref is the registry reference to push: host/repo:tag.
	Ref string `json:"ref"`
	// Insecure allows a plain-HTTP registry (a localhost dev registry).
	Insecure bool `json:"insecure,omitempty"`
	// LayoutDir, when set, ALSO writes the image as a local OCI Image Layout
	// (oci:<dir>) so the artifact can be consumed without a registry.
	LayoutDir string `json:"layout_dir,omitempty"`
}

// ContainerDiskEmitReply is the emitter result: the pushed digest and the
// media type actually written (the caller asserts the +gzip contract).
type ContainerDiskEmitReply struct {
	Ref       string `json:"ref"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	LayerSize int64  `json:"layer_size"`
	LayoutDir string `json:"layout_dir,omitempty"`
}

// defaultContainerDiskInImagePath is the KubeVirt containerDisk contract, the
// path Cua's published cua-omarchy-workspace artifact carries. It is the SDK's
// single-homed constant (deploykit.ContainerDiskPath) rather than a second
// literal, so the emitter and the VM-box reader cannot drift.
const defaultContainerDiskInImagePath = deploykit.ContainerDiskPath

// containerDiskEmitLeg is oci_op=container-disk-emit. A failure is a real Go
// error (an emit/push that did not happen must never masquerade as a
// success-shaped reply — the cache transport's contract).
func containerDiskEmitLeg(paramsJSON []byte) (*pb.InvokeReply, error) {
	var req ContainerDiskEmitRequest
	if len(paramsJSON) > 0 {
		if err := json.Unmarshal(paramsJSON, &req); err != nil {
			return nil, fmt.Errorf("oci container-disk-emit: decode request: %w", err)
		}
	}
	reply, err := runContainerDiskEmit(req)
	if err != nil {
		return nil, fmt.Errorf("oci container-disk-emit: %w", err)
	}
	j, err := json.Marshal(reply)
	if err != nil {
		return nil, fmt.Errorf("oci container-disk-emit: encode reply: %w", err)
	}
	return &pb.InvokeReply{ResultJson: j}, nil
}

// runContainerDiskEmit builds the containerDisk image and pushes it. The
// deterministic test drives the REAL leg against an in-memory registry, so the
// whole path (layer build → media type → manifest → push → digest) is
// exercised without an external registry.
func runContainerDiskEmit(req ContainerDiskEmitRequest) (ContainerDiskEmitReply, error) {
	if req.DiskPath == "" {
		return ContainerDiskEmitReply{}, fmt.Errorf("disk_path is required")
	}
	if req.Ref == "" {
		return ContainerDiskEmitReply{}, fmt.Errorf("ref is required (the registry reference to push)")
	}
	inImage, err := normalizeInImagePath(req.InImagePath)
	if err != nil {
		return ContainerDiskEmitReply{}, err
	}

	layer, layerSize, err := diskLayer(req.DiskPath, inImage)
	if err != nil {
		return ContainerDiskEmitReply{}, err
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return ContainerDiskEmitReply{}, fmt.Errorf("append disk layer: %w", err)
	}
	// empty.Image is a Docker manifest; a containerDisk is OCI (Cua's own
	// artifact is OCI). mutate.MediaType rewrites the manifest media type
	// without touching the layer (which already carries its +gzip OCI type).
	img = mutate.MediaType(img, types.OCIManifestSchema1)

	cfg, err := img.ConfigFile()
	if err != nil {
		return ContainerDiskEmitReply{}, fmt.Errorf("read image config: %w", err)
	}
	cfg = cfg.DeepCopy()
	cfg.Architecture = runtime.GOARCH
	cfg.OS = "linux"
	if len(req.Labels) > 0 {
		if cfg.Config.Labels == nil {
			cfg.Config.Labels = map[string]string{}
		}
		for k, v := range req.Labels {
			cfg.Config.Labels[k] = v
		}
	}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		return ContainerDiskEmitReply{}, fmt.Errorf("set image config: %w", err)
	}

	layers, err := img.Layers()
	if err != nil || len(layers) != 1 {
		return ContainerDiskEmitReply{}, fmt.Errorf("containerDisk image must have exactly one layer")
	}
	mt, err := layers[0].MediaType()
	if err != nil {
		return ContainerDiskEmitReply{}, fmt.Errorf("read layer media type: %w", err)
	}
	reply := ContainerDiskEmitReply{MediaType: string(mt), LayerSize: layerSize}

	ref, err := name.ParseReference(req.Ref, parseOpts(req.Insecure)...)
	if err != nil {
		return ContainerDiskEmitReply{}, fmt.Errorf("parse ref %q: %w", req.Ref, err)
	}
	if err := remote.Write(ref, img, remoteOpts()...); err != nil {
		return ContainerDiskEmitReply{}, fmt.Errorf("push to %s: %w", ref, err)
	}
	reply.Ref = ref.String()
	if dgst, derr := img.Digest(); derr == nil {
		reply.Digest = dgst.String()
	}

	if req.LayoutDir != "" {
		if err := writeImageLayout(req.LayoutDir, img); err != nil {
			return ContainerDiskEmitReply{}, err
		}
		reply.LayoutDir = req.LayoutDir
	}
	return reply, nil
}

// diskLayer builds the single gzip tar layer carrying diskPath at inImage.
// The disk is streamed (io.Pipe → tar → go-containerregistry's gzip) rather
// than staged on disk, and the media type is set explicitly to the OCI +gzip
// form — the one value buildah cannot be told to use here.
func diskLayer(diskPath, inImage string) (v1.Layer, int64, error) {
	fi, err := os.Stat(diskPath)
	if err != nil {
		return nil, 0, fmt.Errorf("disk %q: %w", diskPath, err)
	}
	if fi.IsDir() {
		return nil, 0, fmt.Errorf("disk %q is a directory (expected a disk file)", diskPath)
	}
	opener := func() (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			tw := tar.NewWriter(pw)
			hdr := &tar.Header{
				Name:     inImage,
				Mode:     0o644,
				Size:     fi.Size(),
				Typeflag: tar.TypeReg,
				ModTime:  time.Unix(0, 0),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			f, err := os.Open(diskPath)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			defer f.Close() //nolint:errcheck
			if _, err := io.Copy(tw, f); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if err := tw.Close(); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_ = pw.Close()
		}()
		return pr, nil
	}
	layer, err := tarball.LayerFromOpener(opener,
		tarball.WithCompression(compression.GZip),
		tarball.WithCompressionLevel(gzip.BestSpeed),
		tarball.WithMediaType(types.OCILayer),
	)
	if err != nil {
		return nil, 0, fmt.Errorf("build disk layer: %w", err)
	}
	size, err := layer.Size()
	if err != nil {
		return nil, 0, fmt.Errorf("measure disk layer: %w", err)
	}
	return layer, size, nil
}

// writeImageLayout writes the image into an OCI Image Layout directory
// (creating it), so a registry-less consumer can read it as oci:<dir>.
func writeImageLayout(dir string, img v1.Image) error {
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		return fmt.Errorf("create layout %s: %w", dir, err)
	}
	if err := lp.AppendImage(img); err != nil {
		return fmt.Errorf("write image to layout %s: %w", dir, err)
	}
	return nil
}

// normalizeInImagePath returns the layer-relative path for inImage (default
// /disk/disk.img), validated: a container-absolute path with no traversal and
// no whitespace/control characters. The name goes into a tar header verbatim,
// so an unvalidated value is an archive-traversal surface.
func normalizeInImagePath(inImage string) (string, error) {
	if inImage == "" {
		inImage = defaultContainerDiskInImagePath
	}
	if !strings.HasPrefix(inImage, "/") {
		return "", fmt.Errorf("in_image_path %q must be a container-absolute path", inImage)
	}
	rel := strings.TrimPrefix(path.Clean(inImage), "/")
	if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || rel == ".." {
		return "", fmt.Errorf("in_image_path %q is not a valid in-layer path", inImage)
	}
	for _, r := range rel {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '-' || r == '_' || r == '+':
		default:
			return "", fmt.Errorf("in_image_path %q has an unsafe character", inImage)
		}
	}
	return rel, nil
}
