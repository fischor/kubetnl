package tunnel

import (
	"io"
	"log"
	"net"
	"sync"
)

// To create a new Tunnel call Setup.
type Tunnel struct {
	lis net.Listener

	// The address to forward the traffic from the remote to.
	// Format: <ip>:<port> or :<port>
	forward string

	doneCh chan struct{}
}

// Creates a new tunnel.
//
// You can close lis once the tunnel is done.
func New(lis net.Listener, target string) *Tunnel {
	return &Tunnel{
		lis:     lis,
		forward: target,
		doneCh:  make(chan struct{}),
	}
}

// TODO better logging (logrus)

// Open the tunnel.
//
// Note that in case the address passed to forward is invalid, no error will be
// returned. Each new incoming request will again try to establish a connection
// to the forward address.
func (t *Tunnel) Open() error {
	defer close(t.doneCh)

	// Waits for all connections handlers to finish.
	var handlers sync.WaitGroup

	// Loop until t.lis is closed.
	for {
		var conn net.Conn
		// t.lis.Accept waits for new connections. Unblocks with an err
		// if t.lis.Close is called. Earlier accepted connections can
		// still finish.
		conn, err := t.lis.Accept()
		if err != nil {
			// Any net package errors that are assured to be
			// retry-able will conform to the net.Error interface,
			// and return Temporary true.
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				log.Printf("tunnel temporary error lis.Accept: %v\n", err)
				continue
			}
			// Closing the underlying ssh connection will cause
			// t.lis.Accept to return EOF.
			log.Printf("tunnel fatal error lis.Accept: %v\n", err)
			t.lis.Close()
			handlers.Wait()
			return err
		}
		log.Println("tunnel lis accepted connection")

		// Handle connection.
		handlers.Add(1)
		go func() {
			e := t.handleConnection(conn) // ignoring returned error
			if e != nil {
				log.Printf("error handling connection: %v\n", e)
			}
			conn.Close()
			handlers.Done()
		}()
	}
}

func (t *Tunnel) handleConnection(conn net.Conn) error {
	// Open connection to tunnel target.
	targetConn, err := net.Dial("tcp", t.forward)
	if err != nil {
		log.Printf("tunnel net.Dial error: %v\n", err)
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		_, err := io.Copy(conn, targetConn)
		if err != nil {
			log.Printf("tunnel: conn -> targetConn error: %v", err)
		}
		wg.Done()
	}()
	go func() {
		_, err := io.Copy(targetConn, conn)
		if err != nil {
			log.Printf("tunnel: targetConn -> conn error: %v\n", err)
		}
		wg.Done()
	}()

	wg.Wait()

	err = targetConn.Close()
	if err != nil {
		log.Printf("tunnel: failed to close target connection %v", err)
	}

	log.Printf("tunnel: handeled connection from %s: successful", conn.RemoteAddr().String())
	return nil
}

// Close closes the tunnel. This will close the underlying listener.
func (t *Tunnel) Close() error {
	// Unblock t.lis.Accept. Earlier accepted connection can still finish.
	return t.lis.Close()
}

// Done returns a channel thats closed when Open exits.
func (t *Tunnel) Done() <-chan struct{} {
	return t.doneCh
}
