// Command phonetopc-mock is a mock phonetopc phone: it serves the draft-0
// protocol on ws://ADDR/v1/ws with synthetic sensor data, so clients can be
// developed and tested without a phone.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

func main() {
	var (
		addr        = flag.String("addr", fmt.Sprintf("127.0.0.1:%d", protocol.DefaultPort), "listen address")
		tapEvery    = flag.Duration("tap-every", 10*time.Second, "interval of synthetic ui/tile_tap events (0 disables)")
		deviceID    = flag.String("device-id", "mock-0001", "device.id reported in hello and events")
		unavailable = flag.String("unavailable", "", "comma-separated sources to report as unavailable (e.g. light,proximity)")
		seed        = flag.Uint64("seed", 0, "random seed for the simulation (0 = random)")
		verbose     = flag.Bool("v", false, "log every frame")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: phonetopc-mock [flags]\n\nServes a mock phonetopc phone (protocol %s, draft 0).\n\n", protocol.Version)
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	opts := mockphone.Options{
		Device:   protocol.DeviceInfo{ID: *deviceID},
		Seed:     *seed,
		TapEvery: *tapEvery,
		Logf:     log.Printf,
	}
	if *tapEvery == 0 {
		opts.TapEvery = -1
	}
	for _, s := range strings.Split(*unavailable, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if _, ok := protocol.LookupSensor(s); !ok {
			log.Fatalf("--unavailable: %q is not in the catalog", s)
		}
		opts.Unavailable = append(opts.Unavailable, s)
	}
	if *verbose {
		opts.Tap = func(dir mockphone.Direction, frame []byte) { log.Printf("%s %s", dir, frame) }
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	mock := mockphone.New(opts)
	srv := &http.Server{Handler: mock.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("phonetopc-mock %s serving ws://%s%s (device %s)", mockphone.Version, ln.Addr(), protocol.WSPath, *deviceID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		log.Fatal(err)
	case <-ctx.Done():
	}
	log.Print("shutting down")
	mock.Close() // sends close frames to connected clients
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Print(err)
	}
}
