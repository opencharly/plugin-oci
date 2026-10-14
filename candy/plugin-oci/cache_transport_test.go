package oci

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencharly/spec/cache"
	pb "github.com/opencharly/spec/proto"
	"github.com/opencharly/spec/spec"
)

// cache_transport_test.go — the verb:oci cache transport. TestCacheTransportDeterministic
// drives the REAL transport legs (cachePushLeg → cachePullLeg) against an
// in-memory go-containerregistry registry (httptest), so it FAILS if
// cache_transport.go is removed or broken — no external registry required. The
// LIVE test additionally drives the same legs against a real registry:2 when
// LIVE_REGISTRY is set (R7a: live-or-skip, never a fake).

// TestCacheTransportDeterministic is the deterministic gate for the transport:
// it pushes a named ArtifactStore through cachePushLeg, pulls it back through
// cachePullLeg into a fresh dir via an in-memory registry, and asserts every
// entry survived byte-identical.
func TestCacheTransportDeterministic(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	src := t.TempDir()
	store := cache.OpenLayout(src)
	if err := store.Put("alpha", cache.Entry{Payload: []byte(`{"v":1}`), Components: map[string]string{"sha": "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("beta", cache.Entry{Payload: []byte(`{"v":2}`), Validator: `W/"e"`}); err != nil {
		t.Fatal(err)
	}

	ref := host + "/charly-cache:deterministic"
	// Drive the LEG (not runCachePush directly) so the leg's error contract is
	// exercised too.
	pushReply, err := cachePushLeg(mustJSON(t, spec.CacheTransferRequest{Dir: src, Ref: ref, Insecure: true}))
	if err != nil {
		t.Fatalf("cachePushLeg: %v", err)
	}
	var pushed spec.CacheTransferReply
	if err := decodeReply(pushReply, &pushed); err != nil {
		t.Fatalf("decode push reply: %v", err)
	}
	if pushed.Digest == "" || pushed.Entries != 2 {
		t.Fatalf("push reply = %+v, want 2 entries and a digest", pushed)
	}

	dst := filepath.Join(t.TempDir(), "pulled")
	pullReply, err := cachePullLeg(mustJSON(t, spec.CacheTransferRequest{Dir: dst, Ref: ref, Insecure: true}))
	if err != nil {
		t.Fatalf("cachePullLeg: %v", err)
	}
	var pulled spec.CacheTransferReply
	if err := decodeReply(pullReply, &pulled); err != nil {
		t.Fatalf("decode pull reply: %v", err)
	}
	if pulled.Digest != pushed.Digest {
		t.Fatalf("pull digest %s != push digest %s", pulled.Digest, pushed.Digest)
	}

	restored := cache.OpenLayout(dst)
	for _, key := range []string{"alpha", "beta"} {
		orig, _ := store.Get(key)
		got, ok := restored.Get(key)
		if !ok {
			t.Fatalf("key %q missing after transport round-trip", key)
		}
		if string(got.Payload) != string(orig.Payload) {
			t.Fatalf("key %q payload = %q, want %q", key, got.Payload, orig.Payload)
		}
		if got.Validator != orig.Validator {
			t.Fatalf("key %q validator = %q, want %q", key, got.Validator, orig.Validator)
		}
		if !reflect.DeepEqual(got.Components, orig.Components) {
			t.Fatalf("key %q components = %v, want %v (lossless claim)", key, got.Components, orig.Components)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "oci-layout")); err != nil {
		t.Fatalf("restored layout missing oci-layout marker: %v", err)
	}
}

// TestCachePushLegErrorsOnBadRef proves the leg surfaces a REAL error (the
// round-1 finding: the legs must not report success-shaped replies on failure).
func TestCachePushLegErrorsOnBadRef(t *testing.T) {
	src := t.TempDir()
	if err := cache.OpenLayout(src).Put("k", cache.Entry{Payload: []byte(`1`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := cachePushLeg(mustJSON(t, spec.CacheTransferRequest{Dir: src, Ref: "http://not a valid ref", Insecure: true})); err == nil {
		t.Fatal("cachePushLeg must return an error for an unparseable ref, not a success reply")
	}
}

// TestCachePushPullLiveRoundTrip is the LIVE proof: needs a real registry at
// LIVE_REGISTRY (e.g. localhost:5000 for registry:2). Absent it, SKIPS cleanly.
func TestCachePushPullLiveRoundTrip(t *testing.T) {
	registryHost := os.Getenv("LIVE_REGISTRY")
	if registryHost == "" {
		t.Skip("LIVE_REGISTRY unset — skipping the live registry round-trip (set it to e.g. localhost:5000)")
	}
	src := t.TempDir()
	if err := cache.OpenLayout(src).Put("live-key", cache.Entry{Payload: []byte(`{"live":true}`)}); err != nil {
		t.Fatal(err)
	}
	ref := registryHost + "/charly-cache-test:live"
	if _, err := runCachePush(spec.CacheTransferRequest{Dir: src, Ref: ref, Insecure: true}); err != nil {
		t.Fatalf("cache-push failed: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "pulled")
	if _, err := runCachePull(spec.CacheTransferRequest{Dir: dst, Ref: ref, Insecure: true}); err != nil {
		t.Fatalf("cache-pull failed: %v", err)
	}
	got, ok := cache.OpenLayout(dst).Get("live-key")
	if !ok || string(got.Payload) != `{"live":true}` {
		t.Fatalf("live round-trip lost the entry: ok=%v payload=%q", ok, got.Payload)
	}
}

// TestCacheInsecureOptionParsed pins that Insecure selects name.Insecure — the
// option that sets the registry SCHEME to http (it does not gate parsing). The
// parsed reference's scheme is the observable that the option controls.
func TestCacheInsecureOptionParsed(t *testing.T) {
	if len(parseOpts(true)) != 1 {
		t.Fatalf("parseOpts(true) must yield exactly name.Insecure, got %v", parseOpts(true))
	}
	if len(parseOpts(false)) != 0 {
		t.Fatalf("parseOpts(false) must yield no options, got %v", parseOpts(false))
	}
	insecure, err := name.ParseReference("registry.example.com/charly-cache:tag", parseOpts(true)...)
	if err != nil {
		t.Fatalf("parse with Insecure: %v", err)
	}
	secure, err := name.ParseReference("registry.example.com/charly-cache:tag")
	if err != nil {
		t.Fatalf("parse without options: %v", err)
	}
	if insecure.Context().Registry.Scheme() != "http" {
		t.Fatalf("Insecure must select the http scheme, got %q", insecure.Context().Registry.Scheme())
	}
	if secure.Context().Registry.Scheme() == "http" {
		t.Fatal("without Insecure a non-localhost registry must use https")
	}
}

// TestCachePullSingleImageRef is the DETERMINISTIC gate for the media-type dispatch: a
// registry serves an ordinary image tag as a SINGLE IMAGE manifest, and the pull leg must
// fetch it. The earlier `remote.Index(ref)`-only form could not, and failed hard:
//
//	unexpected media type for ImageIndex(): application/vnd.docker.distribution.manifest.v2+json; call Image() instead
//
// Measured by the ingest leg (plugin-cache) against exactly this shape, which is why the
// assertion is on the CONTENT that lands rather than on an error being absent.
func TestCachePullSingleImageRef(t *testing.T) {
	host, live := liveRegistryHost(t, "charly-single-live:one")
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	if !live {
		// No LIVE_REGISTRY: the in-memory registry still exercises the dispatch and the
		// landing shape, so the deterministic arm is real. The BED sets LIVE_REGISTRY (the
		// declared reference registry), so there this arm runs against a REAL
		// distribution-spec registry rather than beside one.
		host = strings.TrimPrefix(srv.URL, "http://")
		t.Log("LIVE_REGISTRY unset — running the single-image arm against the in-memory registry")
	}

	// A real single image in the registry: no index anywhere in the transfer.
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(host+"/charly-single:one", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	if live {
		t.Logf("the single-image arm runs LIVE against %s", host)
	}

	// The failure the fix removes, asserted as the CONTROL: the index-only call must
	// still fail on this ref, so the test proves the dispatch is what made the pull work
	// rather than that the registry changed shape.
	if _, idxErr := remote.Index(ref, remoteOpts()...); idxErr == nil {
		t.Fatal("the control failed: remote.Index on a single-image ref succeeded, so this test cannot show what the dispatch fixed")
	}

	dst := filepath.Join(t.TempDir(), "pulled-single")
	reply, err := cachePullLeg(mustJSON(t, spec.CacheTransferRequest{Dir: dst, Ref: ref.String(), Insecure: true}))
	if err != nil {
		t.Fatalf("cachePullLeg on a single-image ref: %v", err)
	}
	var pulled spec.CacheTransferReply
	if err := decodeReply(reply, &pulled); err != nil {
		t.Fatalf("decode pull reply: %v", err)
	}
	if pulled.Digest != wantDigest.String() {
		t.Fatalf("pull reply digest = %s, want the image digest %s", pulled.Digest, wantDigest)
	}

	// The landing shape: a readable OCI layout whose top-level index carries the image as
	// ONE descriptor annotated with the ref — the shape the ingest consumes.
	rawIndex, err := os.ReadFile(filepath.Join(dst, "index.json"))
	if err != nil {
		t.Fatalf("the pull wrote no index.json: %v", err)
	}
	var idx struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(rawIndex, &idx); err != nil {
		t.Fatalf("index.json is not an index: %v", err)
	}
	if idx.MediaType != string(types.OCIImageIndex) {
		t.Fatalf("index.json mediaType = %q, want %q (a bare image manifest has no index to read)", idx.MediaType, types.OCIImageIndex)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("index.json carries %d descriptors, want exactly ONE root descriptor", len(idx.Manifests))
	}
	if idx.Manifests[0].Digest != wantDigest.String() {
		t.Fatalf("the root descriptor points at %s, want the image manifest %s", idx.Manifests[0].Digest, wantDigest)
	}
	if got := idx.Manifests[0].Annotations[refNameAnnotation]; got != ref.String() {
		t.Fatalf("the root descriptor annotation %s = %q, want the ref %q", refNameAnnotation, got, ref.String())
	}

	// And the bytes really landed: the image manifest in the layout hashes to the digest
	// the reply named, so this is content and not a descriptor pointing at nothing.
	blob, err := os.ReadFile(filepath.Join(dst, "blobs", "sha256", wantDigest.Hex))
	if err != nil {
		t.Fatalf("the pulled image manifest is not in the layout: %v", err)
	}
	gotSum := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(blob))
	if gotSum != wantDigest.String() {
		t.Fatalf("the pulled manifest hashes to %s, want %s", gotSum, wantDigest)
	}
}

// TestCachePullIndexRefStaysAnIndex pins the OTHER arm against the tempting wrong fix:
// `desc.Image()` does NOT fail on an index ref — it silently resolves the index to one
// platform's image — so a dispatch shaped "try ImageIndex, fall back to Image" would turn
// every index pull into a single-platform image. The index arm therefore has to be keyed on
// the media type, and this test fails if it ever stops being one: the pulled layout must
// still carry the index's whole entry set, not one image.
func TestCachePullIndexRefStaysAnIndex(t *testing.T) {
	host, live := liveRegistryHost(t, "charly-index-live:multi")
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	if !live {
		host = strings.TrimPrefix(srv.URL, "http://")
	}

	// An INDEX ref carrying two DISTINCT platform entries.
	ii, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(host+"/charly-index:multi", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, ii); err != nil {
		t.Fatal(err)
	}
	if live {
		t.Logf("the index arm runs LIVE against %s", host)
	}
	wantIM, err := ii.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "pulled-index")
	reply, err := cachePullLeg(mustJSON(t, spec.CacheTransferRequest{Dir: dst, Ref: ref.String(), Insecure: true}))
	if err != nil {
		t.Fatalf("cachePullLeg on an index ref: %v", err)
	}
	var pulled spec.CacheTransferReply
	if err := decodeReply(reply, &pulled); err != nil {
		t.Fatalf("decode pull reply: %v", err)
	}
	if pulled.Entries != len(wantIM.Manifests) || pulled.Entries < 2 {
		t.Fatalf("pull reply entries = %d, want the index's %d (an index ref must not collapse to one image)", pulled.Entries, len(wantIM.Manifests))
	}

	rawIndex, err := os.ReadFile(filepath.Join(dst, "index.json"))
	if err != nil {
		t.Fatalf("the pull wrote no index.json: %v", err)
	}
	var idx struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(rawIndex, &idx); err != nil {
		t.Fatalf("index.json is not an index: %v", err)
	}
	if len(idx.Manifests) != len(wantIM.Manifests) {
		t.Fatalf("the pulled layout carries %d entries, want the index's %d (the index was collapsed)", len(idx.Manifests), len(wantIM.Manifests))
	}
	for i, m := range wantIM.Manifests {
		if idx.Manifests[i].Digest != m.Digest.String() {
			t.Fatalf("entry %d = %s, want %s", i, idx.Manifests[i].Digest, m.Digest)
		}
	}

	// ⚑ THE INDEX ARM'S SHAPE IS A CONTRACT, NOT A STYLE — pinned here because the
	// tempting "wrap it like the image arm does" change BREAKS it, measured:
	// `layout.Write(empty.Index)` + `AppendDescriptor(index)` produces an `index.json`
	// carrying ONE root descriptor, and `spec/cache`'s Layout reads index.json's manifests
	// BY TAG-DERIVED KEY, so the named entries (`alpha`/`beta`) stop resolving and
	// `TestCacheTransportDeterministic` fails with `key "alpha" missing after transport
	// round-trip`. So the index arm must write the fetched index AS `index.json`, and the
	// two assertions below are what a wrapper would violate:
	//   (a) index.json's OWN bytes hash to the digest the transport REPORTED — the whole
	//       index IS the artifact (this is also the first branch of the ingest leg's own
	//       `stagedArtifactOf`, which accepts either shape by measurement), and
	//   (b) index.json holds the index's N entry descriptors, not 1.
	ownSum := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(rawIndex))
	if ownSum != pulled.Digest {
		t.Fatalf("index.json hashes to %s but the transport reported %s — a consumer can no longer tell the staged index IS the artifact", ownSum, pulled.Digest)
	}
	if len(idx.Manifests) == 1 {
		t.Fatal("index.json carries a single root descriptor — the index was wrapped, which stops spec/cache's key-by-tag resolution (measured: key \"alpha\" goes missing)")
	}
}

// liveRegistryHost resolves the host the arm should run against: the LIVE_REGISTRY
// environment (the bed sets it to the reference registry it composes) when present, else
// "". `want` is the tag the caller will push, so a live run cannot collide with another
// arm's ref. A live arm is reported through t.Log so the retained evidence names which
// registry the run actually measured — the difference between running live and skipping
// beside a live registry is exactly what R8 turns on.
func liveRegistryHost(t *testing.T, want string) (string, bool) {
	t.Helper()
	host := os.Getenv("LIVE_REGISTRY")
	if host == "" {
		return "", false
	}
	t.Logf("LIVE_REGISTRY is set — this arm measures the real registry %s (pushing %s)", host, want)
	return host, true
}

// mustJSON marshals a request for the leg entrypoints; decodeReply unmarshals a
// leg's InvokeReply. Test-only helpers.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeReply(reply *pb.InvokeReply, out any) error {
	return json.Unmarshal(reply.GetResultJson(), out)
}
