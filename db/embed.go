package db

import "embed"

//go:embed migrations/*.up.sql
var UpSQL embed.FS

//go:embed migrations/*.down.sql
var DownSQL embed.FS
