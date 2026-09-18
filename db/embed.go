package db

import "embed"

//go:embed migrations/*.up.sql
var UpSQL embed.FS
