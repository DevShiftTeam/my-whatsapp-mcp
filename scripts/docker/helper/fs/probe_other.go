//go:build !linux

package main

import "io/fs"

func probe() error { return fs.ErrInvalid }
