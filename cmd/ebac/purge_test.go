package main

import (
	"errors"
	"testing"
)

func TestPurgeRefusedRecognisesOnlyTheAuthzRefusal(t *testing.T) {
	refused := errors.New(`jsonrpc error -32020: figaro.kill: user:ebac holds ebac-interim; none grants kill here`)
	other := errors.New(`jsonrpc error -32000: attach @x: aria @x not found`)
	if !purgeRefused(refused) {
		t.Fatal("authz refusal not recognised")
	}
	if purgeRefused(other) {
		t.Fatal("an ordinary failure was taken for an authz refusal") // control
	}
}
