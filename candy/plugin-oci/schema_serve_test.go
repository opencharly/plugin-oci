package oci

import (
	"context"
	"strings"
	"testing"

	pb "github.com/opencharly/spec/proto"
)

// TestNewMetaServesNonEmptySchema pins the uniform plugin contract: NewMeta's Describe
// reply MUST carry this plugin's own non-empty CUE schema (schema_cue). It FAILS without
// the change — before it, NewMeta passed a nil schema FS and served an EMPTY schema
// (there is no schema-less plugin).
func TestNewMetaServesNonEmptySchema(t *testing.T) {
	caps, err := NewMeta().Describe(context.Background(), &pb.Empty{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if strings.TrimSpace(caps.GetSchemaCue()) == "" {
		t.Fatal("Describe served an EMPTY schema_cue; every plugin MUST ship a non-empty CUE schema")
	}
	if !strings.Contains(caps.GetSchemaCue(), "#OciPlugin") {
		t.Fatalf("served schema does not carry #OciPlugin:\n%s", caps.GetSchemaCue())
	}
}
