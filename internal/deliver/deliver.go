// Package deliver carries a message to one endpoint. Ingest enqueues
// one job per delivery and this package's worker executes them.
package deliver

import "github.com/google/uuid"

// Args is what an enqueued delivery job carries: the identifier of the
// delivery it is for, and nothing else. The worker re-reads the
// delivery, its endpoint and its message when it runs, so a URL changed
// or an endpoint disabled between enqueue and execution is honored
// rather than frozen into the job at the moment it was created.
type Args struct {
	DeliveryID uuid.UUID `json:"delivery_id"`
}

// Kind names the job type the queue dispatches on.
func (Args) Kind() string { return "webhook_delivery" }
