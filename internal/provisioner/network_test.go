package provisioner

import (
	"reflect"
	"testing"
)

func TestInterGuestRuleDefaultsToDrop(t *testing.T) {
	got := interGuestRule("br-fc", false)
	want := []string{"FORWARD", "-i", "br-fc", "-o", "br-fc", "-j", "DROP"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rule = %v, want %v", got, want)
	}
}

func TestInterGuestRuleCompatibilityOptIn(t *testing.T) {
	got := interGuestRule("br-fc", true)
	want := []string{"FORWARD", "-i", "br-fc", "-o", "br-fc", "-j", "ACCEPT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rule = %v, want %v", got, want)
	}
}
