package admin

import "testing"

func TestGoAPIProviderMapping(t *testing.T) {
	if driver, ok := DriverFor("goapi"); !ok || driver != "GoAPI" {
		t.Fatalf("GoAPI static mapping = %q, %v", driver, ok)
	}
	if provider := ProviderForDriver("GoAPI"); provider != "goapi" {
		t.Fatalf("GoAPI adopted provider = %q", provider)
	}
}
