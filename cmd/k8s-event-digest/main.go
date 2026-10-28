// Command k8s-event-digest aggregates Kubernetes Events into a ranked digest.
package main

import (
	"os"

	"github.com/moveeeax/k8s-event-digest/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
