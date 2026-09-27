// plugin-oci's OWN self-contained CUE schema — the SINGLE SOURCE for this plugin's
// served declaration surface (there is no schema-less plugin: every plugin ships a
// non-empty schema over Describe).
//
// SELF-CONTAINED and PACKAGE-LESS: it references no base def and carries no package
// clause, so it compiles STANDALONE — the property the SDK's serve-side compile needs
// and the property that lets the host splice `base ++ plugin` at the load gate
// (registerPluginUnitSchema); a self-contained schema that will not splice is a LOUD
// load failure.
//
// NO GO CONSUMER: the plugin declares no typed `plugin_input` (its authored input is
// its pass-through CLI grammar), so this schema generates NO `params` package and has
// NO `cue exp gengotypes` artifact — it is the SERVED documentation/config surface,
// not a code-generation source.
//
// It DOCUMENTS the internal `verb: oci` op vocabulary — `merge` / `inspect-user` / `cache-push` / `cache-pull` — keyed by the `oci_op` env discriminator. The host's merge / inspect-user / cache consumers reach the verb directly.
#OciPlugin: {
	// The verb word the plugin serves.
	verb: "oci"

	// What the verb does, in one line (the public-docs surface).
	contract: string & !=""

	// The internal ops the verb dispatches, keyed by the `oci_op` env discriminator.
	ops: ["merge", "inspect-user", "cache-push", "cache-pull"]
}
