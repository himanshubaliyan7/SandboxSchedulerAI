// Package deps pins the module's third-party dependencies while packages
// are being built in parallel. Delete once every dependency has a real importer.
package deps

import (
	_ "github.com/coder/websocket"
	_ "github.com/creack/pty"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "golang.org/x/sys/unix"
	_ "golang.org/x/time/rate"
)
