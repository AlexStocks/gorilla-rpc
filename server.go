// Copyright 2009 The Go Authors. All rights reserved.
// Copyright 2012 The Gorilla Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rpc

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"reflect"
	"strings"
)

// ----------------------------------------------------------------------------
// Codec
// ----------------------------------------------------------------------------

// Codec creates a CodecRequest to process each request.
type Codec interface {
	NewRequest(rawReqString []byte, err error) CodecRequest
}

// CodecRequest decodes a request and encodes a response using a specific
// serialization scheme.
type CodecRequest interface {
	// Reads request and returns the RPC method name.
	Method() (string, error)
	// Reads request filling the RPC method args.
	ReadRequest(interface{}) error
	// Writes response using the RPC method reply. The error parameter is
	// the error returned by the method call, if any.
	WriteResponse(http.ResponseWriter, interface{}, error) error
}

// ----------------------------------------------------------------------------
// Server
// ----------------------------------------------------------------------------

// NewServer returns a new RPC server.
func NewServer() *Server {
	return &Server{
		codecs:   make(map[string]Codec),
		services: new(serviceMap),
	}
}

// RequestInfo contains all the information we pass to before/after functions
type RequestInfo struct {
	Method     string
	Error      error
	Request    *http.Request
	StatusCode int
}

// Server serves registered RPC services using registered codecs.
type Server struct {
	codecs        map[string]Codec
	services      *serviceMap
	interceptFunc func(i *RequestInfo) *http.Request
	beforeFunc    func(i *RequestInfo)
	afterFunc     func(i *RequestInfo)
}

// RegisterCodec adds a new codec to the server.
//
// Codecs are defined to process a given serialization scheme, e.g., JSON or
// XML. A codec is chosen based on the "Content-Type" header from the request,
// excluding the charset definition.
func (s *Server) RegisterCodec(codec Codec, contentType string) {
	s.codecs[strings.ToLower(contentType)] = codec
}

// RegisterService adds a new service to the server.
//
// The name parameter is optional: if empty it will be inferred from
// the receiver type name.
//
// Methods from the receiver will be extracted if these rules are satisfied:
//
//    - The receiver is exported (begins with an upper case letter) or local
//      (defined in the package registering the service).
//    - The method name is exported.
//    - The method has three arguments: *http.Request, *args, *reply.
//    - All three arguments are pointers.
//    - The second and third arguments are exported or local.
//    - The method has return type error.
//
// All other methods are ignored.
func (s *Server) RegisterService(receiver interface{}, name string) error {
	return s.services.register(receiver, name, true)
}

// RegisterTCPService adds a new TCP service to the server.
// No HTTP request struct will be passed to the service methods.
//
// The name parameter is optional: if empty it will be inferred from
// the receiver type name.
//
// Methods from the receiver will be extracted if these rules are satisfied:
//
//    - The receiver is exported (begins with an upper case letter) or local
//      (defined in the package registering the service).
//    - The method name is exported.
//    - The method has two arguments: *args, *reply.
//    - Both arguments are pointers.
//    - Both arguments are exported or local.
//    - The method has return type error.
//
// All other methods are ignored.
func (s *Server) RegisterTCPService(receiver interface{}, name string) error {
	return s.services.register(receiver, name, false)
}

// HasMethod returns true if the given method is registered.
//
// The method uses a dotted notation as in "Service.Method".
func (s *Server) HasMethod(method string) bool {
	fmt.Printf("RRRR hello2\n")
	if _, _, err := s.services.get(method); err == nil {
		return true
	}
	return false
}

// RegisterInterceptFunc registers the specified function as the function
// that will be called before every request. The function is allowed to intercept
// the request e.g. add values to the context.
//
// Note: Only one function can be registered, subsequent calls to this
// method will overwrite all the previous functions.
func (s *Server) RegisterInterceptFunc(f func(i *RequestInfo) *http.Request) {
	s.interceptFunc = f
}

// RegisterBeforeFunc registers the specified function as the function
// that will be called before every request.
//
// Note: Only one function can be registered, subsequent calls to this
// method will overwrite all the previous functions.
func (s *Server) RegisterBeforeFunc(f func(i *RequestInfo)) {
	s.beforeFunc = f
}

// RegisterAfterFunc registers the specified function as the function
// that will be called after every request
//
// Note: Only one function can be registered, subsequent calls to this
// method will overwrite all the previous functions.
func (s *Server) RegisterAfterFunc(f func(i *RequestInfo)) {
	s.afterFunc = f
}

func (s *Server) Serve(r *http.Request, codecReq CodecRequest) (errCode int, errString string, errValue []reflect.Value, reply reflect.Value) {
	// Get service method to be called.
	method, errMethod := codecReq.Method()
	if errMethod != nil {
		errCode = 400
		errString = errMethod.Error()
		return
	}
	serviceSpec, methodSpec, errGet := s.services.get(method)
	if errGet != nil {
		errCode = 400
		errString = errGet.Error()
		return
	}
	// Decode the args.
	args := reflect.New(methodSpec.argsType)
	if errRead := codecReq.ReadRequest(args.Interface()); errRead != nil {
		errCode = 400
		errString = errRead.Error()
		return
	}

	// Call the service method.
	reply = reflect.New(methodSpec.replyType)

	// omit the HTTP request if the service method doesn't accept it
	if serviceSpec.passReq {
		errValue = methodSpec.method.Func.Call([]reflect.Value{
			serviceSpec.rcvr,
			reflect.ValueOf(r),
			args,
			reply,
		})
	} else {
		errValue = methodSpec.method.Func.Call([]reflect.Value{
			serviceSpec.rcvr,
			args,
			reply,
		})
	}

	return
}

func (s *Server) NewCodec(r *http.Request) Codec {
	contentType := r.Header.Get("Content-Type")
	idx := strings.Index(contentType, ";")
	if idx != -1 {
		contentType = contentType[:idx]
	}
	var codec Codec
	if contentType == "" && len(s.codecs) == 1 {
		// If Content-Type is not set and only one codec has been registered,
		// then default to that codec.
		for _, c := range s.codecs {
			codec = c
		}
	} else {
		codec = s.codecs[strings.ToLower(contentType)]
	}

	return codec
}

// ServeHTTP
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		s.writeError(w, 405, "rpc: POST method required, received "+r.Method)
		return
	}

	codec := s.NewCodec(r)
	if codec == nil {
		s.writeError(w, 415, "rpc: unrecognized Content-Type: "+r.Header.Get("Content-Type"))
		return
	}

	// Create a new codec request.
	rawxml, err := ioutil.ReadAll(r.Body)
	defer r.Body.Close()
	codecReq := codec.NewRequest(rawxml, err)

	///////////////////////////
	// before intercept func
	///////////////////////////

	// Get service method to be called.
	method, errMethod := codecReq.Method()
	if errMethod != nil {
		s.writeError(w, 400, errMethod.Error())
		return
	}

	// Call the registered Intercept Function
	if s.interceptFunc != nil {
		req := s.interceptFunc(&RequestInfo{
			Request: r,
			Method:  method,
		})
		if req != nil {
			r = req
		}
	}
	// Call the registered Before Function
	if s.beforeFunc != nil {
		s.beforeFunc(&RequestInfo{
			Request: r,
			Method:  method,
		})
	}
	errCode, errStr, errValue, reply := s.Serve(r, codecReq)
	if errCode != 0 || len(errStr) != 0 {
		s.writeError(w, errCode, errStr)
		return
	}

	// Cast the result to error if needed.
	var errResult error
	errInter := errValue[0].Interface()
	if errInter != nil {
		errResult = errInter.(error)
	}

	// Prevents Internet Explorer from MIME-sniffing a response away
	// from the declared content-type
	w.Header().Set("x-content-type-options", "nosniff")
	// Encode the response.
	if errWrite := codecReq.WriteResponse(w, reply.Interface(), errResult); errWrite != nil {
		s.writeError(w, 400, errWrite.Error())
	} else {
		// Call the registered After Function
		if s.afterFunc != nil {
			s.afterFunc(&RequestInfo{
				Request:    r,
				Method:     method,
				Error:      errResult,
				StatusCode: 200,
			})
		}
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, msg)
	if s.afterFunc != nil {
		s.afterFunc(&RequestInfo{
			Error:      fmt.Errorf(msg),
			StatusCode: status,
		})
	}
}
