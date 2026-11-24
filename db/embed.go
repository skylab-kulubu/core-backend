package db

import "embed"

//go:embed migrations/*.up.sql
var UpSQL embed.FS

//go:embed migrations/*.down.sql
var DownSQL embed.FS

// Tasima holds the rollback files of a rename that a release window runs by
// hand (db/tasima/skynotify-geri.sql, ADR 0091); the tests read them.
//
//go:embed tasima/*.sql
var Tasima embed.FS
