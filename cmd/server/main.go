package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"

	"github.com/fischor/dew/internal/port"
	"github.com/spf13/cobra"
)

type ServerOptions struct {
	// RawPorts in the format <number>/<protocol>, e.g. 8080/tcp.
	RawPort string

	// Parsed ports.
	Port port.Port
}

var (
	serverLong    = ""
	serverExample = ""
)

var (
	flog *log.Logger
)

func main() {
	// Since stdout is used to pipe to connection and stderr is used to
	// communicate errors back to the client, logs are written to a file.
	// The container command tails this logfile such that the logs for all
	// created "dew-server" instances appear in the main container logs.
	logfile, err := os.OpenFile("/var/log/dew.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		// Report error back to the dew cli program.
		log.Fatalf(`could not open logfile: %v.
This is an error and should not have happend. 
Please report an issue to: ...\n`, err)
	}
	flog = log.New(logfile, "dew-server", log.LstdFlags)
	flog.Println("hello")

	cmd := NewServerCommand()
	err = cmd.Execute()
	if err != nil {
		// Report error back to the dew cli program.
		flog.Printf("error executing dew-server command: %v\n", err)
		log.Fatalf("error executing dew-server command: %v\n", err)
	}
}

// NewServerCommand creates a new server command.
//
// The command line interface looks as follow:
//
// 	dew-server PORT
//
// where ports are in the format <number>/<protocol>.
func NewServerCommand() *cobra.Command {
	opts := NewServerOptions()

	cmd := &cobra.Command{
		Use:   "dew-server",
		Short: "Pipes traffic to stdout",
		Long:  serverLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Setup stop channel. Notifies the server when to stop
			// serving. Is closed when a os.Interrupt is received.
			stopCh := make(chan struct{})
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			go func() {
				<-sig
				close(stopCh)
			}()

			err := opts.Complete(args)
			if err != nil {
				return err
			}
			return Run(opts, stopCh)
		},
	}

	// cmd.Flags().IntSliceVar(&opts.Ports, "ports", nil, "The ports to pipe")

	return cmd
}

func NewServerOptions() *ServerOptions {
	return &ServerOptions{}
}

func (o *ServerOptions) Complete(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("specify args please")
	}
	if len(args) > 1 {
		return fmt.Errorf("specify one arg please")
	}
	var err error
	o.Port, err = port.ParsePort(args[0])
	return err
}

func Run(completedOptions *ServerOptions, stopCh <-chan struct{}) error {
	addr := fmt.Sprintf("0.0.0.0:%d", completedOptions.Port.Number)
	lis, err := net.Listen(completedOptions.Port.Protocol.String(), addr)
	if err != nil {
		return err
	}
	flog.Printf("dew-server %s called\n", completedOptions.Port.String())
	flog.Printf("dew-server listens on %s\n", addr)

	// Graceful shutdown.
	go func() {
		<-stopCh
		err := lis.Close()
		if err != nil {
			flog.Printf("error lis.Close: %v", err)
		}
		// TODO: this never gets cleaned up if lis.Accept erros
		// without being stopped
	}()

	for {
		conn, err := lis.Accept()
		if err != nil {
			if err == net.ErrClosed {
				break
			}
			flog.Printf("error lis.Accept: %v\n", err)
			continue
		}
		// TODO: conn.Close()
		flog.Printf("new connection %s\n", conn.LocalAddr().String())
		go handleConnection(conn)
	}

	return nil
}

func handleConnection(conn net.Conn) {
	go func() {
		// Writes incoming traffic from conn to stdout.
		w, err := io.Copy(os.Stdout, conn)
		if err != nil {
			flog.Printf("io.Copy error: %v\n", err)
		}
		flog.Printf("io.Copy stdout (dew) -> conn (k8s) success: %d bytes copied\n", w)
	}()
	go func() {
		// Write incoming traffic from stdin to conn.
		w, err := io.Copy(conn, os.Stdin)
		if err != nil {
			flog.Printf("io.Copy error: %v\n", err)
		}
		flog.Printf("io.Copy conn (k8s) -> os.Stdin (dew) success: %d bytes copied\n", w)
	}()
}
