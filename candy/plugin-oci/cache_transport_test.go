package oci

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/opencharly/spec/cache"
)

// cache_transport_test.go — the verb:oci cache transport. The push half needs a
// live registry (gated by LIVE_*); the LAYOUT round-trip is deterministic and
// proves the lossless bridge: a real spec/cache ArtifactStore (an OCI Image
// Layout) survives export -> re-import with every entry intact.

// TestCacheLayoutRoundTrip proves a named ArtifactStore (a real OCI layout) reads
// back identically after being re-materialized through the go-containerregistry
// layout index path the transport uses (layout.ImageIndex -> layout.Write).
func TestCacheLayoutRoundTrip(t *testing.T) {
	src := t.TempDir()
	store := cache.OpenLayout(src)
	if err := store.Put("alpha", cache.Entry{Payload: []byte(`{"v":1}`), Components: map[string]string{"sha": "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("beta", cache.Entry{Payload: []byte(`{"v":2}`), Validator: `W/"e"`}); err != nil {
		t.Fatal(err)
	}

	// Export: the transport reads the layout's index...
	lp, err := layout.FromPath(src)
	if err != nil {
		t.Fatalf("open layout: %v", err)
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		t.Fatalf("image index: %v", err)
	}
	im, err := ii.IndexManifest()
	if err != nil {
		t.Fatalf("index manifest: %v", err)
	}
	if len(im.Manifests) != 2 {
		t.Fatalf("index has %d manifests, want 2 (one per cache entry)", len(im.Manifests))
	}

	// ...and re-imports it into a fresh layout dir (the pull half, sans network).
	dst := filepath.Join(t.TempDir(), "restored")
	if _, err := layout.Write(dst, ii); err != nil {
		t.Fatalf("write layout: %v", err)
	}
	restored := cache.OpenLayout(dst)
	for _, key := range []string{"alpha", "beta"} {
		orig, _ := store.Get(key)
		got, ok := restored.Get(key)
		if !ok {
			t.Fatalf("key %q missing after round-trip", key)
		}
		if string(got.Payload) != string(orig.Payload) {
			t.Fatalf("key %q payload = %q, want %q", key, got.Payload, orig.Payload)
		}
		if got.Validator != orig.Validator {
			t.Fatalf("key %q validator = %q, want %q", key, got.Validator, orig.Validator)
		}
		if !got.FreshComponents(orig.Components) && orig.Components != nil {
			t.Fatalf("key %q components drifted: %v vs %v", key, got.Components, orig.Components)
		}
	}
	// The restored layout is itself a valid OCI layout.
	if _, err := os.Stat(filepath.Join(dst, "oci-layout")); err != nil {
		t.Fatalf("restored layout missing oci-layout marker: %v", err)
	}
}

// TestCachePushPullLiveRoundTrip is the LIVE push/pull proof: it needs a real
// registry reachable at LIVE_REGISTRY (e.g. localhost:5000 for a local
// registry:2). Absent that, it SKIPS cleanly (R7a: live-or-skip, never a fake
// registry). When present, it pushes a named ArtifactStore and pulls it into a
// fresh dir, then asserts byte-identical entries.
func TestCachePushPullLiveRoundTrip(t *testing.T) {
	registry := os.Getenv("LIVE_REGISTRY")
	if registry == "" {
		t.Skip("LIVE_REGISTRY unset — skipping the live registry round-trip (set it to e.g. localhost:5000)")
	}
	src := t.TempDir()
	store := cache.OpenLayout(src)
	if err := store.Put("live-key", cache.Entry{Payload: []byte(`{"live":true}`)}); err != nil {
		t.Fatal(err)
	}
	ref := registry + "/charly-cache-test:live"
	pushReply := runCachePush(CacheTransferRequest{Dir: src, Ref: ref, Insecure: true})
	if pushReply.Digest == "" || len(pushReply.Digest) > 6 && pushReply.Digest[:6] == "error:" {
		t.Fatalf("cache-push failed: %s", pushReply.Digest)
	}
	dst := filepath.Join(t.TempDir(), "pulled")
	pullReply := runCachePull(CacheTransferRequest{Dir: dst, Ref: ref, Insecure: true})
	if pullReply.Digest == "" || len(pullReply.Digest) > 6 && pullReply.Digest[:6] == "error:" {
		t.Fatalf("cache-pull failed: %s", pullReply.Digest)
	}
	got, ok := cache.OpenLayout(dst).Get("live-key")
	if !ok || string(got.Payload) != `{"live":true}` {
		t.Fatalf("live round-trip lost the entry: ok=%v payload=%q", ok, got.Payload)
	}
}
