package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
)

const (
	// httpServerTimeout defines the maximum amount of time before an HTTP call
	// will timeout and an error will be returned.
	httpServerTimeout = 24 * time.Hour
)

// buildHttpRoutes sets up and returns an * httprouter.Router.
// it connected the Router to the given api using the required
// parameters: requiredUserAgent and requiredPassword.
func (api *API) buildHTTPRoutes() {
	router := httprouter.New()
	requiredPassword := api.requiredPassword
	requiredUserAgent := api.requiredUserAgent

	router.NotFound = http.HandlerFunc(api.UnrecognizedCallHandler)
	router.RedirectTrailingSlash = false

	// Daemon API Calls.
	router.GET("/daemon/alerts", api.daemonAlertsHandlerGET)
	router.GET("/daemon/stop", RequirePassword(api.daemonStopHandler, requiredPassword))
	router.GET("/daemon/version", api.daemonVersionHandler)

	// Consensus API Calls.
	if api.cs != nil {
		RegisterRoutesConsensus(router, api.cs)
	}

	// Gateway API Calls.
	if api.gateway != nil {
		RegisterRoutesGateway(router, api.gateway, requiredPassword)
	}

	// Transaction pool API Calls.
	if api.tpool != nil {
		RegisterRoutesTransactionPool(router, api.tpool)
	}

	// Wallet API Calls.
	if api.wallet != nil {
		RegisterRoutesWallet(router, api.wallet, requiredPassword)
	}

	// HostDB API Calls.
	if api.satellite != nil {
		router.GET("/hostdb", api.hostdbHandler)
		router.GET("/hostdb/active", api.hostdbActiveHandler)
		router.GET("/hostdb/all", api.hostdbAllHandler)
		router.GET("/hostdb/hosts/:pubkey", api.hostdbHostsHandler)
		router.GET("/hostdb/filtermode", api.hostdbFilterModeHandlerGET)
		router.POST("/hostdb/filtermode", RequirePassword(api.hostdbFilterModeHandlerPOST, requiredPassword))
	}

	// Satellite API Calls.
	if api.satellite != nil {
		router.GET("/satellite/renters", RequirePassword(api.satelliteRentersHandlerGET, requiredPassword))
		router.GET("/satellite/renter/:publickey", RequirePassword(api.satelliteRenterHandlerGET, requiredPassword))
		router.GET("/satellite/balance/:publickey", RequirePassword(api.satelliteBalanceHandlerGET, requiredPassword))
		router.GET("/satellite/contracts", RequirePassword(api.satelliteContractsHandlerGET, requiredPassword))
		router.GET("/satellite/contracts/:publickey", RequirePassword(api.satelliteContractsHandlerGET, requiredPassword))
	}

	// Apply UserAgent middleware and return the Router.
	api.routerMu.Lock()
	api.router = timeoutHandler(RequireUserAgent(router, requiredUserAgent), httpServerTimeout)
	api.routerMu.Unlock()
	return
}

// timeoutHandler is a middleware that enforces a specific timeout on the route
// by closing the context after the httpServerTimeout.
func timeoutHandler(h http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Create a new context with timeout.
		ctx, cancel := context.WithTimeout(req.Context(), httpServerTimeout)
		defer cancel()

		// Add the new context to the request and call the handler.
		h.ServeHTTP(w, req.WithContext(ctx))
	})
}

// RequireUserAgent is middleware that requires all requests to set a
// UserAgent that contains the specified string.
func RequireUserAgent(h http.Handler, ua string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.UserAgent(), ua) {
			WriteError(w, Error{"Browser access disabled due to security vulnerability."},
				http.StatusBadRequest)
			return
		}
		h.ServeHTTP(w, req)
	})
}

// RequirePassword is middleware that requires a request to authenticate with a
// password using HTTP basic auth. Usernames are ignored. Empty passwords
// indicate no authentication is required.
func RequirePassword(h httprouter.Handle, password string) httprouter.Handle {
	// An empty password is equivalent to no password.
	if password == "" {
		return h
	}
	return func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		_, pass, ok := req.BasicAuth()
		if !ok || pass != password {
			w.Header().Set("WWW-Authenticate", "Basic realm=\"SatAPI\"")
			WriteError(w, Error{"API authentication failed."}, http.StatusUnauthorized)
			return
		}
		h(w, req, ps)
	}
}
