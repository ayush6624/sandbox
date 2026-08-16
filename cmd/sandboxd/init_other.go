//go:build !linux

package main

func isInit() bool { return false }

func initMode() bool { return false }

func runInit() {}
