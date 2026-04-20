package sqlite

import "database/sql"

// RawDB exposes the internal *sql.DB for use in tests only.
func (d *DB) RawDB() *sql.DB { return d.db }
