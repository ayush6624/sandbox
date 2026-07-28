package main

import (
	"testing"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

func TestIsThawWakeFrame(t *testing.T) {
	frame := make([]byte, 14+len(agentapi.ThawWakeMagic))
	frame[12] = byte(agentapi.ThawWakeEtherType >> 8)
	frame[13] = byte(agentapi.ThawWakeEtherType & 0xff)
	copy(frame[14:], agentapi.ThawWakeMagic)
	if !isThawWakeFrame(frame) {
		t.Fatal("valid thaw frame rejected")
	}

	frame[13] ^= 1
	if isThawWakeFrame(frame) {
		t.Fatal("wrong EtherType accepted")
	}
	if isThawWakeFrame([]byte(agentapi.ThawWakeMagic)) {
		t.Fatal("short frame accepted")
	}
}
