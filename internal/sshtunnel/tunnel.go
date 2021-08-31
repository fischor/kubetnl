package sshtunnel

import (
	"golang.org/x/crypto/ssh"
	"io"
	"log"
	"net"
	"sync"
)

// To create a new Tunnel call Setup.
type Tunnel struct {
	// The underlying SSH client. The tunnel is set up over a new SSH
	// channel using SSH remote port forwarding
	client *ssh.Client

	// The remote address to listen to for incoming TCP connections.
	// Format: <ip>:<port> or :<port>
	remote string

	// The address to forward the traffic from the remote to.
	// Format: <ip>:<port> or :<port>
	forward string

	lis net.Listener

	stopCh chan struct{}

	doneCh chan struct{}
}

// TODO call open
// NewTunnel creates a new Tunnel from remote to local.
func Setup(client *ssh.Client, remote, forward string) (*Tunnel, error) {
	// Request remote server to open ports.
	// Listen requests the remote peer open a listening socket on addr.
	// Incoming connections will be available by calling Accept on the
	// returned net.Listener. The listener must be serviced, or the SSH
	// connection may hang.
	var err error
	lis, err := client.Listen("tcp", remote)
	if err != nil {
		// Failed to open port on remote
		return nil, err
	}
	stopCh := make(chan struct{})
	go func() {
		<-stopCh
		lis.Close()
	}()

	t := &Tunnel{
		client:  client,
		remote:  remote,
		forward: forward,
		lis:     lis,
		stopCh:  stopCh,
		doneCh:  make(chan struct{}),
	}
	return t, nil
}

// TODO better logging (logrus)

// Run tunnels connections.
//
// Note that in case the address passed to forward is invalid, no error will be
// returned. Each new incoming request will again try to establish a connection
// to the forward address.
func (t *Tunnel) Run() error {
	defer close(t.doneCh)
	// Loop until t.lis is closed.
	for {
		log.Println("tunnel lis is waiting..")
		// TODO: kickoff handling the connection to a new
		// goroutine? As of now connections are handled sequentially.
		conn, err := t.lis.Accept()
		if err != nil {
			// Any net package errors that are assured to be
			// retry-able will conform to the net.Error interface,
			// and return Temporary true.
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				log.Printf("tunnel temporary error lis.Accept: %v\n", err)
				continue
			}
			log.Printf("tunnel fatal error lis.Accept: %v\n", err)
			return err
		}
		log.Println("tunnel lis accepted connection")

		// Handle connection.
		// Open connection to tunnel target.
		targetConn, err := net.Dial("tcp", t.forward)
		if err != nil {
			log.Printf("tunnel net.Dial error: %v\n", err)
			conn.Close() // TODO check the resulting error messages
			continue
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { io.Copy(conn, targetConn); log.Print("tunnel: conn -> targetConn done"); wg.Done() }()
		go func() { io.Copy(targetConn, conn); log.Print("tunnel: targetConn -> conn done"); wg.Done() }()
		// TODO: handle hard interrupts (closing the conns) in a third goroutine)
		// TODO: handle errors from io.Copy: espacially take care if whe
		wg.Wait()
		log.Printf("tunnel: handeled connection from %s: successful", conn.RemoteAddr().String())
	}
}

func (t *Tunnel) Stop() {
	close(t.stopCh)
}

func (t *Tunnel) Done() <-chan struct{} {
	return t.doneCh
}
