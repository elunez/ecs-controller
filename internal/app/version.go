package app

// These values are replaced by release builds through -ldflags. Keeping the
// defaults useful makes local `go run` builds explicit about their status.
var (
	Version   = "dev"
	Commit    = "dev"
	BuildDate = "unknown"
)
