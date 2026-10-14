package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/opencharly/spec/spec"
)

// plugin_artifact_test.go — the verb:oci PLUGIN-ARTIFACT transport.
//
// TestPluginArtifactTransportDeterministic drives the REAL legs
// (pluginPushLeg → pluginPullLeg) against an in-memory go-containerregistry registry
// (httptest), so it fails if plugin_artifact.go is removed or broken — no external
// registry, no credential. TestPluginArtifactLiveRoundTrip drives the same legs against
// a real registry when LIVE_REGISTRY is set and SKIPS VISIBLY when it is not (R7a: live
// or skip, never a fake).
//
// The fixture layout below is hand-written on purpose: it is the INPUT to a transport
// test, not a second implementation of the artifact format. The production assembler is
// the shared sdk mechanism (the emitter and the consumer read the same bytes through the
// same code); this fixture exists so the transport can be proven without it, and the
// transport's own validation — not the fixture — is what asserts the format.

// fixtureArtifact is one platform's payload set.
type fixtureArtifact struct {
	platform  string // "linux/amd64"
	binary    []byte
	manifest  []byte // the candy charly.yml
	providers []byte // newline-joined class:word
	metadata  []byte // the config blob (JSON)
}

// writeFixtureLayout writes a minimal plugin-artifact OCI image layout: raw blobs under
// blobs/sha256/, a manifest carrying artifactType + the plugin config media type + the
// three typed layer roles, an index descriptor with the image-spec `platform` object, and
// the oci-layout marker.
func writeFixtureLayout(t *testing.T, dir string, arts ...fixtureArtifact) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeBlob := func(b []byte) (string, int64) {
		sum := sha256.Sum256(b)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", hex.EncodeToString(sum[:])), b, 0o644); err != nil {
			t.Fatal(err)
		}
		return digest, int64(len(b))
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	type desc struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Platform    *v1.Platform      `json:"platform,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	manifests := make([]desc, 0, len(arts))
	for _, a := range arts {
		cfgDigest, cfgSize := writeBlob(a.metadata)
		binDigest, binSize := writeBlob(a.binary)
		mfDigest, mfSize := writeBlob(a.manifest)
		pvDigest, pvSize := writeBlob(a.providers)

		manifest := map[string]any{
			"schemaVersion": 2,
			"mediaType":     spec.MediaTypePluginArtifactManifest,
			"artifactType":  spec.ArtifactTypePlugin,
			"config": map[string]any{
				"mediaType": spec.MediaTypePluginConfig,
				"digest":    cfgDigest,
				"size":      cfgSize,
			},
			"layers": []map[string]any{
				{"mediaType": spec.MediaTypePluginBinary, "digest": binDigest, "size": binSize},
				{"mediaType": spec.MediaTypePluginManifest, "digest": mfDigest, "size": mfSize},
				{"mediaType": spec.MediaTypePluginProviders, "digest": pvDigest, "size": pvSize},
			},
		}
		rawManifest, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		manDigest, manSize := writeBlob(rawManifest)

		osName, arch, variant := splitPlatform(a.platform)
		manifests = append(manifests, desc{
			MediaType: spec.MediaTypePluginArtifactManifest,
			Digest:    manDigest,
			Size:      manSize,
			Platform:  &v1.Platform{OS: osName, Architecture: arch, Variant: variant},
			// The layout's index descriptor names its tag this way — the tag a push
			// carries. image-spec: ref.name is valid on layout index descriptors.
			Annotations: map[string]string{"org.opencontainers.image.ref.name": "test"},
		})
	}
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     spec.MediaTypePluginArtifactIndex,
		"artifactType":  spec.ArtifactTypePlugin,
		"manifests":     manifests,
	}
	rawIndex, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), rawIndex, 0o644); err != nil {
		t.Fatal(err)
	}
}

// splitPlatform splits "os/arch[/variant]".
func splitPlatform(p string) (string, string, string) {
	parts := strings.Split(p, "/")
	if len(parts) == 2 {
		return parts[0], parts[1], ""
	}
	return parts[0], parts[1], parts[2]
}

// artifactFixture returns a realistic single-platform fixture (a ~30-byte "binary" is
// enough to prove byte-identity; the point is the transport, not the payload size).
func artifactFixture() fixtureArtifact {
	return fixtureArtifact{
		platform:  "linux/amd64",
		binary:    []byte("\x7fELF\x02\x01\x01\x00fake-provider-binary-for-transport-test"),
		manifest:  []byte("plugin-bpf:\n    candy:\n        plugin:\n            providers:\n                - command:bpf\n"),
		providers: []byte("command:bpf\nverb:bpf\n"),
		metadata:  []byte(`{"module":"github.com/opencharly/plugin-bpf/candy/plugin-bpf","revision":"deadbeef","go_sum":"h1:x=","toolchain":"go1.26.0","os":"linux","architecture":"amd64","libc":"glibc","protocol":2,"calver":"2026.286.0739","schema_digest":"sha256:00","providers":["command:bpf","verb:bpf"],"layers":{}}`),
	}
}

func TestPluginArtifactTransportDeterministic(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	fx := artifactFixture()
	src := filepath.Join(t.TempDir(), "src")
	writeFixtureLayout(t, src, fx)

	ref := host + "/opencharly/plugin-bpf:2026.286.0739"
	pushReply, err := pluginPushLeg(mustJSON(t, spec.PluginArtifactTransferRequest{Dir: src, Ref: ref, Insecure: true}))
	if err != nil {
		t.Fatalf("pluginPushLeg: %v", err)
	}
	var pushed spec.PluginArtifactTransferReply
	if err := decodeReply(pushReply, &pushed); err != nil {
		t.Fatalf("decode push reply: %v", err)
	}
	if pushed.Digest == "" {
		t.Fatalf("push reply carries no digest: %+v", pushed)
	}
	if pushed.Platform != "linux/amd64" {
		t.Fatalf("push reply platform = %q, want linux/amd64", pushed.Platform)
	}

	dst := filepath.Join(t.TempDir(), "pulled")
	pullReply, err := pluginPullLeg(mustJSON(t, spec.PluginArtifactTransferRequest{Dir: dst, Ref: ref, Insecure: true, Platform: "linux/amd64"}))
	if err != nil {
		t.Fatalf("pluginPullLeg: %v", err)
	}
	var pulled spec.PluginArtifactTransferReply
	if err := decodeReply(pullReply, &pulled); err != nil {
		t.Fatalf("decode pull reply: %v", err)
	}
	if pulled.Digest != pushed.Digest {
		t.Fatalf("pull digest %s != push digest %s — the artifact did not round-trip", pulled.Digest, pushed.Digest)
	}
	if pulled.Platform != "linux/amd64" {
		t.Fatalf("pull reply platform = %q, want linux/amd64", pulled.Platform)
	}
	if len(pulled.Platforms) != 1 || pulled.Platforms[0] != "linux/amd64" {
		t.Fatalf("pull reply platforms = %v, want [linux/amd64]", pulled.Platforms)
	}

	// BYTE IDENTITY of every blob the consumer materializes from — the claim the whole
	// capability rests on. Checked by content, not by descriptor, so a transport that
	// silently rewrote a payload cannot pass by keeping the digest bookkeeping intact.
	lp, err := layout.FromPath(dst)
	if err != nil {
		t.Fatalf("open pulled layout: %v", err)
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		t.Fatalf("pulled index: %v", err)
	}
	im, err := ii.IndexManifest()
	if err != nil {
		t.Fatalf("pulled index manifest: %v", err)
	}
	rawIndex, err := ii.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawIndex), spec.ArtifactTypePlugin) {
		t.Fatalf("pulled index lost its artifactType: %s", rawIndex)
	}
	img, err := lp.Image(im.Manifests[0].Digest)
	if err != nil {
		t.Fatalf("pulled manifest: %v", err)
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	var mf struct {
		ArtifactType string `json:"artifactType"`
		Config       struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(rawManifest, &mf); err != nil {
		t.Fatal(err)
	}
	if mf.ArtifactType != spec.ArtifactTypePlugin {
		t.Fatalf("pulled manifest artifactType = %q, want %q", mf.ArtifactType, spec.ArtifactTypePlugin)
	}
	if mf.Config.MediaType != spec.MediaTypePluginConfig {
		t.Fatalf("pulled config media type = %q, want %q", mf.Config.MediaType, spec.MediaTypePluginConfig)
	}
	want := map[string][]byte{
		spec.MediaTypePluginBinary:    fx.binary,
		spec.MediaTypePluginManifest:  fx.manifest,
		spec.MediaTypePluginProviders: fx.providers,
	}
	for _, l := range mf.Layers {
		payload, ok := want[l.MediaType]
		if !ok {
			t.Fatalf("pulled artifact carries an unexpected layer role %q", l.MediaType)
		}
		digest, err := v1.NewHash(l.Digest)
		if err != nil {
			t.Fatal(err)
		}
		got, err := blobBytes(dst, digest)
		if err != nil {
			t.Fatalf("read layer %s: %v", l.MediaType, err)
		}
		if string(got) != string(payload) {
			t.Fatalf("layer %s round-tripped changed: got %d bytes, want %d", l.MediaType, len(got), len(payload))
		}
		delete(want, l.MediaType)
	}
	if len(want) != 0 {
		t.Fatalf("pulled artifact is missing layer roles: %v", keysOf(want))
	}
}

// blobBytes reads a blob out of a layout DIRECTORY by digest. It takes the directory
// rather than a layout.Path because layout.Path keeps its path unexported — and reading
// the byte at its content address is exactly the assertion (a digest-shaped bookkeeping
// success would not survive it).
func blobBytes(dir string, digest v1.Hash) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, "blobs", digest.Algorithm, digest.Hex))
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPluginArtifactPullRefusesUnknownPlatform is the LOUD-NON-MATCH contract: a request
// the artifact cannot answer names what the artifact DOES carry. This is the defect
// `charly box pull --platform` ships today (a documented flag that silently does nothing),
// and a platform-aware pull must not repeat it.
func TestPluginArtifactPullRefusesUnknownPlatform(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	src := filepath.Join(t.TempDir(), "src")
	writeFixtureLayout(t, src, artifactFixture())
	ref := host + "/opencharly/plugin-bpf:platforms"
	if _, err := pluginPushLeg(mustJSON(t, spec.PluginArtifactTransferRequest{Dir: src, Ref: ref, Insecure: true})); err != nil {
		t.Fatalf("pluginPushLeg: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "pulled")
	_, err := pluginPullLeg(mustJSON(t, spec.PluginArtifactTransferRequest{Dir: dst, Ref: ref, Insecure: true, Platform: "linux/s390x"}))
	if err == nil {
		t.Fatal("pluginPullLeg must REFUSE a platform the artifact does not carry, not fall through")
	}
	msg := err.Error()
	if !strings.Contains(msg, "linux/s390x") {
		t.Errorf("refusal must name the requested platform: %v", err)
	}
	if !strings.Contains(msg, "linux/amd64") {
		t.Errorf("refusal must name the platforms the artifact DOES carry: %v", err)
	}
}

// TestPluginArtifactPushRefusesMalformedLayout: a layout that is not a plugin artifact —
// here one missing the `.providers` role — must not reach a registry where a consumer
// would take it for one. The refusal names the layers that ARE present.
func TestPluginArtifactPushRefusesMalformedLayout(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	dir := filepath.Join(t.TempDir(), "src")
	writeFixtureLayout(t, dir, artifactFixture())
	// Drop the `.providers` layer from the manifest, keeping every blob in place: the
	// artifact is now structurally incomplete rather than merely absent.
	indexPath := filepath.Join(dir, "index.json")
	rawIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]any
	if err := json.Unmarshal(rawIndex, &index); err != nil {
		t.Fatal(err)
	}
	manDigest := index["manifests"].([]any)[0].(map[string]any)["digest"].(string)
	manPath := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(manDigest, "sha256:"))
	rawMan, err := os.ReadFile(manPath)
	if err != nil {
		t.Fatal(err)
	}
	var man map[string]any
	if err := json.Unmarshal(rawMan, &man); err != nil {
		t.Fatal(err)
	}
	layers := man["layers"].([]any)
	kept := make([]any, 0, len(layers))
	for _, l := range layers {
		if l.(map[string]any)["mediaType"] != spec.MediaTypePluginProviders {
			kept = append(kept, l)
		}
	}
	man["layers"] = kept
	rawMan, err = json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rawMan)
	newManPath := filepath.Join(dir, "blobs", "sha256", hex.EncodeToString(sum[:]))
	if err := os.WriteFile(newManPath, rawMan, 0o644); err != nil {
		t.Fatal(err)
	}
	index["manifests"].([]any)[0].(map[string]any)["digest"] = "sha256:" + hex.EncodeToString(sum[:])
	index["manifests"].([]any)[0].(map[string]any)["size"] = int64(len(rawMan))
	rawIndex, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, rawIndex, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = pluginPushLeg(mustJSON(t, spec.PluginArtifactTransferRequest{Dir: dir, Ref: host + "/opencharly/plugin-bpf:malformed", Insecure: true}))
	if err == nil {
		t.Fatal("pluginPushLeg must refuse an artifact missing a layer role, not ship it")
	}
	if !strings.Contains(err.Error(), spec.MediaTypePluginProviders) {
		t.Errorf("refusal must name the missing role: %v", err)
	}
}

// TestSelectPlatform pins the selection policy — including the two rules that keep a
// silently-wrong binary off a host: a request that names no variant answers only a
// BASELINE entry (an unqualified host is baseline, and a v3-built binary on a baseline CPU
// is a SIGILL), and a request that names one accepts its own variant or the baseline.
func TestSelectPlatform(t *testing.T) {
	mk := func(variant string) v1.Descriptor {
		return v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64", Variant: variant}}
	}
	index := func(ds ...v1.Descriptor) *v1.IndexManifest {
		return &v1.IndexManifest{Manifests: ds}
	}
	host := hostPlatform()

	tests := []struct {
		name         string
		im           *v1.IndexManifest
		want         string
		wantPlatform string
		wantErr      string // substring the refusal must carry
	}{
		{name: "baseline entry answers an unqualified request",
			im: index(mk("")), want: "linux/amd64", wantPlatform: "linux/amd64"},
		{name: "explicit variant request matches its own entry",
			im: index(mk("v3")), want: "linux/amd64/v3", wantPlatform: "linux/amd64/v3"},
		{name: "explicit variant request accepts a baseline entry",
			im: index(mk("")), want: "linux/amd64/v3", wantPlatform: "linux/amd64"},
		{name: "unqualified request REFUSES a variant-only artifact",
			im: index(mk("v3")), want: "linux/amd64", wantErr: "linux/amd64/v3"},
		{name: "several matches prefer the exact variant",
			im: index(mk("v3"), mk("")), want: "linux/amd64/v3", wantPlatform: "linux/amd64/v3"},
		{name: "several matches with an unqualified request prefer the baseline entry",
			im: index(mk("v3"), mk("")), want: "linux/amd64", wantPlatform: "linux/amd64"},
		{name: "wrong os refused", im: index(mk("")), want: "windows/amd64", wantErr: "windows/amd64"},
		{name: "wrong arch refused", im: index(mk("")), want: "linux/arm64", wantErr: "linux/arm64"},
		{name: "empty index refused", im: index(), want: "linux/amd64", wantErr: "no manifests"},
		{name: "empty request platform means THIS host",
			im: index(mk("")), want: "", wantPlatform: "linux/amd64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A descriptor for THIS host's platform, so the "empty request" case has
			// something to answer with on any machine.
			if tc.want == "" {
				parts := strings.Split(host, "/")
				tc.im.Manifests[0].Platform = &v1.Platform{OS: parts[0], Architecture: parts[1]}
			}
			got, platforms, err := selectPlatform(tc.im, tc.want)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("selectPlatform accepted an index it must refuse; got %q", platformString(got.Platform))
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("refusal = %q, want it to carry %q", err, tc.wantErr)
				}
				if len(platforms) != len(tc.im.Manifests) {
					t.Errorf("refusal listed %d platforms, want all %d", len(platforms), len(tc.im.Manifests))
				}
				return
			}
			if err != nil {
				t.Fatalf("selectPlatform(%s): %v", tc.name, err)
			}
			want := tc.wantPlatform
			if tc.want == "" {
				want = host
			}
			if gotPlatform := platformString(got.Platform); gotPlatform != want {
				t.Fatalf("selected %s, want %s", gotPlatform, want)
			}
		})
	}
}

// TestPluginArtifactLiveRoundTrip is the LIVE proof: needs a real distribution-spec
// registry at LIVE_REGISTRY (e.g. localhost:5001 for registry:2). Absent it, SKIPS
// visibly with the reason — never mocked, never silently green.
func TestPluginArtifactLiveRoundTrip(t *testing.T) {
	registryHost := os.Getenv("LIVE_REGISTRY")
	if registryHost == "" {
		t.Skip("LIVE_REGISTRY unset — skipping the live registry round-trip (set it to e.g. localhost:5001)")
	}
	// The arm reports the registry it measured, so a retained log distinguishes a run that
	// measured a REAL registry from one that skipped beside it — the distinction R8 turns on,
	// and the reason the bed asserts this line.
	t.Logf("LIVE_REGISTRY is set — this arm measures the real registry %s", registryHost)
	fx := artifactFixture()
	src := filepath.Join(t.TempDir(), "src")
	writeFixtureLayout(t, src, fx)
	ref := registryHost + "/opencharly/plugin-bpf:live"

	pushed, err := runPluginPush(spec.PluginArtifactTransferRequest{Dir: src, Ref: ref, Insecure: true})
	if err != nil {
		t.Fatalf("plugin-push: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "pulled")
	pulled, err := runPluginPull(spec.PluginArtifactTransferRequest{Dir: dst, Ref: ref, Insecure: true, Platform: "linux/amd64"})
	if err != nil {
		t.Fatalf("plugin-pull: %v", err)
	}
	if pulled.Digest != pushed.Digest {
		t.Fatalf("live pull digest %s != push digest %s", pulled.Digest, pushed.Digest)
	}
	lp, err := layout.FromPath(dst)
	if err != nil {
		t.Fatalf("open pulled layout: %v", err)
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	im, err := ii.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	img, err := lp.Image(im.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	var mf struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(rawManifest, &mf); err != nil {
		t.Fatal(err)
	}
	for _, l := range mf.Layers {
		digest, err := v1.NewHash(l.Digest)
		if err != nil {
			t.Fatal(err)
		}
		got, err := blobBytes(dst, digest)
		if err != nil {
			t.Fatalf("read layer %s: %v", l.MediaType, err)
		}
		want := map[string][]byte{
			spec.MediaTypePluginBinary:    fx.binary,
			spec.MediaTypePluginManifest:  fx.manifest,
			spec.MediaTypePluginProviders: fx.providers,
		}[l.MediaType]
		if len(want) == 0 {
			t.Fatalf("unexpected layer role %s in the live artifact", l.MediaType)
		}
		if string(got) != string(want) {
			t.Fatalf("live round-trip changed layer %s", l.MediaType)
		}
	}
}

// TestPluginArtifactHostPlatform pins that the default request platform is this runtime's
// own, in the image-spec vocabulary the index's `platform` object uses: GOOS/GOARCH, with
// the Go micro-architecture level appended only when the environment declares one.
func TestPluginArtifactHostPlatform(t *testing.T) {
	want := runtime.GOOS + "/" + runtime.GOARCH
	got := hostPlatform()
	if got != want && !strings.HasPrefix(got, want+"/") {
		t.Fatalf("hostPlatform() = %q, want %q or %q plus a variant", got, want, want)
	}
	if strings.Count(got, "/") > 2 {
		t.Fatalf("hostPlatform() = %q has more than os/arch/variant segments", got)
	}
	if v := variantOf(got); v != "" && v != os.Getenv("GOAMD64") && v != os.Getenv("GOARM64") {
		t.Fatalf("hostPlatform() variant %q came from neither GOAMD64 nor GOARM64", v)
	}
}
