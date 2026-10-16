//go:build !linux

package main

import "io"

func flushInput(io.Reader) {}
