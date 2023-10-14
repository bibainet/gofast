package gofast

import (
	"bytes"
	"log"
	"net/http"
)

// Handler is an alias to http.Handler
type Handler = http.Handler

// NewHandler returns the default Handler implementation. This default Handler
// act as the "web server" component in fastcgi specification, which connects
// fastcgi "application" through the network/address and passthrough I/O as
// specified.
func NewHandler(sessionHandler SessionHandler, clientFactory ClientFactory) Handler {
	return &defaultHandler {
		sessionHandler: sessionHandler,
		newClient:      clientFactory,
	}
}

// defaultHandler implements Handler
type defaultHandler struct {
	sessionHandler SessionHandler
	newClient      ClientFactory
}

// ServeHTTP implements http.Handler
func (h *defaultHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TODO: separate dial logic to pool client / connection
	c, err := h.newClient()
	if err != nil {
		http.Error(w, "Failed to connect to FastCGI backend", http.StatusBadGateway)
		log.Println("# FastCGI: failed to connect to backend:", err)
		return
	}
	// defer closing with error reporting
	defer func() {
		// signal to close the client
		// or the pool to return the client
		if err = c.Close(); err != nil {
			log.Println("# FastCGI: error closing client:", err)
		}
	}()
	// handle the session
	resp, err := h.sessionHandler(c, NewRequest(r))
	if err != nil {
		http.Error(w, "Failed to process FastCGI request", http.StatusInternalServerError)
		log.Println("# FastCGI: failed to process request:", err)
		return
	}
	ew := new(bytes.Buffer)
	if err = resp.WriteTo(w, ew); err != nil {
		log.Println("# FastCGI: backend stream error:", err)
	}
	if ew.Len() > 0 {
		log.Println("# FastCGI: backend stream error:", ew.String())
	}
}
