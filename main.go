// Command terraform-provider-windowsddi is the Terraform provider for
// Microsoft Windows Server DHCP.
//
// How a provider runs: a Terraform provider is not a library that Terraform loads. It is a
// separate executable (this program) that `terraform init` downloads from the Registry.
// During plan and apply, Terraform starts it as a child process and talks to it over gRPC
// using "plugin protocol v6". Terraform asks things like "what is your schema?" or
// "read this data source", and the provider answers. When Terraform is done, it stops the
// process. Nothing here listens on the network for other clients.
//
// Go note: `package main` plus a `func main()` is how Go marks a program that compiles to an
// executable. Every other package in this repository is a library imported from here.
package main

// Go note: imports are grouped: standard library first, then third party modules, then
// packages from this repository (the "internal/..." paths, which only this module may use).
import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/provider"
)

// Run "go generate" to format example terraform files and generate the docs for the registry/website.
//
// The two `//go:generate` lines below are not executed at build time. They run only when
// someone types `go generate ./...`:
//   - `terraform fmt` reformats the HCL examples under examples/ (they feed the docs).
//   - `tfplugindocs` builds this provider, reads every schema MarkdownDescription plus the
//     examples, and regenerates the Registry pages under docs/resources and docs/data-sources.

//go:generate terraform fmt -recursive ./examples/
//go:generate go tool tfplugindocs generate -provider-name windowsddi

// version is set by goreleaser.
//
// At release time goreleaser builds with `-ldflags "-X main.version=1.2.3"`, which overwrites
// this variable inside the compiled binary. Local builds keep the placeholder "dev". The value
// is reported to Terraform through the provider's Metadata method.
//
// Go note: a lowercase name (`version`) is unexported, meaning private to this package.
// Uppercase names (`New`, `Provider`) are exported and usable from other packages.
var version = "dev"

// main parses command line flags and hands control to the plugin framework, which serves the
// gRPC protocol until Terraform shuts the process down.
func main() {
	// -debug starts the provider in "debuggable" mode: instead of being launched by Terraform,
	// you start it yourself (usually under the delve debugger), it prints a
	// TF_REATTACH_PROVIDERS value, and you export that so Terraform attaches to the running
	// process. Normal users never pass this flag.
	//
	// Go note: the `flag` package parses command line options. `&debug` passes a pointer (the
	// address of the variable) so flag.Parse can write the parsed value into it.
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	// providerserver.Serve runs the gRPC server that speaks plugin protocol v6. It receives a
	// factory (provider.New(version) returns a function that builds a fresh provider) and the
	// Registry address Terraform uses to identify this provider. It blocks until Terraform is
	// done with the process.
	//
	// Go note: `:=` declares a new variable and infers its type from the right hand side.
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/thomaschristory/windowsddi",
		Debug:   debug,
	})
	// Go note: Go has no exceptions. Functions that can fail return an `error` value, and the
	// caller checks `if err != nil`. log.Fatal prints the message and exits with status 1.
	if err != nil {
		log.Fatal(err.Error())
	}
}
