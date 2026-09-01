package version

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)
