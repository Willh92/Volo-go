//go:build windows

package main

import (
	"context"
	"fmt"
	"io"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "Volo"

type voloWindowsService struct {
	parentCtx context.Context
	serveFn   func(context.Context) int
	exitCode  int
}

func (s *voloWindowsService) Execute(
	_ []string,
	req <-chan svc.ChangeRequest,
	changes chan<- svc.Status,
) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	srvCtx, cancelSrv := context.WithCancel(s.parentCtx)
	defer cancelSrv()

	doneCh := make(chan int, 1)
	go func() {
		doneCh <- s.serveFn(srvCtx)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for {
		select {
		case code := <-doneCh:
			s.exitCode = code
			changes <- svc.Status{State: svc.StopPending}
			if code != 0 {
				return false, uint32(code)
			}
			return false, 0

		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancelSrv()
				code := <-doneCh
				s.exitCode = code
				if code != 0 {
					return false, uint32(code)
				}
				return false, 0
			default:
			}
		}
	}
}

func runWithServiceSupport(ctx context.Context, serveFn func(context.Context) int, stderr io.Writer) int {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "volo serve: windows service check failed: %v\n", err)
		}
		return serveFn(ctx)
	}
	if !isSvc {
		return serveFn(ctx)
	}

	handler := &voloWindowsService{
		parentCtx: ctx,
		serveFn:   serveFn,
	}
	if err := svc.Run(windowsServiceName, handler); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "volo serve: windows service failed: %v\n", err)
		}
		if handler.exitCode != 0 {
			return handler.exitCode
		}
		return 1
	}
	return handler.exitCode
}
