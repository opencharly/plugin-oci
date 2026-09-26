package oci

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// container_disk_emit_test.go — the verb:oci containerDisk emitter. The
// deterministic test drives the REAL leg (containerDiskEmitLeg) against an
// in-memory go-containerregistry registry, then pulls the pushed image back and
// asserts the containerDisk contract: exactly one layer, the +gzip media type,
// the disk at /disk/disk.img byte-identical, and the labels round-tripped. So
// the test FAILS if the emitter is removed or regresses to an uncompressed
// layer. No external registry is required.

// diskLayerContract is the asserted shape of one pulled containerDisk image.
type diskLayerContract struct {
	mediaType string
	inImage   string
	content   []byte
}

// pullDiskLayer fetches imageRef from the test registry and returns the single
// layer's media type plus the first tar entry under want.
func pullDiskLayer(t *testing.T, imageRef, want string) diskLayerContract {
	t.Helper()
	ref, err := name.ParseReference(imageRef, name.Insecure)
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	img, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("remote.Image: %v", err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("layer count = %d, want 1 (a containerDisk is a scratch image with one disk layer)", len(layers))
	}
	mt, err := layers[0].MediaType()
	if err != nil {
		t.Fatalf("layer media type: %v", err)
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatalf("compressed layer: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	zr, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("layer is not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("tar entry %q not found in layer", want)
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if strings.TrimPrefix(hdr.Name, "./") == want {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read entry: %v", err)
			}
			return diskLayerContract{mediaType: string(mt), inImage: hdr.Name, content: b}
		}
	}
}

// TestContainerDiskEmitDeterministic is the deterministic gate: emit a fixture
// disk through the real leg into an in-memory registry, pull it back, and assert
// the containerDisk contract.
func TestContainerDiskEmitDeterministic(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	disk := filepath.Join(t.TempDir(), "disk.img")
	payload := []byte("not-really-a-qcow2-but-byte-identical-is-the-contract")
	if err := os.WriteFile(disk, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	ref := host + "/cua-omarchy-workspace:deterministic"
	labels := map[string]string{
		"ai.opencharly.vm.box": `{"distro":"omarchy","arch":"amd64"}`,
		"test.label":           "yes",
	}
	replyPB, err := containerDiskEmitLeg(mustJSON(t, ContainerDiskEmitRequest{
		DiskPath: disk,
		Labels:   labels,
		Ref:      ref,
		Insecure: true,
	}))
	if err != nil {
		t.Fatalf("containerDiskEmitLeg: %v", err)
	}
	var reply ContainerDiskEmitReply
	if err := decodeReply(replyPB, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if reply.Digest == "" {
		t.Fatalf("reply has no digest: %+v", reply)
	}
	if reply.MediaType != string(types.OCILayer) {
		t.Fatalf("reply media type = %q, want %q", reply.MediaType, types.OCILayer)
	}

	got := pullDiskLayer(t, ref, "disk/disk.img")
	if got.mediaType != string(types.OCILayer) {
		t.Fatalf("pulled layer media type = %q, want %q (the Fleet +gzip contract)", got.mediaType, types.OCILayer)
	}
	if string(got.content) != string(payload) {
		t.Fatalf("pulled disk = %q, want %q", got.content, payload)
	}

	// Labels round-trip on the config, and the manifest is OCI (Cua's own
	// artifact is OCI, not Docker).
	pulledRef, _ := name.ParseReference(ref, name.Insecure)
	img, err := remote.Image(pulledRef)
	if err != nil {
		t.Fatalf("remote.Image: %v", err)
	}
	if mt, merr := img.MediaType(); merr != nil || mt != types.OCIManifestSchema1 {
		t.Fatalf("manifest media type = %q (%v), want %q", mt, merr, types.OCIManifestSchema1)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	for k, v := range labels {
		if cfg.Config.Labels[k] != v {
			t.Fatalf("label %q = %q, want %q", k, cfg.Config.Labels[k], v)
		}
	}
}

// TestContainerDiskEmitValidatesInputs proves the leg surfaces real errors for
// the two unsafe inputs: an unparseable ref and a traversal in_image_path. A
// success-shaped reply here would be the failure mode.
func TestContainerDiskEmitValidatesInputs(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "d.img")
	if err := os.WriteFile(disk, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := containerDiskEmitLeg(mustJSON(t, ContainerDiskEmitRequest{DiskPath: disk, Ref: "http://not a valid ref"})); err == nil {
		t.Fatal("an unparseable ref must be an error, not a success reply")
	}
	if _, err := containerDiskEmitLeg(mustJSON(t, ContainerDiskEmitRequest{DiskPath: disk, Ref: "reg.example/x:1", InImagePath: "/disk/disk img"})); err == nil {
		t.Fatal("an in_image_path with whitespace must be an error, not an accepted tar entry")
	}
}

// TestContainerDiskEmitLiveRoundTrip is the LIVE proof: a real registry at
// LIVE_REGISTRY (e.g. localhost:5000 for registry:2) receives the emitted
// containerDisk and returns the +gzip layer with the disk intact. Absent the
// env, SKIPS cleanly (R7a live-or-skip).
func TestContainerDiskEmitLiveRoundTrip(t *testing.T) {
	registryHost := os.Getenv("LIVE_REGISTRY")
	if registryHost == "" {
		t.Skip("LIVE_REGISTRY unset — skipping the live registry round-trip (set it to e.g. localhost:5000)")
	}
	disk := filepath.Join(t.TempDir(), "disk.img")
	payload := []byte("live-container-disk-payload")
	if err := os.WriteFile(disk, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	ref := registryHost + "/charly-container-disk-test:live"
	replyPB, err := containerDiskEmitLeg(mustJSON(t, ContainerDiskEmitRequest{
		DiskPath: disk,
		Ref:      ref,
		Insecure: true,
	}))
	if err != nil {
		t.Fatalf("containerDiskEmitLeg: %v", err)
	}
	var reply ContainerDiskEmitReply
	if err := decodeReply(replyPB, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.MediaType != string(types.OCILayer) {
		t.Fatalf("live media type = %q, want %q", reply.MediaType, types.OCILayer)
	}
	got := pullDiskLayer(t, ref, "disk/disk.img")
	if string(got.content) != string(payload) {
		t.Fatalf("live pulled disk = %q, want %q", got.content, payload)
	}
}

// TestNormalizeInImagePath covers the default plus the validate/clean split.
func TestNormalizeInImagePath(t *testing.T) {
	got, err := normalizeInImagePath("")
	if err != nil || got != "disk/disk.img" {
		t.Fatalf("default = %q, %v; want disk/disk.img", got, err)
	}
	got, err = normalizeInImagePath("/disk/disk.img")
	if err != nil || got != "disk/disk.img" {
		t.Fatalf("explicit = %q, %v; want disk/disk.img", got, err)
	}
	got, err = normalizeInImagePath("/disk//./disk.img")
	if err != nil || got != "disk/disk.img" {
		t.Fatalf("cleaned = %q, %v; want disk/disk.img", got, err)
	}
	// path.Clean resolves dot-dot segments against the root, so no traversal
	// can survive into the tar header.
	got, err = normalizeInImagePath("/../../x")
	if err != nil || got != "x" {
		t.Fatalf("traversal-cleaned = %q, %v; want x (Clean removes the dot-dots)", got, err)
	}
	for _, bad := range []string{"disk/disk.img", "/", "/disk/disk img"} {
		if _, err := normalizeInImagePath(bad); err == nil {
			t.Errorf("normalizeInImagePath(%q) = nil error, want an error", bad)
		}
	}
}

// TestContainerDiskEmitLayout proves the optional local OCI Image Layout arm
// (a registry-less consumer reads it as oci:<dir>).
func TestContainerDiskEmitLayout(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	disk := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(disk, []byte("disk-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	layoutDir := filepath.Join(t.TempDir(), "layout")
	replyPB, err := containerDiskEmitLeg(mustJSON(t, ContainerDiskEmitRequest{
		DiskPath:  disk,
		Ref:       host + "/cua:layout",
		Insecure:  true,
		LayoutDir: layoutDir,
	}))
	if err != nil {
		t.Fatalf("containerDiskEmitLeg(layout): %v", err)
	}
	var reply ContainerDiskEmitReply
	if err := decodeReply(replyPB, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.LayoutDir != layoutDir {
		t.Fatalf("reply.LayoutDir = %q, want %q", reply.LayoutDir, layoutDir)
	}
	if _, err := os.Stat(filepath.Join(layoutDir, "oci-layout")); err != nil {
		t.Fatalf("layout marker missing: %v", err)
	}
}
