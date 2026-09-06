// Package observability bootstraps tracing. Phase 0: stub only —
// records whether OTEL is enabled so later phases can attach an
// exporter without changing call sites. No network activity here.
package observability

import "log"

// Init announces the tracing mode. Exporter wiring arrives later.
func Init(service string, enabled bool) func() {
	if !enabled {
		log.Printf("[otel] disabled for %s", service)
		return func() {}
	}
	log.Printf("[otel] enabled for %s (exporter wiring pending)", service)
	return func() {}
}
