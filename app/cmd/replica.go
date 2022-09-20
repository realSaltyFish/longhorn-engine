package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/longhorn/longhorn-engine/pkg/backingfile"
	"github.com/longhorn/longhorn-engine/pkg/replica"
	replicarpc "github.com/longhorn/longhorn-engine/pkg/replica/rpc"
	"github.com/longhorn/longhorn-engine/pkg/types"
	"github.com/longhorn/longhorn-engine/pkg/util"
	diskutil "github.com/longhorn/longhorn-engine/pkg/util/disk"
	"github.com/longhorn/longhorn-engine/proto/ptypes"
)

func ReplicaCmd() cli.Command {
	return cli.Command{
		Name:      "replica",
		UsageText: "longhorn controller DIRECTORY SIZE",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "listen",
				Value: "localhost:9502",
			},
			cli.StringFlag{
				Name:  "backing-file",
				Usage: "qcow file or encapsulating directory to use as the base image of this disk",
			},
			cli.BoolTFlag{
				Name: "sync-agent",
			},
			cli.StringFlag{
				Name:  "size",
				Usage: "Volume size in bytes or human readable 42kb, 42mb, 42gb",
			},
			cli.StringFlag{
				Name:  "restore-from",
				Usage: "specify backup to be restored, must be used with --restore-name",
			},
			cli.StringFlag{
				Name:  "restore-name",
				Usage: "specify the snapshot name for restore, must be used with --restore-from",
			},
			cli.IntFlag{
				Name:  "sync-agent-port-count",
				Value: 10,
			},
			cli.BoolFlag{
				Name:   "disableRevCounter",
				Hidden: false,
				Usage:  "To disable revision counter for every write",
			},
			cli.StringFlag{
				Name:  "volume-name",
				Value: "",
			},
			cli.StringFlag{
				Name:  "data-server-protocol",
				Value: "tcp",
				Usage: "Specify the data-server protocol. Available options are \"tcp\" and \"unix\"",
			},
			cli.BoolFlag{
				Name:   "unmap-mark-disk-chain-removed",
				Hidden: false,
				Usage:  "To mark the current disk chain as removed before starting unmap",
			},
		},
		Action: func(c *cli.Context) {
			if err := startReplica(c); err != nil {
				logrus.WithError(err).Fatalf("Error running start replica command")
			}
		},
	}
}

func startReplica(c *cli.Context) error {
	if c.NArg() != 1 {
		return errors.New("directory name is required")
	}

	dir := c.Args()[0]
	backingFile, err := backingfile.OpenBackingFile(c.String("backing-file"))
	if err != nil {
		return err
	}

	disableRevCounter := c.Bool("disableRevCounter")
	unmapMarkDiskChainRemoved := c.Bool("unmap-mark-disk-chain-removed")

	s := replica.NewServer(dir, backingFile, diskutil.ReplicaSectorSize, disableRevCounter, unmapMarkDiskChainRemoved)

	address := c.String("listen")

	size := c.String("size")
	if size != "" {
		size, err := units.RAMInBytes(size)
		if err != nil {
			return err
		}

		if err := s.Create(size); err != nil {
			return err
		}
	}

	volumeName := c.String("volume-name")
	dataServerProtocol := c.String("data-server-protocol")

	controlAddress, dataAddress, syncAddress, syncPort, err :=
		util.GetAddresses(volumeName, address, types.DataServerProtocol(dataServerProtocol))
	if err != nil {
		return err
	}

	resp := make(chan error)

	go func() {
		listen, err := net.Listen("tcp", controlAddress)
		if err != nil {
			logrus.Warnf("Failed to listen %v: %v", controlAddress, err)
			resp <- err
			return
		}

		server := grpc.NewServer()
		rs := replicarpc.NewReplicaServer(s)
		ptypes.RegisterReplicaServiceServer(server, rs)
		healthpb.RegisterHealthServer(server, replicarpc.NewReplicaHealthCheckServer(rs))
		reflection.Register(server)

		logrus.Infof("Listening on gRPC Replica server %s", controlAddress)
		err = server.Serve(listen)
		logrus.Warnf("gRPC Replica server at %v is down: %v", controlAddress, err)
		resp <- err
	}()

	go func() {
		rpcServer := replicarpc.NewDataServer(types.DataServerProtocol(dataServerProtocol), dataAddress, s)
		logrus.Infof("Listening on data server %s", dataAddress)
		err := rpcServer.ListenAndServe()
		logrus.Warnf("Replica rest server at %v is down: %v", dataAddress, err)
		resp <- err
	}()

	if c.Bool("sync-agent") {
		exe, err := exec.LookPath(os.Args[0])
		if err != nil {
			return err
		}

		exe, err = filepath.Abs(exe)
		if err != nil {
			return err
		}

		go func() {
			cmd := exec.Command(exe, "sync-agent", "--listen", syncAddress,
				"--replica", controlAddress,
				"--listen-port-range",
				fmt.Sprintf("%v-%v", syncPort+1, syncPort+c.Int("sync-agent-port-count")))
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Pdeathsig: syscall.SIGKILL,
			}
			cmd.Dir = dir
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			logrus.Infof("Listening on sync agent server %s", syncAddress)
			err := cmd.Run()
			logrus.Warnf("Replica sync agent at %v is down: %v", syncAddress, err)
			resp <- err
		}()
	}

	// empty shutdown hook for signal message
	addShutdown(func() (err error) { return nil })

	return <-resp
}
