// Command api boots the ClaimOps Go Fiber edge service.
package main

import (
	"fmt"
	"log"

	"claimops-api/internal/app"
	"claimops-api/internal/config"
	"claimops-api/internal/observability"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	shutdown := observability.Init("claimops-api", cfg.OtelEnabled)
	defer shutdown()

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("claimops-api listening on %s (env=%s)", addr, cfg.AppEnv)
	if err := app.New().Listen(addr); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
