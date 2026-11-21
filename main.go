// terraform-provider-boxbox is the Terraform and OpenTofu provider for
// boxbox3's HTTP API. See docs/terraform.md in the boxbox3 repository.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/triac-hq/terraform-provider-boxbox/internal/provider"
)

// version is set at build time (`-ldflags "-X main.version=..."`).
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		// The address is local: the provider is not in any registry. Both
		// tools resolve `triac-hq/boxbox` under their own default registry
		// host, and the dev override matches on that.
		Address: "registry.opentofu.org/triac-hq/boxbox",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
