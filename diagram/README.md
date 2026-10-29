# Chorus Architecture Diagram

This directory contains the C4 architecture model and views for Chorus written
with the [`goa.design/model`](https://github.com/goadesign/model) DSL.

- `model/model.go`: Source architecture model (actors, software systems, containers, and relationships).
- `views.go`: View selection, layout configuration, and element/relationship visual styles.
- `design.json`: Generated serialized model representation.
- `Chorus Architecture.svg`: Rendered SVG diagram embedded in the root [`README.md`](../README.md).

## Regenerating the Diagram

Render the SVG and JSON artifacts from the repository root:

```bash
mdl svg github.com/goadesign/chorus/diagram -all -dir diagram -out diagram/design.json
```

To refine layout interactively in the MDL browser editor:

```bash
mdl serve github.com/goadesign/chorus/diagram -dir diagram -out diagram/design.json -port 8085
```
