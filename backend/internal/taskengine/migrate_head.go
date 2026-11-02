package taskengine

// SchemaHead reports the schema version this build's registered migrations
// migrate to. Registration itself stays in migrate.go; operator tooling uses
// this read-only view to declare the head it expects before it writes anything.
func SchemaHead() int { return migrationHead }
