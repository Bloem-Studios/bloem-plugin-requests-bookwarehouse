// Command bloem-plugin-requests-bookwarehouse serves the ordinary request router.
package main

import (
	_ "embed"
	"github.com/Bloem-Studios/bloem-plugin-requests-bookwarehouse/internal/router"
	sdkruntime "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte
var version = "0.1.5"

func main() {
	sdkruntime.ServeManifest(manifestJSON, version, sdkruntime.CapabilityServers{RequestRouter: router.New()})
}
