package sqlite

// SupportedSchemaVersion exposes the upstream schema version to the CPA Suite
// importer so it can refuse databases written by a newer upstream release.
// Suite-owned file: it only re-exports a constant and never changes behaviour.
const SupportedSchemaVersion = schemaVersion
