package main

import (
	"log"

	"github.com/welcometotheweb/rmmway/server/internal/releases"
)

// wireReleases builds the signed agent release distribution (W4-2) when
// RMMWAY_RELEASES_DIR is set to a directory holding release.json + signed
// binaries — the server serves them at /agent/releases/* for the agents'
// auto-update. Unset = the routes 404 and agents treat themselves as
// up-to-date. Returns nil when unset. Pure move out of main() (wave-0 F2).
func wireReleases() *releases.Server {
	var relSrv *releases.Server
	if releasesDir := env("RMMWAY_RELEASES_DIR", ""); releasesDir != "" {
		relSrv, err := releases.New(releasesDir)
		if err != nil {
			log.Fatalf("releases: %v", err)
		}
		log.Printf("agent releases: serving signed releases from %s", relSrv.Dir())
	}
	return relSrv
}
