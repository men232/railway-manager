// The module root is an entry point of its own, so that
// `go install github.com/men232/railway-manager@latest` works as well as
// `go install github.com/men232/railway-manager/cmd/railway-manager@latest`.
// Both entry points share app.Main, so their behaviour cannot drift apart.
package main

import "github.com/men232/railway-manager/internal/app"

func main() {
	app.Main()
}
