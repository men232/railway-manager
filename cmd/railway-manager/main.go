package main

import "github.com/men232/railway-manager/internal/app"

// Override the version at build time with ldflags (see the Makefile):
//   go build -ldflags "-X github.com/men232/railway-manager/internal/version.Version=v0.1.0 -X github.com/men232/railway-manager/internal/version.Commit=$(git rev-parse --short HEAD) -X github.com/men232/railway-manager/internal/version.BuildDate=$(date +%Y-%m-%d)" -o railway-manager ./cmd/railway-manager

func main() {
	app.Main()
}
