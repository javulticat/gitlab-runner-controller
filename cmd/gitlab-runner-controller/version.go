package main

import (
	"runtime/debug"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Container builds supply the version because their context excludes Git metadata.
var version = "dev"

func buildVersion() string {
	info, _ := debug.ReadBuildInfo()
	return versionFromBuild(version, info)
}

func versionFromBuild(override string, info *debug.BuildInfo) string {
	candidate := override
	if candidate == "dev" {
		if info == nil {
			return "dev"
		}
		candidate = info.Main.Version
		for _, setting := range info.Settings {
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				return "dev"
			}
		}
	}
	if !semver.IsValid(candidate) || semver.Canonical(candidate) != candidate || module.IsPseudoVersion(candidate) {
		return "dev"
	}
	return candidate
}
