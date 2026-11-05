package figaro

import (
	"errors"
	"testing"
)

func TestIsRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New(`jsonrpc error -32000: fork "@x": sandbox "local": user:ebac holds ebac-interim; none grants use here`), true},
		{errors.New(`jsonrpc error -32020: figaro.kill: user:ebac holds ebac-interim; none grants kill here`), true},
		{errors.New(`jsonrpc error -32000: attach @x: aria @x not found`), false}, // control: an ordinary failure
		{nil, false},
	} {
		if got := IsRefused(c.err); got != c.want {
			t.Errorf("IsRefused(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
