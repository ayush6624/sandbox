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

func TestPrimeGuestNetworkCommands(t *testing.T) {
	got, err := primeGuestNetworkCommands("br-fc", "fc7", "172.16.0.42", "02:AA:BB:CC:DD:EE")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"bridge", "fdb", "replace", "02:aa:bb:cc:dd:ee", "dev", "fc7", "master", "static"},
		{"ip", "neigh", "replace", "172.16.0.42", "lladdr", "02:aa:bb:cc:dd:ee", "nud", "reachable", "dev", "br-fc"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %#v, want %#v", got, want)
	}
}

func TestPrimeGuestNetworkCommandsRejectInvalidIdentity(t *testing.T) {
	if _, err := primeGuestNetworkCommands("br-fc", "fc7", "not-an-ip", "02:AA:BB:CC:DD:EE"); err == nil {
		t.Fatal("invalid IP accepted")
	}
	if _, err := primeGuestNetworkCommands("br-fc", "fc7", "172.16.0.42", "not-a-mac"); err == nil {
		t.Fatal("invalid MAC accepted")
	}
}

func TestParseNeighborMAC(t *testing.T) {
	got, err := parseNeighborMAC([]byte("172.16.0.42 dev br-fc lladdr 02:AA:BB:CC:DD:EE REACHABLE\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("MAC = %q", got)
	}
	if _, err := parseNeighborMAC([]byte("172.16.0.42 dev br-fc INCOMPLETE\n")); err == nil {
		t.Fatal("neighbor without lladdr accepted")
	}
}
