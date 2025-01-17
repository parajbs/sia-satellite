package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/mike76-dev/sia-satellite/modules"

	smodules "go.sia.tech/siad/modules"
)

const (
	// StatusModuleNotLoaded is a custom http code to indicate that a module
	// wasn't yet loaded by the Daemon and can therefore not be reached.
	StatusModuleNotLoaded = 490
)

// ErrAPICallNotRecognized is returned by API client calls made to modules that
// are not yet loaded.
var ErrAPICallNotRecognized = errors.New("API call not recognized")

// Error is a type that is encoded as JSON and returned in an API response in
// the event of an error. Only the Message field is required. More fields may
// be added to this struct in the future for better error reporting.
type Error struct {
	// Message describes the error in English. Typically it is set to
	// `err.Error()`. This field is required.
	Message string `json:"message"`
}

// Error implements the error interface for the Error type. It returns only the
// Message field.
func (err Error) Error() string {
	return err.Message
}

// HttpGET is a utility function for making http get requests with a
// whitelisted user-agent. A non-2xx response does not return an error.
func HttpGET(url string) (resp *http.Response, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Sat-Agent")
	return http.DefaultClient.Do(req)
}

// HttpGETAuthenticated is a utility function for making authenticated http get
// requests with a whitelisted user-agent and the supplied password. A
// non-2xx response does not return an error.
func HttpGETAuthenticated(url string, password string) (resp *http.Response, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Sat-Agent")
	req.SetBasicAuth("", password)
	return http.DefaultClient.Do(req)
}

// HttpPOST is a utility function for making post requests with a
// whitelisted user-agent. A non-2xx response does not return an error.
func HttpPOST(url string, data string) (resp *http.Response, err error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Sat-Agent")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return http.DefaultClient.Do(req)
}

// HttpPOSTAuthenticated is a utility function for making authenticated http
// post requests with a whitelisted user-agent and the supplied
// password. A non-2xx response does not return an error.
func HttpPOSTAuthenticated(url string, data string, password string) (resp *http.Response, err error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Sat-Agent")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("", password)
	return http.DefaultClient.Do(req)
}

type (
	// API encapsulates a collection of modules and implements a http.Handler
	// to access their methods.
	API struct {
		cs                smodules.ConsensusSet
		gateway           smodules.Gateway
		portal            modules.Portal
		satellite         modules.Satellite
		tpool             smodules.TransactionPool
		wallet            smodules.Wallet

		router            http.Handler
		routerMu          sync.RWMutex

		requiredUserAgent string
		requiredPassword  string
		modulesSet        bool
		Shutdown          func() error
	}
)

// ServeHTTP implements the http.Handler interface.
func (api *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.routerMu.RLock()
	api.router.ServeHTTP(w, r)
	api.routerMu.RUnlock()
}

// SetModules allows for replacing the modules in the API at runtime.
func (api *API) SetModules(cs smodules.ConsensusSet, g smodules.Gateway, p modules.Portal, s modules.Satellite, tp smodules.TransactionPool, w smodules.Wallet) {
	if api.modulesSet {
		log.Fatal("can't call SetModules more than once")
	}
	api.cs = cs
	api.gateway = g
	api.portal = p
	api.satellite = s
	api.tpool = tp
	api.wallet = w
	api.modulesSet = true
	api.buildHTTPRoutes()
}

// New creates a new API. The API will require authentication using HTTP basic
// auth for certain endpoints if the supplied password is not the empty string.
// Usernames are ignored for authentication.
func New(requiredUserAgent string, requiredPassword string, cs smodules.ConsensusSet, g smodules.Gateway, p modules.Portal, s modules.Satellite, tp smodules.TransactionPool, w smodules.Wallet) *API {
	api := &API{
		cs:                cs,
		gateway:           g,
		portal:            p,
		satellite:         s,
		tpool:             tp,
		wallet:            w,
		requiredUserAgent: requiredUserAgent,
		requiredPassword:  requiredPassword,
	}

	// Register API handlers
	api.buildHTTPRoutes()

	return api
}

// UnrecognizedCallHandler handles calls to not-loaded modules.
func (api *API) UnrecognizedCallHandler(w http.ResponseWriter, _ *http.Request) {
	var errStr string
	errStr = fmt.Sprintf("%d Module not loaded", StatusModuleNotLoaded)
	WriteError(w, Error{errStr}, StatusModuleNotLoaded)
}

// WriteError writes an error to the API caller.
func WriteError(w http.ResponseWriter, err Error, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	encodingErr := json.NewEncoder(w).Encode(err)
	if _, isJsonErr := encodingErr.(*json.SyntaxError); isJsonErr {
		// Marshalling should only fail in the event of a developer error.
		// Specifically, only non-marshallable types should cause an error here.
		log.Fatal("failed to encode API error response:", encodingErr)
	}
}

// WriteJSON writes the object to the ResponseWriter. If the encoding fails, an
// error is written instead. The Content-Type of the response header is set
// accordingly.
func WriteJSON(w http.ResponseWriter, obj interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err := json.NewEncoder(w).Encode(obj)
	if _, isJsonErr := err.(*json.SyntaxError); isJsonErr {
		// Marshalling should only fail in the event of a developer error.
		// Specifically, only non-marshallable types should cause an error here.
		log.Fatal("failed to encode API response:", err)
	}
}

// WriteSuccess writes the HTTP header with status 204 No Content to the
// ResponseWriter. WriteSuccess should only be used to indicate that the
// requested action succeeded AND there is no data to return.
func WriteSuccess(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}
