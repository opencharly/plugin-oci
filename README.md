# plugin-oci

The `plugin-oci` plugin candy of the [opencharly/charly](https://github.com/opencharly/charly)
candy library, as a standalone repo (the candy de-submodule cutover, plugin
kind). The Go module lives at `candy/plugin-oci/` with module path
`github.com/opencharly/plugin-oci/candy/plugin-oci`; the charly resolver fetches this repo at the pinned tag and
the compiled-in wiring imports the module at that path.
