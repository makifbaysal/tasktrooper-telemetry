package worker

import (
	"github.com/gorilla/mux"
	"github.com/hibiken/asynq"
	"github.com/robfig/cron/v3"
)

func Run() {
	m := asynq.NewServeMux()
	m.HandleFunc("email:send", sendEmail)
	c := cron.New()
	c.AddFunc("*/15 * * * *", syncInvoices)
	r := mux.NewRouter()
	r.HandleFunc("/hooks/github", sendEmail).Methods("POST")
	api := r.PathPrefix("/internal").Subrouter()
	api.HandleFunc("/jobs/{id}", sendEmail).Methods("GET", "DELETE")
}

func sendEmail() {}
func syncInvoices() {}
