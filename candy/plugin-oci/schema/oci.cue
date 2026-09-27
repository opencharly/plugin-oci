// plugin-oci's OWN self-contained CUE schema — the SINGLE SOURCE for this plugin's
// declaration surface, served over the Describe channel (there is no schema-less
// plugin). SELF-CONTAINED: it references no base def, so it compiles STANDALONE (the
// property the SDK's serve-side compile and `cue exp gengotypes` both need).
//
// `verb:oci` is a pure INTERNAL RPC verb (never authored as an `oci:` check step): the
// host's merge / adopt-user / cache consumers reach it directly, keyed by the `oci_op`
// env discriminator. There is no structured plugin_input, so this schema DOCUMENTS the
// verb's op vocabulary and the plugin's own request shapes.
#OciPlugin: {
	// The verb word the plugin serves.
	verb: "oci"

	// What the verb does, in one line (the public-docs surface).
	contract: string & !=""

	// The internal ops the verb dispatches, keyed by the `oci_op` env discriminator.
	ops: ["merge", "inspect-user", "cache-push", "cache-pull"]
}
