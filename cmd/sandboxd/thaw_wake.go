package main

import (
	"bytes"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

func isThawWakeFrame(frame []byte) bool {
	return len(frame) >= 14+len(agentapi.ThawWakeMagic) &&
		frame[12] == byte(agentapi.ThawWakeEtherType>>8) &&
		frame[13] == byte(agentapi.ThawWakeEtherType&0xff) &&
		bytes.Equal(frame[14:14+len(agentapi.ThawWakeMagic)], []byte(agentapi.ThawWakeMagic))
}
