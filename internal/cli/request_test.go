package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/yaojingang/geoflow-updater/internal/cli"
	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"github.com/yaojingang/geoflow-updater/internal/operation"
	"testing"
)

type requestReader struct {
	*operation.Manager
	receipt coordination.Receipt
	queries int
}

func (r *requestReader) Request(id, request string) (coordination.Receipt, error) {
	r.queries++
	return r.receipt, nil
}
func TestRequestCommandReadsReceiptAndReturnsHeldOrFailure(t *testing.T) {
	for _, test := range []struct {
		background, status string
		exit               int
	}{{"ready", "succeeded", 0}, {"held", "succeeded", 1}, {"ready", "rolled_back", 1}, {"ready", "recovery_required", 1}, {"ready", "running", 0}} {
		t.Run(test.background+test.status, func(t *testing.T) {
			reader := &requestReader{receipt: coordination.Receipt{SchemaVersion: 2, ClientRequestID: "request-0001", BackgroundStatus: test.background, Operation: json.RawMessage(`{"status":"` + test.status + `"}`)}}
			var out, err bytes.Buffer
			exit := (cli.App{Operations: reader, Stdout: &out, Stderr: &err}).Run(context.Background(), []string{"request", "request-0001", "--json"})
			if exit != test.exit || reader.queries != 1 || out.Len() == 0 {
				t.Fatal(exit, out.String(), err.String())
			}
		})
	}
}
