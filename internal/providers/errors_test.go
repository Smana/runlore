package providers

import (
	"errors"
	"fmt"
	"testing"
)

func TestPermanent(t *testing.T) {
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must be nil")
	}
	base := errors.New("boom")
	p := Permanent(base)
	if !IsPermanent(p) {
		t.Fatal("IsPermanent(Permanent(err)) = false")
	}
	if !errors.Is(p, base) {
		t.Fatal("Permanent must unwrap to the original error")
	}
	// Must survive %w wrapping — loop.go returns fmt.Errorf("model: %w", err).
	if !IsPermanent(fmt.Errorf("model: %w", p)) {
		t.Fatal("IsPermanent must see through %w wrapping")
	}
	if IsPermanent(base) {
		t.Fatal("a plain error must not be permanent")
	}
}
