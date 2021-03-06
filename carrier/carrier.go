//Package carrier provides a WebSocket proxy to carry or proxy a connection
//from the local client to the edge. See it as a wrapper around any protocol
//that it packages up in a WebSocket connection to the edge.
package carrier

import (
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/Andrewpk/cloudflared/cmd/cloudflared/token"
	"github.com/Andrewpk/cloudflared/h2mux"
	"github.com/Andrewpk/cloudflared/logger"
	"github.com/pkg/errors"
)

type StartOptions struct {
	OriginURL string
	Headers   http.Header
}

// Connection wraps up all the needed functions to forward over the tunnel
type Connection interface {
	// ServeStream is used to forward data from the client to the edge
	ServeStream(*StartOptions, io.ReadWriter) error

	// StartServer is used to listen for incoming connections from the edge to the origin
	StartServer(net.Listener, string, <-chan struct{}) error
}

// StdinoutStream is empty struct for wrapping stdin/stdout
// into a single ReadWriter
type StdinoutStream struct {
}

// Read will read from Stdin
func (c *StdinoutStream) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)

}

// Write will write to Stdout
func (c *StdinoutStream) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

// Helper to allow defering the response close with a check that the resp is not nil
func closeRespBody(resp *http.Response) {
	if resp != nil {
		resp.Body.Close()
	}
}

// StartForwarder will setup a listener on a specified address/port and then
// forward connections to the origin by calling `Serve()`.
func StartForwarder(conn Connection, address string, shutdownC <-chan struct{}, options *StartOptions) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.Wrap(err, "failed to start forwarding server")
	}
	return Serve(conn, listener, shutdownC, options)
}

// StartClient will copy the data from stdin/stdout over a WebSocket connection
// to the edge (originURL)
func StartClient(conn Connection, stream io.ReadWriter, options *StartOptions) error {
	return serveStream(conn, stream, options)
}

// Serve accepts incoming connections on the specified net.Listener.
// Each connection is handled in a new goroutine: its data is copied over a
// WebSocket connection to the edge (originURL).
// `Serve` always closes `listener`.
func Serve(remoteConn Connection, listener net.Listener, shutdownC <-chan struct{}, options *StartOptions) error {
	defer listener.Close()
	errChan := make(chan error)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				// don't block if parent goroutine quit early
				select {
				case errChan <- err:
				default:
				}
				return
			}
			go serveConnection(remoteConn, conn, options)
		}
	}()

	select {
	case <-shutdownC:
		return nil
	case err := <-errChan:
		return err
	}
}

// serveConnection handles connections for the Serve() call
func serveConnection(remoteConn Connection, c net.Conn, options *StartOptions) {
	defer c.Close()
	serveStream(remoteConn, c, options)
}

// serveStream will serve the data over the WebSocket stream
func serveStream(remoteConn Connection, conn io.ReadWriter, options *StartOptions) error {
	return remoteConn.ServeStream(options, conn)
}

// IsAccessResponse checks the http Response to see if the url location
// contains the Access structure.
func IsAccessResponse(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusFound {
		return false
	}

	location, err := resp.Location()
	if err != nil || location == nil {
		return false
	}
	if strings.HasPrefix(location.Path, "/cdn-cgi/access/login") {
		return true
	}

	return false
}

// BuildAccessRequest builds an HTTP request with the Access token set
func BuildAccessRequest(options *StartOptions, logger logger.Service) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, options.OriginURL, nil)
	if err != nil {
		return nil, err
	}

	token, err := token.FetchToken(req.URL, logger)
	if err != nil {
		return nil, err
	}

	// We need to create a new request as FetchToken will modify req (boo mutable)
	// as it has to follow redirect on the API and such, so here we init a new one
	originRequest, err := http.NewRequest(http.MethodGet, options.OriginURL, nil)
	if err != nil {
		return nil, err
	}
	originRequest.Header.Set(h2mux.CFAccessTokenHeader, token)

	for k, v := range options.Headers {
		if len(v) >= 1 {
			originRequest.Header.Set(k, v[0])
		}
	}

	return originRequest, nil
}
