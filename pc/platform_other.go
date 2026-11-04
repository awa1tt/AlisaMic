//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func openURL(url string) error          { return errors.New("только в Windows") }
func firewallRuleExists() (bool, error) { return false, nil }
func addFirewallRule() error            { return errors.New("только в Windows") }

func hideChild(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
