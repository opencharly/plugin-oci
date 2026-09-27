package oci

import (
	"context"
	"strings"
	"testing"

	"cuelang.org/go/cue/cuecontext"

	pb "github.com/opencharly/spec/proto"
	sdkschema "github.com/opencharly/spec/schema"
	"github.com/opencharly/spec/schemaconcat"
)

// TestNewMetaServesNonEmptySchema pins the uniform plugin contract: NewMeta's
// Describe reply MUST carry this plugin's own non-empty CUE schema (schema_cue).
// It FAILS without the change — before it, NewMeta passed a nil schema FS and
// served an EMPTY schema ("there is no schema-less plugin").
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

// TestServedSchemaSplicesOntoHostBase exercises the SDK/host load gate this change
// exists to satisfy: it reproduces charly's `compileBasePlusServed` splice
// (`registerPluginUnitSchema` → the `base ++ plugin` compile) by concatenating the
// plugin's OWN served schema_cue onto the real spec base schema and compiling the
// union. A schema that is empty, will not compile, or will not splice onto the base
// is rejected here exactly as at the host load gate. It is stronger than Describe
// alone (standalone compile only) and than `cue eval` on the schema file (which never
// touches the base).
func TestServedSchemaSplicesOntoHostBase(t *testing.T) {
	caps, err := NewMeta().Describe(context.Background(), &pb.Empty{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	served := caps.GetSchemaCue()
	if strings.TrimSpace(served) == "" {
		t.Fatal("empty schema_cue: cannot splice onto the host base")
	}
	baseBody, _, err := schemaconcat.ConcatSchema(sdkschema.FS, ".", nil)
	if err != nil {
		t.Fatalf("spec base schema: %v", err)
	}
	v := cuecontext.New().CompileString(baseBody + "\n" + served)
	if err := v.Err(); err != nil {
		t.Fatalf("served schema does not splice onto the base (base ++ plugin): %v", err)
	}
}
