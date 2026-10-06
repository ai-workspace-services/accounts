package api

import "os"

// RuntimeImageMetadata is shared by normal and database-standby probes. IMAGE
// remains the single release identity; a separate commit variable is not used.
func RuntimeImageMetadata() map[string]any {
	info := parseImageVersionInfo(os.Getenv("IMAGE"))
	return map[string]any{"status": "ok", "image": info.ImageRef, "tag": info.Tag, "commit": info.Commit, "version": info.Version}
}
