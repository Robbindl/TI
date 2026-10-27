//go:build windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "WhatsAppTelegramBridge"

type bridgeService struct{}

func runWindowsService() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find service executable: %w", err)
	}
	if err := os.Chdir(filepath.Dir(executable)); err != nil {
		return fmt.Errorf("set service working directory: %w", err)
	}
	if err := loadServiceEnvironment(filepath.Join("data", "service.env")); err != nil {
		return err
	}

	logFile, err := os.OpenFile(filepath.Join("data", "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open service log: %w", err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))

	return svc.Run(windowsServiceName, &bridgeService{})
}

func (s *bridgeService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	accepts := svc.Accepted(svc.AcceptStop | svc.AcceptShutdown)
	status <- svc.Status{State: svc.StartPending, WaitHint: 10000}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runBridge(ctx)
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case err := <-done:
			if err != nil {
				log.Printf("bridge stopped: %v", err)
			}
			status <- svc.Status{State: svc.StopPending}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- svc.Status{State: svc.Running, Accepts: accepts}
			case svc.Stop, svc.Shutdown:
				cancel()
				status <- svc.Status{State: svc.StopPending, WaitHint: 10000}
				if err := <-done; err != nil {
					log.Printf("bridge stopped: %v", err)
				}
				return false, 0
			}
		}
	}
}

func loadServiceEnvironment(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open service environment %q: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("invalid service environment line")
		}
		os.Setenv(strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"`))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read service environment: %w", err)
	}
	return nil
}
