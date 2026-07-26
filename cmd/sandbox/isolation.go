package main

import (
	"fmt"

	"github.com/ayush6624/sandbox/internal/config"
	"github.com/ayush6624/sandbox/internal/vm"
)

func jailerConfigFrom(cfg *config.Config) *vm.JailerConfig {
	if cfg == nil || cfg.VMIsolation != "jailer" {
		return nil
	}
	return &vm.JailerConfig{
		JailerBin:         cfg.JailerBin,
		ChrootBaseDir:     cfg.JailerChrootBase,
		UIDStart:          cfg.JailerUIDStart,
		GIDStart:          cfg.JailerGIDStart,
		IdentityCount:     cfg.JailerIdentityCount,
		MemoryOverheadMIB: cfg.JailerMemoryOverheadMIB,
		PIDsMax:           cfg.JailerPIDsMax,
		CPUWeight:         cfg.JailerCPUWeight,
		CPUPeriodUS:       cfg.JailerCPUPeriodUS,
		IOReadBPS:         cfg.JailerIOReadBPS,
		IOWriteBPS:        cfg.JailerIOWriteBPS,
		NoFile:            cfg.JailerNoFile,
		FileSize:          cfg.JailerFileSize,
	}
}

func checkJailerPrerequisites(cfg *config.Config) (string, error) {
	jailer := jailerConfigFrom(cfg)
	if jailer == nil {
		return "", fmt.Errorf("direct VMM execution is development-only; set vm_isolation to jailer for production")
	}
	return vm.CheckJailerPrerequisites(*jailer, cfg.FirecrackerBin, cfg.RootfsBase, cfg.RootfsDir, cfg.SnapshotDir)
}
