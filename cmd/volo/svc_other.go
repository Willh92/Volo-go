//go:build !windows

package main

import (
	"context"
	"io"
)

func runWithServiceSupport(ctx context.Context, serveFn func(context.Context) int, _ io.Writer) int {
	return serveFn(ctx)
}
