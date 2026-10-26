// Package trooperhttp instruments net/http servers and clients for TaskTrooper Telemetry.
//
// Server (Go 1.22+ ServeMux patterns become trooper.route):
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("GET /users/{id}", getUser)
//	http.ListenAndServe(":8080", trooperhttp.Handler(mux))
//
// Client:
//
//	client := &http.Client{Transport: trooperhttp.NewTransport(nil, trooperhttp.WithPeerService("billing"))}
//
// No headers, query strings or bodies are recorded.
package trooperhttp
