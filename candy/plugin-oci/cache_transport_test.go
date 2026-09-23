package oci

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencharly/spec/cache"
	pb "github.com/opencharly/spec/proto"
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
	pushReply, err := cachePushLeg(mustJSON(t, CacheTransferRequest{Dir: src, Ref: ref, Insecure: true}))
	if err != nil {
		t.Fatalf("cachePushLeg: %v", err)
	}
	var pushed CacheTransferReply
	if err := decodeReply(pushReply, &pushed); err != nil {
		t.Fatalf("decode push reply: %v", err)
	}
	if pushed.Digest == "" || pushed.Entries != 2 {
		t.Fatalf("push reply = %+v, want 2 entries and a digest", pushed)
	}

	dst := filepath.Join(t.TempDir(), "pulled")
	pullReply, err := cachePullLeg(mustJSON(t, CacheTransferRequest{Dir: dst, Ref: ref, Insecure: true}))
	if err != nil {
		t.Fatalf("cachePullLeg: %v", err)
	}
	var pulled CacheTransferReply
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
	if _, err := cachePushLeg(mustJSON(t, CacheTransferRequest{Dir: src, Ref: "http://not a valid ref", Insecure: true})); err == nil {
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
	if _, err := runCachePush(CacheTransferRequest{Dir: src, Ref: ref, Insecure: true}); err != nil {
		t.Fatalf("cache-push failed: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "pulled")
	if _, err := runCachePull(CacheTransferRequest{Dir: dst, Ref: ref, Insecure: true}); err != nil {
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
