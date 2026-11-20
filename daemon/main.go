package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/emigcm98/meshnet-cni/daemon/cni"
	"github.com/emigcm98/meshnet-cni/daemon/meshnet"
	"github.com/emigcm98/meshnet-cni/daemon/vxlan"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	log "github.com/sirupsen/logrus"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := cni.Init(ctx); err != nil {
		log.Errorf("Failed to initialise CNI plugin: %v", err)
		os.Exit(1)
	}
	defer cni.Cleanup()

	isDebug := flag.Bool("d", false, "enable debugging")
	grpcPort, err := strconv.Atoi(os.Getenv("GRPC_PORT"))
	if err != nil || grpcPort == 0 {
		grpcPort = wireutil.GRPCDefaultPort
	}
	flag.Parse()
	log.SetLevel(log.InfoLevel)
	if *isDebug {
		log.SetLevel(log.DebugLevel)
		log.Debug("Verbose logging enabled")
	}

	meshnet.InitLogger()
	vxlan.InitLogger()

	m, err := meshnet.New(meshnet.Config{
		Port: grpcPort,
	})
	if err != nil {
		log.Errorf("failed to create meshnet: %v", err)
		os.Exit(1)
	}
	log.Info("Starting meshnet daemon...")

	go m.RunControllerLoop(ctx)

	// A SIGTERM from the kubelet stops the server so the deferred cleanup runs.
	go func() {
		<-ctx.Done()
		m.Stop()
	}()
	if err := m.Serve(); err != nil {
		log.Errorf("daemon exited badly: %v", err)
		os.Exit(1)
	}
}
