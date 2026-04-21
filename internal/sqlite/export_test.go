package sqlite

import "database/sql"

// RawDB exposes the internal *sql.DB for use in tests only.
func (d *DB) RawDB() *sql.DB { return d.db }

// MigrateForTest exposes the unexported migrate function for white-box tests.
var MigrateForTest = migrate

// SchemaForTest exposes the v1 schema constant for white-box tests.
const SchemaForTest = schema
