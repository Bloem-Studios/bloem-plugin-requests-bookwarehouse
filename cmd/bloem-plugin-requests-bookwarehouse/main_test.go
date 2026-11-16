package main

import (
	"github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginsdk/manifest"
	"testing"
)

// ServeManifest replaces the embedded manifest version with this value, so a
// stale pin would make the executable report the previous release.
func TestExecutableVersionMatchesManifest(t *testing.T) {
	m, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if version != m.Version {
		t.Fatalf("executable version = %q, manifest version = %q", version, m.Version)
	}
}

func TestManifestExplicitlySupportsOnlyBooks(t *testing.T) {
	m, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if m.PluginId != "bloem.requests.bookwarehouse" || len(m.Capabilities) != 1 {
		t.Fatal(m)
	}
	c := m.Capabilities[0]
	kinds := c.GetMetadata().GetFields()["supported_media_types"].GetListValue().GetValues()
	if c.Type != "request_router.v1" || c.Id != "bookwarehouse" || len(kinds) != 1 || kinds[0].GetStringValue() != "book" {
		t.Fatal(c)
	}
}
