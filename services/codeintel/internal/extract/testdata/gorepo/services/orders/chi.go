package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func router() http.Handler {
	r := chi.NewRouter()
	r.Get("/v2/carts/{cartID}", getCart)
	r.Route("/v2/admin", func(r chi.Router) {
		r.Post("/refunds", createRefund)
		r.Route("/users", func(r chi.Router) {
			r.Delete("/{userID}", deleteUser)
		})
	})
	r.Group(func(g chi.Router) {
		g.Put("/v2/profile", updateProfile)
	})
	r.Mount("/v2/reports", reportsRouter())
	r.Method("PATCH", "/v2/carts/{cartID}", http.HandlerFunc(getCart))
	return r
}

func reportsRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/daily", getCart)
	return r
}

func getCart(w http.ResponseWriter, r *http.Request)       {}
func createRefund(w http.ResponseWriter, r *http.Request)  {}
func deleteUser(w http.ResponseWriter, r *http.Request)    {}
func updateProfile(w http.ResponseWriter, r *http.Request) {}
