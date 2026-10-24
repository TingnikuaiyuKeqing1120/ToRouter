package configbackup

import (
	"torouter/internal/store"
)

// FullConfigPayload is the decrypted complete configuration of a to-router instance.
type FullConfigPayload = store.FullConfig

// FullConfigSummary is the overview of items contained in a configuration backup.
type FullConfigSummary = store.FullConfigSummary
