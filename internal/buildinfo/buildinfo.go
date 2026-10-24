// Package buildinfo contains public build metadata injected by the release build.
package buildinfo

import "runtime"

var (
	Version         = "dev"
	Commit          = ""
	BuildTime       = ""
	UpdateFeedURL   = ""
	UpdatePublicKey = ""
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func Current() Info { return Info{Version, Commit, BuildTime, runtime.GOOS, runtime.GOARCH} }
