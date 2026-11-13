package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionFromBuild(t *testing.T) {
	for _, test := range []struct {
		name     string
		override string
		module   string
		modified bool
		want     string
	}{
		{name: "installed release", module: "v0.1.0", want: "v0.1.0"},
		{name: "clean release checkout", module: "v0.1.0", want: "v0.1.0"},
		{name: "dirty release checkout", module: "v0.1.0+dirty", modified: true, want: "dev"},
		{name: "modified VCS setting", module: "v0.1.0", modified: true, want: "dev"},
		{name: "untagged checkout", module: "v0.0.0-20261112000000-0123456789ab", want: "dev"},
		{name: "commit after release", module: "v0.1.1-0.20261112000000-0123456789ab", want: "dev"},
		{name: "source archive", module: "(devel)", want: "dev"},
		{name: "missing module version", want: "dev"},
		{name: "container release", override: "v0.1.0", want: "v0.1.0"},
		{name: "container development", override: "dev", want: "dev"},
		{name: "invalid override", override: "release", want: "dev"},
		{name: "incomplete version", override: "v0.1", want: "dev"},
		{name: "future release", module: "v0.2.0", want: "v0.2.0"},
		{name: "release candidate", module: "v0.2.0-rc.1", want: "v0.2.0-rc.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			override := test.override
			if override == "" {
				override = "dev"
			}
			info := &debug.BuildInfo{Main: debug.Module{Version: test.module}}
			if test.modified {
				info.Settings = []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}
			}
			if got := versionFromBuild(override, info); got != test.want {
				t.Fatalf("version = %q, want %q", got, test.want)
			}
		})
	}
	if got := versionFromBuild("dev", nil); got != "dev" {
		t.Fatalf("missing build information: %q", got)
	}
	if got := versionFromBuild("v0.1.0", nil); got != "v0.1.0" {
		t.Fatalf("linker version without build information: %q", got)
	}
}
