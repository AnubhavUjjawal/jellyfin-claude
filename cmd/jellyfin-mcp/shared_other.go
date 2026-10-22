//go:build !unix

package main

import "errors"

// Without Unix sockets and sessions each server keeps a player of its own.
func (a *app) newPlayer() playerAPI { return a.localPlayer() }

func spawnDaemon() error { return errors.New("the shared player needs a Unix system") }

func runPlayerDaemon() error { return errors.New("the shared player needs a Unix system") }
